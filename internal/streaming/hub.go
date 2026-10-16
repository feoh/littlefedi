// Package streaming implements the Mastodon-compatible streaming API
// (/api/v1/streaming) as an in-process publish/subscribe hub plus WebSocket and
// Server-Sent-Events transports.
//
// Design goals:
//   - Zero cost when no client is connected (a Pi must not pay for an idle
//     feature) and no new mandatory runtime dependency - fan-out is in-process.
//   - Bounded memory: every connection has a fixed-size send buffer; a client
//     that cannot keep up is dropped rather than allowed to grow the buffer.
//   - One fan-out point: producers (app.Service for local actions,
//     federation.InboxProcessor for inbound) publish through a single Publisher,
//     so every front end benefits.
//
// Cross-process fan-out (streaming.backend = "postgres") replaces the transport
// between producers and the hub without touching producers: they only depend on
// the Publisher interface.
package streaming

import (
	"encoding/json"
	"strings"
	"sync"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// Stream identifiers, matching Mastodon's stream names.
const (
	StreamUser         = "user"
	StreamNotification = "user:notification"
	StreamPublic       = "public"
	StreamPublicLocal  = "public:local"
	StreamPublicRemote = "public:remote"
	StreamHashtag      = "hashtag"
	StreamHashtagLocal = "hashtag:local"
	StreamList         = "list"
	StreamDirect       = "direct"
)

// envelope is a fully-rendered message queued to a single connection. wireStreams
// is the JSON "stream" array sent to the client (e.g. ["hashtag","catpics"]).
type envelope struct {
	wireStreams []string
	event       string
	payload     string
	wsBytes     []byte // immutable, shared by every WebSocket recipient
	// notificationType is set on notification events so that API clients,
	// which only understand Mastodon's types, can be skipped.
	notificationType string
}

func makeEnvelope(wireStreams []string, event, payload string) envelope {
	env := envelope{wireStreams: wireStreams, event: event, payload: payload}
	env.wsBytes, _ = json.Marshal(wireMessage{Stream: wireStreams, Event: event, Payload: payload})
	return env
}

// Hub is the in-process pub/sub registry. Broadcast streams (public*, hashtag*,
// list*) are indexed by stream key; per-account streams (user, direct) are
// indexed by account id so a targeted event only visits that account's own
// connections, not every connection on the instance.
type Hub struct {
	mu        sync.RWMutex
	byStream  map[string]map[*Conn]struct{} // broadcast key -> conns
	byAccount map[string]map[*Conn]struct{} // accountID -> conns (any stream)
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{
		byStream:  make(map[string]map[*Conn]struct{}),
		byAccount: make(map[string]map[*Conn]struct{}),
	}
}

// isBroadcastKey reports whether a subscription key is delivered to every
// subscriber (public/hashtag/list) rather than to a single account (user/direct).
func isBroadcastKey(key string) bool {
	switch {
	case key == StreamPublic, key == StreamPublicLocal, key == StreamPublicRemote:
		return true
	case len(key) >= 7 && key[:7] == "hashtag":
		return true
	case len(key) >= 4 && key[:4] == "list":
		return true
	default:
		return false
	}
}

// register adds a connection to the account index (called once at connect).
func (h *Hub) register(c *Conn) {
	if c.accountID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.byAccount[c.accountID]
	if set == nil {
		set = make(map[*Conn]struct{})
		h.byAccount[c.accountID] = set
	}
	set[c] = struct{}{}
}

// unregister removes a connection from every index.
func (h *Hub) unregister(c *Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set := h.byAccount[c.accountID]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(h.byAccount, c.accountID)
		}
	}
	for key := range c.keys {
		if set := h.byStream[key]; set != nil {
			delete(set, c)
			if len(set) == 0 {
				delete(h.byStream, key)
			}
		}
	}
}

// subscribe adds key to the connection and indexes it for broadcast delivery.
func (h *Hub) subscribe(c *Conn, key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c.mu.Lock()
	c.keys[key] = true
	c.mu.Unlock()
	if isBroadcastKey(key) {
		set := h.byStream[key]
		if set == nil {
			set = make(map[*Conn]struct{})
			h.byStream[key] = set
		}
		set[c] = struct{}{}
	}
}

// unsubscribe removes key from the connection.
func (h *Hub) unsubscribe(c *Conn, key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c.mu.Lock()
	delete(c.keys, key)
	c.mu.Unlock()
	if set := h.byStream[key]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(h.byStream, key)
		}
	}
}

// broadcast delivers env to every connection subscribed to key.
func (h *Hub) broadcast(key string, env envelope) {
	h.mu.RLock()
	targets := make([]*Conn, 0, len(h.byStream[key]))
	for c := range h.byStream[key] {
		targets = append(targets, c)
	}
	h.mu.RUnlock()
	for _, c := range targets {
		c.enqueue(env)
	}
}

func (h *Hub) broadcastAnonymous(key string, env envelope) {
	h.mu.RLock()
	targets := make([]*Conn, 0, len(h.byStream[key]))
	for c := range h.byStream[key] {
		if c.accountID == "" {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range targets {
		c.enqueue(env)
	}
}

// sendToAccount delivers env to the given account's connections that are
// subscribed to key.
func (h *Hub) sendToAccount(accountID, key string, env envelope) {
	if accountID == "" {
		return
	}
	h.mu.RLock()
	var targets []*Conn
	for c := range h.byAccount[accountID] {
		if c.apiClient && env.notificationType != "" && !models.IsMastodonNotificationType(env.notificationType) {
			continue
		}
		c.mu.Lock()
		ok := c.keys[key]
		c.mu.Unlock()
		if ok {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range targets {
		c.enqueue(env)
	}
}

// HasAccountStream reports whether accountID has a live subscription to key.
// It only inspects that account's connections, unlike fan-out helpers that
// enumerate all recipients.
func (h *Hub) HasAccountStream(accountID, key string) bool {
	if accountID == "" {
		return false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.byAccount[accountID] {
		c.mu.Lock()
		ok := c.keys[key]
		c.mu.Unlock()
		if ok {
			return true
		}
	}
	return false
}

// AccountIDsForStream returns the distinct account ids that currently have at
// least one connection subscribed to the given stream key. Producers use this to
// fan a status out to only the home streams that are actually being watched,
// bounding work by live connections rather than by total followers.
func (h *Hub) AccountIDsForStream(key string) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []string
	for acct, conns := range h.byAccount {
		for c := range conns {
			c.mu.Lock()
			ok := c.keys[key]
			c.mu.Unlock()
			if ok {
				out = append(out, acct)
				break
			}
		}
	}
	return out
}

// BroadcastKeysWithPrefix returns the live broadcast stream keys that start
// with prefix (e.g. "list:"). Producers use it to skip all list resolution
// work unless a list stream is actually being watched. Bounded by the number
// of live broadcast subscriptions, and zero cost when none match.
func (h *Hub) BroadcastKeysWithPrefix(prefix string) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []string
	for key := range h.byStream {
		if strings.HasPrefix(key, prefix) {
			out = append(out, key)
		}
	}
	return out
}

// HasSubscribers reports whether anyone is connected at all. Producers can use
// this as a cheap gate to skip all routing work on an idle instance.
func (h *Hub) HasSubscribers() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.byAccount) > 0 || len(h.byStream) > 0
}

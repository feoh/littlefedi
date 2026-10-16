package streaming

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// AuthStore is the minimal storage surface needed to authenticate a streaming
// connection from an OAuth bearer token.
type AuthStore interface {
	GetOAuthTokenByAccess(ctx context.Context, accessToken string) (*models.OAuthToken, error)
	GetAccount(ctx context.Context, id string) (*models.Account, error)
}

type sessionAuthStore interface {
	GetSessionByTokenHash(context.Context, string) (*models.Session, error)
}

// listAuthStore verifies list ownership before a client may subscribe to a
// list stream. A list timeline is private, so subscribing to someone else's
// list must be rejected - and the check fails closed if the store cannot
// resolve lists.
type listAuthStore interface {
	GetList(ctx context.Context, id string) (*models.List, error)
}

// Handler serves the Mastodon-compatible streaming endpoints over WebSocket and
// Server-Sent Events, backed by a Hub.
type Handler struct {
	hub             *Hub
	auth            AuthStore
	requireEmail    bool
	requireAdminMFA bool
}

// NewHandler builds a streaming HTTP handler over hub.
func NewHandler(hub *Hub, auth AuthStore, requireEmail ...bool) *Handler {
	h := &Handler{hub: hub, auth: auth}
	if len(requireEmail) > 0 {
		h.requireEmail = requireEmail[0]
	}
	if len(requireEmail) > 1 {
		h.requireAdminMFA = requireEmail[1]
	}
	return h
}

// RegisterRoutes mounts the streaming endpoints. They must sit outside any
// request-timeout middleware; the handlers detach from the request deadline
// themselves (see serveWebSocket / serveSSE) so a 60s server timeout does not
// sever a long-lived stream.
func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Get("/api/v1/streaming", h.handleMultiplex)
	r.Get("/api/v1/streaming/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("OK"))
	})

	// Per-stream endpoints (WebSocket if upgraded, otherwise SSE).
	r.Get("/api/v1/streaming/user", h.streamHandler("user"))
	r.Get("/api/v1/streaming/user/notification", h.streamHandler("user:notification"))
	r.Get("/api/v1/streaming/public", h.streamHandler("public"))
	r.Get("/api/v1/streaming/public/local", h.streamHandler("public:local"))
	r.Get("/api/v1/streaming/public/remote", h.streamHandler("public:remote"))
	r.Get("/api/v1/streaming/hashtag", h.streamHandler("hashtag"))
	r.Get("/api/v1/streaming/hashtag/local", h.streamHandler("hashtag:local"))
	r.Get("/api/v1/streaming/direct", h.streamHandler("direct"))
	r.Get("/api/v1/streaming/list", h.streamHandler("list"))
}

// streamDesc describes a resolved subscription: the internal routing key, the
// "stream" array echoed to the client, and whether an authenticated user is
// required.
type streamDesc struct {
	key          string
	wire         []string
	requiresUser bool
}

// descriptorFor resolves a Mastodon stream name (plus tag/list query params) into
// a streamDesc. ok is false for an unknown stream.
func descriptorFor(stream string, r *http.Request) (streamDesc, bool) {
	switch stream {
	case "user":
		return streamDesc{key: StreamUser, wire: []string{StreamUser}, requiresUser: true}, true
	case "user:notification":
		// Notifications are delivered on the user stream; subscribing here is
		// equivalent.
		return streamDesc{key: StreamUser, wire: []string{StreamUser}, requiresUser: true}, true
	case "public":
		return streamDesc{key: StreamPublic, wire: []string{StreamPublic}}, true
	case "public:local":
		return streamDesc{key: StreamPublicLocal, wire: []string{StreamPublicLocal}}, true
	case "public:remote":
		// Remote-only is not split out; treat as the federated public stream.
		return streamDesc{key: StreamPublic, wire: []string{StreamPublic}}, true
	case "hashtag", "hashtag:local":
		tag := strings.TrimSpace(r.URL.Query().Get("tag"))
		if tag == "" {
			return streamDesc{}, false
		}
		return streamDesc{key: StreamHashtag + ":" + tag, wire: []string{StreamHashtag, tag}}, true
	case "direct":
		return streamDesc{key: StreamDirect, wire: []string{StreamDirect}, requiresUser: true}, true
	case "list":
		list := strings.TrimSpace(r.URL.Query().Get("list"))
		if list == "" {
			return streamDesc{}, false
		}
		return streamDesc{key: StreamList + ":" + list, wire: []string{StreamList, list}, requiresUser: true}, true
	default:
		return streamDesc{}, false
	}
}

// streamHandler returns the HTTP handler for a fixed per-stream endpoint.
func (h *Handler) streamHandler(stream string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		desc, ok := descriptorFor(stream, r)
		if !ok {
			http.Error(w, "unknown or incomplete stream", http.StatusBadRequest)
			return
		}
		account := h.authenticate(r)
		if desc.requiresUser && account == nil {
			http.Error(w, "missing or invalid access token", http.StatusUnauthorized)
			return
		}
		if !h.canSubscribe(r.Context(), account, desc.key) {
			http.Error(w, "Record not found", http.StatusNotFound)
			return
		}
		if isWebSocketUpgrade(r) {
			h.serveWebSocket(w, r, account, []streamDesc{desc})
			return
		}
		h.serveSSE(w, r, account, desc)
	}
}

// handleMultiplex serves the multiplexed WebSocket endpoint. An initial stream
// may be supplied via the ?stream= query; further streams are added with
// subscribe messages.
func (h *Handler) handleMultiplex(w http.ResponseWriter, r *http.Request) {
	if !isWebSocketUpgrade(r) {
		http.Error(w, "websocket upgrade required", http.StatusUpgradeRequired)
		return
	}
	account := h.authenticate(r)
	var initial []streamDesc
	if stream := strings.TrimSpace(r.URL.Query().Get("stream")); stream != "" {
		if desc, ok := descriptorFor(stream, r); ok && (!desc.requiresUser || account != nil) && h.canSubscribe(r.Context(), account, desc.key) {
			initial = append(initial, desc)
		}
	}
	h.serveWebSocket(w, r, account, initial)
}

// authenticate resolves the bearer token from the Authorization header or the
// access_token query parameter (browsers cannot set WS headers), returning the
// account or nil for an anonymous connection.
// requestToken returns the OAuth access token a streaming request carries,
// from the Authorization header or the access_token parameter.
func requestToken(r *http.Request) string {
	token := ""
	if auth := r.Header.Get("Authorization"); auth != "" {
		token = strings.TrimPrefix(auth, "Bearer ")
		if token == auth {
			token = strings.TrimPrefix(auth, "token ")
		}
	}
	if token == "" {
		token = r.URL.Query().Get("access_token")
	}
	return token
}

func (h *Handler) authenticate(r *http.Request) *models.Account {
	token := requestToken(r)
	if token == "" {
		return h.authenticateSession(r)
	}
	oauthToken, err := h.auth.GetOAuthTokenByAccess(r.Context(), token)
	if err != nil || oauthToken == nil {
		return nil
	}
	if oauthToken.AuthorizationCode || oauthToken.RevokedAt != nil || !hasReadScope(oauthToken.Scopes) {
		return nil
	}
	if oauthToken.ExpiresAt != nil && time.Now().After(*oauthToken.ExpiresAt) {
		return nil
	}
	account, err := h.auth.GetAccount(r.Context(), oauthToken.AccountID)
	if err != nil || !account.CanAuthenticate(h.requireEmail, h.requireAdminMFA) {
		return nil
	}
	return account
}

func (h *Handler) authenticateSession(r *http.Request) *models.Account {
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host) {
			return nil
		}
	}
	sessions, ok := h.auth.(sessionAuthStore)
	if !ok {
		return nil
	}
	cookie, err := r.Cookie("session")
	if err != nil || cookie.Value == "" {
		return nil
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(cookie.Value)))
	session, err := sessions.GetSessionByTokenHash(r.Context(), hash)
	if err != nil || (!session.ExpiresAt.IsZero() && time.Now().After(session.ExpiresAt)) {
		return nil
	}
	account, err := h.auth.GetAccount(r.Context(), session.AccountID)
	if err != nil || !account.CanAuthenticate(h.requireEmail, h.requireAdminMFA) {
		return nil
	}
	return account
}

func hasReadScope(scopes string) bool {
	for _, scope := range strings.Fields(scopes) {
		// A user stream includes both statuses and notifications.
		if scope == "read" {
			return true
		}
	}
	return false
}

// canSubscribe reports whether account may subscribe to desc. Non-list streams
// are governed by desc.requiresUser alone; list streams additionally require
// that account owns the list. Fails closed: an unauthenticated user, or a store
// that cannot resolve lists, is denied.
func (h *Handler) canSubscribe(ctx context.Context, account *models.Account, key string) bool {
	listID, ok := listStreamID(key)
	if !ok {
		return true
	}
	if account == nil {
		return false
	}
	lister, ok := h.auth.(listAuthStore)
	if !ok {
		return false // fail closed rather than leak a private timeline
	}
	list, err := lister.GetList(ctx, listID)
	if err != nil || list == nil {
		return false
	}
	return list.AccountID == account.ID
}

// listStreamID extracts the list id from a "list:<id>" routing key.
func listStreamID(key string) (string, bool) {
	prefix := StreamList + ":"
	if strings.HasPrefix(key, prefix) {
		return key[len(prefix):], true
	}
	return "", false
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func accountID(a *models.Account) string {
	if a == nil {
		return ""
	}
	return a.ID
}

// serveWebSocket upgrades the request and runs the read/write pumps. It detaches
// from the request context so the global request timeout cannot sever the
// long-lived connection.
func (h *Handler) serveWebSocket(w http.ResponseWriter, r *http.Request, account *models.Account, initial []streamDesc) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// CORS is handled by the global middleware; the API is token-authenticated.
		InsecureSkipVerify: true,
	})
	if err != nil {
		slog.Debug("streaming: websocket accept failed", "error", err)
		return
	}
	ws.SetReadLimit(64 * 1024)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn := newConn(h.hub, accountID(account))
	conn.apiClient = account != nil && requestToken(r) != ""
	h.hub.register(conn)
	defer func() {
		conn.Close()
		ws.Close(websocket.StatusNormalClosure, "")
	}()
	for _, d := range initial {
		conn.Subscribe(d.key)
	}

	go h.wsWritePump(ctx, ws, conn, r, account)
	h.wsReadPump(ctx, ws, conn, account)
}

// wsWritePump drains the connection's queue to the socket and sends periodic
// pings to keep the connection alive and detect dead peers.
func (h *Handler) wsWritePump(ctx context.Context, ws *websocket.Conn, conn *Conn, r *http.Request, account *models.Account) {
	defer ws.CloseNow()
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-conn.Done():
			return
		case env := <-conn.Out():
			if !h.streamAuthorized(r, account) {
				return
			}
			data := env.wsBytes
			if len(data) == 0 {
				data, _ = json.Marshal(wireMessage{Stream: env.wireStreams, Event: env.event, Payload: env.payload})
			}
			writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := ws.Write(writeCtx, websocket.MessageText, data)
			cancel()
			if err != nil {
				conn.Close()
				return
			}
		case <-ping.C:
			if !h.streamAuthorized(r, account) {
				return
			}
			pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := ws.Ping(pingCtx)
			cancel()
			if err != nil {
				conn.Close()
				return
			}
		}
	}
}

// wsReadPump processes client subscribe/unsubscribe messages and terminates the
// connection when the socket closes.
func (h *Handler) wsReadPump(ctx context.Context, ws *websocket.Conn, conn *Conn, account *models.Account) {
	defer conn.Close()
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var msg clientMessage
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		desc, ok := descriptorForClientMessage(msg)
		if !ok {
			continue
		}
		if desc.requiresUser && account == nil {
			continue
		}
		switch strings.ToLower(msg.Type) {
		case "subscribe":
			// List streams are private: verify ownership before subscribing.
			if !h.canSubscribe(ctx, account, desc.key) {
				continue
			}
			conn.Subscribe(desc.key)
		case "unsubscribe":
			conn.Unsubscribe(desc.key)
		}
	}
}

// serveSSE streams a single subscription as Server-Sent Events. It clears the
// connection write deadline so the global server WriteTimeout does not cut the
// stream, and relies on write errors (and a heartbeat) to detect disconnects.
func (h *Handler) serveSSE(w http.ResponseWriter, r *http.Request, account *models.Account, desc streamDesc) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if rc := http.NewResponseController(w); rc != nil {
		_ = rc.SetWriteDeadline(time.Time{})
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	conn := newConn(h.hub, accountID(account))
	conn.apiClient = account != nil && requestToken(r) != ""
	h.hub.register(conn)
	defer conn.Close()
	conn.Subscribe(desc.key)

	// Note: r.Context() is not used for liveness - a request-timeout middleware
	// would cancel it on a fixed deadline and sever the stream. Client
	// disconnects surface as write errors below (the periodic heartbeat
	// guarantees a write at least every 15s), and conn.Done() covers hub-side
	// teardown (e.g. a slow-consumer drop) and server shutdown.
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-conn.Done():
			return
		case env := <-conn.Out():
			if !h.streamAuthorized(r, account) {
				return
			}
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", env.event, env.payload); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if !h.streamAuthorized(r, account) {
				return
			}
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := fmt.Fprint(w, ":thump\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (h *Handler) streamAuthorized(r *http.Request, account *models.Account) bool {
	if account == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	current := h.authenticate(r.WithContext(ctx))
	return current != nil && current.ID == account.ID
}

// wireMessage is the JSON envelope sent to clients, matching Mastodon's shape:
// {"stream":["user"],"event":"update","payload":"<json string>"}.
type wireMessage struct {
	Stream  []string `json:"stream"`
	Event   string   `json:"event"`
	Payload string   `json:"payload"`
}

// clientMessage is a subscribe/unsubscribe control message from the client.
type clientMessage struct {
	Type   string `json:"type"`
	Stream string `json:"stream"`
	Tag    string `json:"tag"`
	List   string `json:"list"`
}

// descriptorForClientMessage resolves a control message's stream (with its tag /
// list) into a streamDesc.
func descriptorForClientMessage(msg clientMessage) (streamDesc, bool) {
	switch msg.Stream {
	case "user", "user:notification":
		return streamDesc{key: StreamUser, requiresUser: true}, true
	case "public":
		return streamDesc{key: StreamPublic}, true
	case "public:local":
		return streamDesc{key: StreamPublicLocal}, true
	case "public:remote":
		return streamDesc{key: StreamPublic}, true
	case "hashtag", "hashtag:local":
		if msg.Tag == "" {
			return streamDesc{}, false
		}
		return streamDesc{key: StreamHashtag + ":" + msg.Tag}, true
	case "direct":
		return streamDesc{key: StreamDirect, requiresUser: true}, true
	case "list":
		if msg.List == "" {
			return streamDesc{}, false
		}
		return streamDesc{key: StreamList + ":" + msg.List, requiresUser: true}, true
	default:
		return streamDesc{}, false
	}
}

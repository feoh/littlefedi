package streaming

import (
	"context"
	"log/slog"
	"sync"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// Publisher is the producer-facing fan-out interface. app.Service (local
// actions) and federation.InboxProcessor (inbound activities) depend only on
// this, so the streaming transport can change without touching them. All methods
// must be safe to call on a nil Publisher and safe for concurrent use.
type Publisher interface {
	// StatusCreated streams a newly created status to the relevant timelines.
	StatusCreated(ctx context.Context, status *models.Status)
	// StatusUpdated streams an edit of an existing status (event "status.update").
	StatusUpdated(ctx context.Context, status *models.Status)
	// StatusDeleted streams a delete (event "delete", payload is the status id).
	StatusDeleted(ctx context.Context, status *models.Status)
	// NotificationCreated streams a notification to its recipient's user stream.
	NotificationCreated(ctx context.Context, n *models.Notification)
	// CacheInvalidated propagates non-status membership/profile mutations to
	// other application processes. It has no client-visible stream payload.
	CacheInvalidated(ctx context.Context)
}

// Serializer renders domain objects to the Mastodon JSON the client expects. It
// is implemented by the API layer (which owns the entity shapes) and injected
// from main.go, keeping the streaming package free of an api import cycle.
type Serializer interface {
	SerializeStatus(viewer *models.Account, status *models.Status) ([]byte, error)
	SerializeNotification(viewer *models.Account, n *models.Notification) ([]byte, error)
}

type FilteredSerializer interface {
	SerializeFilteredStatus(context.Context, string, *models.Account, *models.Status) ([]byte, bool, error)
	SerializeFilteredNotification(context.Context, *models.Account, *models.Notification) ([]byte, bool, error)
}

// StatusHydrator populates relations omitted by a compact cross-process broker
// event before the status is serialized.
type StatusHydrator interface {
	HydrateStreamingStatus(context.Context, *models.Status)
}

// RouterStore is the minimal storage surface the router needs to resolve home
// fan-out and direct-message recipients.
type RouterStore interface {
	IsFollowing(ctx context.Context, accountID, targetID string) (bool, error)
	GetAccount(ctx context.Context, id string) (*models.Account, error)
	GetMentionsForStatus(ctx context.Context, statusID string) ([]*models.Mention, error)
}

type brokerEventStore interface {
	GetStatus(ctx context.Context, id string) (*models.Status, error)
	GetNotification(ctx context.Context, id string) (*models.Notification, error)
}

// listRouterStore is the optional storage surface for list fan-out. When the
// wired store implements it, publishStatus delivers to list:<id> streams and
// suppresses exclusive-list members from their owners' home streams.
type listRouterStore interface {
	ListsContainingAccount(ctx context.Context, memberID string) ([]*models.List, error)
	GetStatus(ctx context.Context, id string) (*models.Status, error)
	IsFollowing(ctx context.Context, accountID, targetID string) (bool, error)
}

// EventRouter turns domain events into hub deliveries. It is the single fan-out
// point: it decides which streams a status belongs to and, for home/direct
// streams, which connected accounts should receive it.
type EventRouter struct {
	hub   *Hub
	store RouterStore

	mu         sync.RWMutex
	serializer Serializer
	hydrator   StatusHydrator
}

// NewEventRouter builds a router over the given hub. The serializer may be nil
// at construction and set later with SetSerializer (the API handler that
// implements it is created after the router during startup); until then the
// router's methods are safe no-ops. A nil hub or store also yields a no-op
// router, so wiring can be unconditional.
func NewEventRouter(hub *Hub, store RouterStore, serializer Serializer) *EventRouter {
	return &EventRouter{hub: hub, store: store, serializer: serializer}
}

// SetSerializer wires (or replaces) the serializer in a race-free way, so it can
// be supplied after the router has already been handed to producers.
func (r *EventRouter) SetSerializer(s Serializer) {
	r.mu.Lock()
	r.serializer = s
	r.mu.Unlock()
}

func (r *EventRouter) SetHydrator(h StatusHydrator) {
	r.mu.Lock()
	r.hydrator = h
	r.mu.Unlock()
}

func (r *EventRouter) getSerializer() Serializer {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.serializer
}

func (r *EventRouter) active() bool {
	return r != nil && r.hub != nil && r.store != nil && r.getSerializer() != nil
}

// HandleBrokerEvent resolves compact cross-process references and routes them
// through the same logic used for local events.
func (r *EventRouter) HandleBrokerEvent(ctx context.Context, event BrokerEvent) {
	loader, _ := r.store.(brokerEventStore)
	switch event.Kind {
	case "status.created", "status.updated":
		status := event.status
		remote := status == nil
		if remote && loader != nil {
			status, _ = loader.GetStatus(ctx, event.ID)
		}
		if remote && status != nil {
			r.mu.RLock()
			hydrator := r.hydrator
			r.mu.RUnlock()
			if hydrator != nil {
				hydrator.HydrateStreamingStatus(ctx, status)
			}
		}
		if event.Kind == "status.created" {
			r.StatusCreated(ctx, status)
		} else {
			r.StatusUpdated(ctx, status)
		}
	case "status.deleted":
		status := event.status
		if status == nil {
			status = &models.Status{ID: event.ID, AccountID: event.AccountID, Visibility: event.Visibility}
		}
		r.StatusDeleted(ctx, status)
	case "notification.created":
		n := event.notification
		if n == nil && loader != nil {
			n, _ = loader.GetNotification(ctx, event.ID)
		}
		r.NotificationCreated(ctx, n)
	}
}

// StatusCreated implements Publisher.
func (r *EventRouter) StatusCreated(ctx context.Context, status *models.Status) {
	r.publishStatus(ctx, status, "update")
}

// StatusUpdated implements Publisher.
func (r *EventRouter) StatusUpdated(ctx context.Context, status *models.Status) {
	r.publishStatus(ctx, status, "status.update")
}

// StatusDeleted implements Publisher.
func (r *EventRouter) StatusDeleted(ctx context.Context, status *models.Status) {
	if !r.active() || status == nil {
		return
	}
	// Skip entirely when nobody is connected (the common, idle case).
	if !r.hub.HasSubscribers() {
		return
	}
	if status.Account == nil && status.AccountID != "" {
		status.Account, _ = r.store.GetAccount(ctx, status.AccountID)
	}
	// Broadcast deletes to public/hashtag streams; targeted to home/direct.
	if status.Visibility == models.VisibilityPublic {
		r.broadcastDelete(StreamPublic, "delete", status.ID)
		if isLocal(status.Account) {
			r.broadcastDelete(StreamPublicLocal, "delete", status.ID)
		}
	}
	homeDelete := makeEnvelope([]string{StreamUser}, "delete", status.ID)
	for _, acct := range r.homeRecipients(ctx, status) {
		r.hub.sendToAccount(acct, StreamUser, homeDelete)
	}
}

// NotificationCreated implements Publisher.
func (r *EventRouter) NotificationCreated(ctx context.Context, n *models.Notification) {
	if !r.active() || n == nil || n.AccountID == "" {
		return
	}
	// Notifications only matter to their recipient - skip if they're not connected.
	if !r.hub.HasAccountStream(n.AccountID, StreamUser) {
		return
	}
	target, err := r.store.GetAccount(ctx, n.AccountID)
	if err != nil || target == nil {
		return
	}
	payload, ok, err := r.serializeNotification(ctx, target, n)
	if err != nil {
		slog.Warn("streaming: serialize notification failed", "error", err)
		return
	}
	if !ok {
		return
	}
	env := makeEnvelope([]string{StreamUser}, "notification", string(payload))
	r.hub.sendToAccount(target.ID, StreamUser, env)
}

// publishStatus routes a created/edited status to every stream it belongs to.
func (r *EventRouter) publishStatus(ctx context.Context, status *models.Status, event string) {
	if !r.active() || status == nil {
		return
	}
	if !r.hub.HasSubscribers() {
		return
	}
	if status.Account == nil && status.AccountID != "" {
		status.Account, _ = r.store.GetAccount(ctx, status.AccountID)
	}
	switch status.Visibility {
	case models.VisibilityDirect:
		for _, acct := range r.directRecipients(ctx, status) {
			r.sendStatusToAccount(ctx, acct, StreamDirect, []string{StreamDirect}, "home", event, status)
			r.sendStatusToAccount(ctx, acct, StreamUser, []string{StreamUser}, "home", event, status)
		}
		return
	case models.VisibilityPublic:
		r.broadcastStatus(ctx, StreamPublic, []string{StreamPublic}, "public", event, status)
		// Boosts are absent from the stored local timeline, so streaming one
		// would show a post that vanishes on the next reload.
		if isLocal(status.Account) && status.BoostOfID == nil {
			r.broadcastStatus(ctx, StreamPublicLocal, []string{StreamPublicLocal}, "public", event, status)
		}
		r.publishHashtags(ctx, status, event)
	case models.VisibilityUnlisted:
		// Unlisted is excluded from public streams but included in hashtag streams.
		r.publishHashtags(ctx, status, event)
	}

	// Resolve list memberships once: they drive both list-stream delivery and
	// exclusive-list home suppression. Direct visibility already returned above.
	recipients := r.homeRecipients(ctx, status)
	authorLists, exclusiveOwners := r.resolveAuthorLists(ctx, status, len(recipients) > 0)

	// Home (user) stream: the author plus connected followers, minus any owner
	// whose exclusive list contains the author (exclusive lists hide their
	// members from home).
	for _, acct := range recipients {
		if acct != status.Account.ID && exclusiveOwners[acct] {
			continue
		}
		r.sendStatusToAccount(ctx, acct, StreamUser, []string{StreamUser}, "home", event, status)
	}

	// List streams: deliver to each live list:<id> whose members include the
	// author, subject to the list's replies_policy.
	r.publishLists(ctx, status, authorLists, event)
}

// resolveAuthorLists returns the lists (any owner) that contain the status
// author, plus the set of owners whose *exclusive* list contains them. The
// query is skipped entirely (the common idle case) when no list stream is
// live and no home recipient could be suppressed.
func (r *EventRouter) resolveAuthorLists(ctx context.Context, status *models.Status, haveHomeRecipients bool) ([]*models.List, map[string]bool) {
	lister, ok := r.store.(listRouterStore)
	if !ok || status.Account == nil {
		return nil, nil
	}
	liveListStreams := len(r.hub.BroadcastKeysWithPrefix(StreamList+":")) > 0
	if !liveListStreams && !haveHomeRecipients {
		return nil, nil
	}
	lists, err := lister.ListsContainingAccount(ctx, status.Account.ID)
	if err != nil || len(lists) == 0 {
		return nil, nil
	}
	exclusiveOwners := make(map[string]bool)
	for _, l := range lists {
		if l.Exclusive {
			exclusiveOwners[l.AccountID] = true
		}
	}
	return lists, exclusiveOwners
}

// publishLists delivers status to every live list stream whose members include
// the author, honouring each list's replies_policy.
func (r *EventRouter) publishLists(ctx context.Context, status *models.Status, authorLists []*models.List, event string) {
	if len(authorLists) == 0 {
		return
	}
	live := r.hub.BroadcastKeysWithPrefix(StreamList + ":")
	if len(live) == 0 {
		return
	}
	liveSet := make(map[string]bool, len(live))
	for _, k := range live {
		liveSet[k] = true
	}
	// Cache of "lists containing the parent's author", fetched at most once.
	var parentLists []*models.List
	parentFetched := false
	for _, l := range authorLists {
		key := StreamList + ":" + l.ID
		if !liveSet[key] {
			continue
		}
		if !r.listAllowsReply(ctx, l, status, &parentLists, &parentFetched) {
			continue
		}
		r.broadcastStatus(ctx, key, []string{StreamList, l.ID}, "home", event, status)
	}
}

// listAllowsReply applies a list's replies_policy to a status. Non-replies and
// self-replies always pass; a reply whose parent is not stored locally is
// always filtered (matching the list timeline SQL and Mastodon).
func (r *EventRouter) listAllowsReply(ctx context.Context, list *models.List, status *models.Status, parentLists *[]*models.List, parentFetched *bool) bool {
	isReply := status.InReplyToID != nil ||
		(status.InReplyToURI != nil && *status.InReplyToURI != "")
	if !isReply {
		return true
	}
	lister, ok := r.store.(listRouterStore)
	if !ok || status.InReplyToID == nil {
		return false // reply to an unknown/unfetched parent
	}
	parent, err := lister.GetStatus(ctx, *status.InReplyToID)
	if err != nil || parent == nil {
		return false
	}
	if parent.AccountID == status.AccountID {
		return true // self-reply
	}
	switch list.RepliesPolicy {
	case models.ListRepliesNone:
		return false
	case models.ListRepliesFollowed:
		following, err := lister.IsFollowing(ctx, list.AccountID, parent.AccountID)
		return err == nil && following
	default: // ListRepliesList
		if !*parentFetched {
			*parentLists, _ = lister.ListsContainingAccount(ctx, parent.AccountID)
			*parentFetched = true
		}
		for _, pl := range *parentLists {
			if pl.ID == list.ID {
				return true
			}
		}
		return false
	}
}

func (r *EventRouter) publishHashtags(ctx context.Context, status *models.Status, event string) {
	for _, tag := range status.Tags {
		if tag.Name == "" {
			continue
		}
		key := StreamHashtag + ":" + tag.Name
		r.broadcastStatus(ctx, key, []string{StreamHashtag, tag.Name}, "public", event, status)
	}
}

func (r *EventRouter) serializeStatus(ctx context.Context, scope string, viewer *models.Account, status *models.Status) ([]byte, bool, error) {
	serializer := r.getSerializer()
	if filtered, ok := serializer.(FilteredSerializer); ok {
		return filtered.SerializeFilteredStatus(ctx, scope, viewer, status)
	}
	payload, err := serializer.SerializeStatus(viewer, status)
	return payload, err == nil, err
}

func (r *EventRouter) serializeNotification(ctx context.Context, viewer *models.Account, notification *models.Notification) ([]byte, bool, error) {
	serializer := r.getSerializer()
	if filtered, ok := serializer.(FilteredSerializer); ok {
		return filtered.SerializeFilteredNotification(ctx, viewer, notification)
	}
	payload, err := serializer.SerializeNotification(viewer, notification)
	return payload, err == nil, err
}

func (r *EventRouter) sendStatusToAccount(ctx context.Context, accountID, key string, streams []string, scope, event string, status *models.Status) {
	viewer, err := r.store.GetAccount(ctx, accountID)
	if err != nil {
		return
	}
	payload, keep, err := r.serializeStatus(ctx, scope, viewer, status)
	if err != nil || !keep {
		return
	}
	r.hub.sendToAccount(accountID, key, makeEnvelope(streams, event, string(payload)))
}

func (r *EventRouter) broadcastStatus(ctx context.Context, key string, streams []string, scope, event string, status *models.Status) {
	if payload, keep, err := r.serializeStatus(ctx, scope, nil, status); err == nil && keep {
		r.hub.broadcastAnonymous(key, makeEnvelope(streams, event, string(payload)))
	}
	for _, accountID := range r.hub.AccountIDsForStream(key) {
		r.sendStatusToAccount(ctx, accountID, key, streams, scope, event, status)
	}
}

// homeRecipients returns the connected account ids whose home (user) stream this
// status belongs to: the author plus the subset of connected user-stream
// accounts that follow the author. Work is bounded by live connections, not by
// the author's total follower count.
func (r *EventRouter) homeRecipients(ctx context.Context, status *models.Status) []string {
	if status.Account == nil {
		return nil
	}
	connected := r.hub.AccountIDsForStream(StreamUser)
	if len(connected) == 0 {
		return nil
	}
	viewers := status.HomeFeedViewers
	if viewers == nil {
		if store, ok := r.store.(interface {
			HomeTimelineViewers(context.Context, *models.Status) ([]string, error)
		}); ok {
			if loaded, err := store.HomeTimelineViewers(ctx, status); err == nil {
				if loaded == nil {
					loaded = []string{}
				}
				status.HomeFeedViewers = loaded
				viewers = loaded
			}
		}
	}
	if viewers != nil {
		connectedSet := make(map[string]bool, len(connected))
		for _, accountID := range connected {
			connectedSet[accountID] = true
		}
		out := make([]string, 0, min(len(viewers), len(connected)))
		for _, accountID := range viewers {
			if connectedSet[accountID] {
				out = append(out, accountID)
			}
		}
		return out
	}
	// Compatibility fallback for minimal test stores. Production storage
	// implements HomeTimelineViewers, so it never takes this per-account path.
	seen := map[string]bool{}
	out := make([]string, 0, len(connected))
	for _, accountID := range connected {
		if accountID == status.Account.ID {
			seen[accountID] = true
			out = append(out, accountID)
			continue
		}
		following, err := r.store.IsFollowing(ctx, accountID, status.Account.ID)
		if err == nil && following && !seen[accountID] {
			seen[accountID] = true
			out = append(out, accountID)
		}
	}
	return out
}

// directRecipients returns the local participants of a direct status: the author
// plus any mentioned local accounts.
func (r *EventRouter) directRecipients(ctx context.Context, status *models.Status) []string {
	seen := map[string]bool{}
	var out []string
	if status.Account != nil && isLocal(status.Account) {
		seen[status.Account.ID] = true
		out = append(out, status.Account.ID)
	}
	mentions := status.Mentions
	if len(mentions) == 0 {
		loaded, _ := r.store.GetMentionsForStatus(ctx, status.ID)
		for _, m := range loaded {
			mentions = append(mentions, *m)
		}
	}
	for _, m := range mentions {
		if m.AccountID == "" || seen[m.AccountID] {
			continue
		}
		acct, err := r.store.GetAccount(ctx, m.AccountID)
		if err == nil && isLocal(acct) {
			seen[m.AccountID] = true
			out = append(out, m.AccountID)
		}
	}
	return out
}

func (r *EventRouter) broadcast(key string, wireStreams []string, event, payload string) {
	r.hub.broadcast(key, makeEnvelope(wireStreams, event, payload))
}

func (r *EventRouter) broadcastDelete(key, event, payload string) {
	r.hub.broadcast(key, makeEnvelope([]string{key}, event, payload))
}

func isLocal(a *models.Account) bool {
	return a != nil && a.Domain == nil
}

// NopPublisher is a Publisher that does nothing; used as the default so callers
// never need a nil check.
type NopPublisher struct{}

func (NopPublisher) StatusCreated(context.Context, *models.Status)             {}
func (NopPublisher) StatusUpdated(context.Context, *models.Status)             {}
func (NopPublisher) StatusDeleted(context.Context, *models.Status)             {}
func (NopPublisher) NotificationCreated(context.Context, *models.Notification) {}
func (NopPublisher) CacheInvalidated(context.Context)                          {}

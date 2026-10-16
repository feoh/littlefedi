package federation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/cache"
	contentlib "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/content"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/emoji"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mail"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/metrics"
	accountmigration "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/migration"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/opengraph"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/push"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/queue"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/streaming"
)

// InboxProcessor handles incoming activities from the job queue.
type InboxProcessor struct {
	blockSensitiveRelationshipNotifications bool

	store          storage.Storage
	federator      *Federator
	domain         string
	stream         streaming.Publisher // live streaming fan-out; nil until wired
	componentCache *cache.MemoryCache
	backfillCtx    context.Context
	backfillSlots  chan struct{}
}

func permanentActivity(format string, args ...any) error {
	return queue.Permanent(fmt.Errorf(format, args...))
}

func sameOrigin(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	return errA == nil && errB == nil && ua.Scheme == "https" && ub.Scheme == "https" && ua.Host != "" &&
		strings.EqualFold(ua.Host, ub.Host)
}

func (p *InboxProcessor) validateStatusObjectAuthority(actor string, object map[string]interface{}) error {
	id, _ := object["id"].(string)
	attributedTo := activityStreamsLink(object["attributedTo"])
	if actor == "" || id == "" || attributedTo != actor || !sameOrigin(actor, id) {
		return permanentActivity("status object is not owned by activity actor")
	}
	if u, err := url.Parse(id); err != nil || strings.EqualFold(u.Hostname(), p.domain) {
		return permanentActivity("remote activity attempted to use local object namespace")
	}
	return nil
}

func (p *InboxProcessor) statusOwnedByActor(ctx context.Context, status *models.Status, actor string) bool {
	if status == nil || actor == "" {
		return false
	}
	account, err := p.store.GetAccount(ctx, status.AccountID)
	return err == nil && account.ActorURI == actor
}

func (p *InboxProcessor) accountOwnedByActor(account *models.Account, actor string) bool {
	return account != nil && account.ActorURI == actor
}

func (p *InboxProcessor) followTargetsActor(ctx context.Context, follow *models.Follow, actor string) bool {
	if follow == nil {
		return false
	}
	target, err := p.store.GetAccount(ctx, follow.TargetAccountID)
	return err == nil && target.ActorURI == actor
}

func (p *InboxProcessor) remoteCanViewStatus(ctx context.Context, status *models.Status, viewer *models.Account) bool {
	if status == nil || viewer == nil {
		return false
	}
	switch status.Visibility {
	case models.VisibilityPublic, models.VisibilityUnlisted:
		return true
	case models.VisibilityPrivate, models.VisibilityDirect:
	default:
		return false
	}
	allowed, err := p.store.CanReadStatusVersion(ctx, status, viewer.ID)
	return err == nil && allowed
}

func (p *InboxProcessor) SetBlockSensitiveRelationshipNotifications(blocked bool) {
	p.blockSensitiveRelationshipNotifications = blocked
}

// NewInboxProcessor creates a new inbox processor.
func NewInboxProcessor(store storage.Storage, federator *Federator, domain string) *InboxProcessor {
	return &InboxProcessor{
		store:         store,
		federator:     federator,
		domain:        domain,
		backfillCtx:   context.Background(),
		backfillSlots: make(chan struct{}, 1),
	}
}

// ConfigureBackfill shares the process shutdown context and the configured
// thread-crawl concurrency with inbox-triggered parent/quote backfills.
func (p *InboxProcessor) ConfigureBackfill(ctx context.Context, concurrency int) {
	if ctx == nil {
		ctx = context.Background()
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 16 {
		concurrency = 16
	}
	p.backfillCtx = ctx
	p.backfillSlots = make(chan struct{}, concurrency)
}

// SetStreamPublisher wires the streaming fan-out so inbound posts and
// notifications reach connected clients live. Safe to leave unset.
func (p *InboxProcessor) SetStreamPublisher(pub streaming.Publisher) {
	p.stream = pub
}

func (p *InboxProcessor) SetComponentCache(componentCache *cache.MemoryCache) {
	p.componentCache = componentCache
}

func (p *InboxProcessor) invalidateStatusComponents(statusID string) {
	if p.componentCache != nil && statusID != "" {
		p.componentCache.DeleteDependencies("status:" + statusID)
	}
}

// streamNotification fans a freshly created inbound notification out to its
// recipient's live stream (best-effort, no-op when streaming is not wired).
func (p *InboxProcessor) streamNotification(ctx context.Context, notif *models.Notification) {
	if p.stream == nil || notif == nil {
		return
	}
	p.stream.CacheInvalidated(ctx)
	p.stream.NotificationCreated(ctx, notif)
}

// streamStatusDeleted / streamStatusUpdated fan inbound status deletes and edits
// out to live timelines, ensuring the author is loaded for routing.
func (p *InboxProcessor) streamStatusDeleted(ctx context.Context, status *models.Status) {
	if p.stream == nil || status == nil {
		return
	}
	if status.Account == nil && status.AccountID != "" {
		status.Account, _ = p.store.GetAccount(ctx, status.AccountID)
	}
	p.stream.StatusDeleted(ctx, status)
}

func (p *InboxProcessor) streamStatusUpdated(ctx context.Context, status *models.Status) {
	if p.stream == nil || status == nil {
		return
	}
	if status.Account == nil && status.AccountID != "" {
		status.Account, _ = p.store.GetAccount(ctx, status.AccountID)
	}
	p.stream.StatusUpdated(ctx, status)
}

// handledActivityTypes lists the activity types ProcessActivity has a branch
// for. The inbox handler checks it before writing a job row, because anything
// missing from it is only going to be dropped once a worker picks it up. A
// single PeerTube peer can announce every video View to our inbox, and each
// one otherwise costs an insert, a dequeue update and a completion update on
// storage where those writes are the scarce resource.
var handledActivityTypes = map[string]bool{
	"Follow": true, "Accept": true, "Reject": true, "Undo": true,
	"Create": true, "Delete": true, "Update": true,
	"Like": true, "EmojiReact": true, "Announce": true,
	"Block": true, "Flag": true, "Add": true, "Remove": true,
	"Move": true, "QuoteRequest": true,
}

func handledActivityType(activityType string) bool {
	return handledActivityTypes[activityType]
}

// ProcessActivity processes a single incoming ActivityPub activity.
func (p *InboxProcessor) ProcessActivity(ctx context.Context, activityJSON json.RawMessage) error {
	started := time.Now()
	var activity map[string]interface{}
	if err := json.Unmarshal(activityJSON, &activity); err != nil {
		return permanentActivity("unmarshal activity: %w", err)
	}

	activityType, _ := activity["type"].(string)
	activityID, _ := activity["id"].(string)
	defer func() { metrics.ObserveInboxActivityDuration(activityType, time.Since(started)) }()

	slog.Info("processing inbox activity", "type", activityType, "id", activityID)

	// Track the sending domain for liveness signals
	var senderDomain string
	if actorStr, ok := activity["actor"].(string); ok && actorStr != "" {
		if parsed, err := url.Parse(actorStr); err == nil && parsed.Hostname() != "" && parsed.Hostname() != p.domain {
			senderDomain = parsed.Hostname()
		}
	}

	// Moderation gate: refuse inbound traffic from suspended instances and
	// suspend-severity domain blocks. Silence-severity blocks are enforced at
	// read time (timelines filter them), so silenced-domain activity is still
	// accepted and stored.
	if senderDomain != "" && hostInboundBlocked(ctx, p.store, senderDomain, p.domain) {
		slog.Debug("inbox: dropping activity from blocked domain", "domain", senderDomain, "type", activityType)
		return nil
	}
	if senderDomain != "" {
		if err := p.federator.EnsureInstanceExists(ctx, senderDomain); err != nil {
			slog.Warn("inbox: failed to ensure instance record", "domain", senderDomain, "error", err)
		}
	}
	// Drop activity from an individually suspended remote account.
	if actorStr, _ := activity["actor"].(string); actorStr != "" {
		if acct, err := p.store.GetAccountByURI(ctx, actorStr); err == nil && acct != nil &&
			(acct.SuspendedAt != nil || acct.ModerationState == "suspended") {
			slog.Debug("inbox: dropping activity from suspended account", "actor", actorStr, "type", activityType)
			return nil
		}
	}

	var procErr error
	switch activityType { // keep handledActivityTypes in sync with these cases
	case "Follow":
		procErr = p.processFollow(ctx, activity)
	case "Accept":
		procErr = p.processAccept(ctx, activity)
	case "Reject":
		procErr = p.processReject(ctx, activity)
	case "Undo":
		procErr = p.processUndo(ctx, activity)
	case "Create":
		procErr = p.processCreate(ctx, activity)
	case "Delete":
		procErr = p.processDelete(ctx, activity)
	case "Update":
		procErr = p.processUpdate(ctx, activity)
	case "Like":
		if content, _ := activity["content"].(string); strings.TrimSpace(content) != "" {
			procErr = p.processEmojiReaction(ctx, activity)
		} else {
			procErr = p.processLike(ctx, activity)
		}
	case "EmojiReact":
		procErr = p.processEmojiReaction(ctx, activity)
	case "Announce":
		procErr = p.processAnnounce(ctx, activity)
	case "Block":
		procErr = p.processBlock(ctx, activity)
	case "Flag":
		procErr = p.processFlag(ctx, activity)
	case "Add":
		procErr = p.processAdd(ctx, activity)
	case "Remove":
		procErr = p.processRemove(ctx, activity)
	case "Move":
		procErr = p.processMove(ctx, activity)
	case "QuoteRequest":
		procErr = p.processQuoteRequest(ctx, activity)
	default:
		slog.Debug("unhandled activity type", "type", activityType)
	}

	// If processing succeeded, mark the sender instance as reachable
	if procErr == nil && senderDomain != "" {
		if err := p.store.MarkInstanceReachable(ctx, senderDomain); err != nil {
			slog.Warn("inbox: failed to mark instance reachable", "domain", senderDomain, "error", err)
		}
	}

	return procErr
}

func (p *InboxProcessor) processFollow(ctx context.Context, activity map[string]interface{}) error {
	actor, _ := activity["actor"].(string)
	object, _ := activity["object"].(string)
	activityID, _ := activity["id"].(string)

	if actor == "" || object == "" || activityID == "" || !sameOrigin(actor, activityID) {
		return permanentActivity("follow missing or foreign actor, object, or id")
	}

	// Resolve the remote actor
	remoteAccount, err := p.federator.FetchActorLight(ctx, actor)
	if err != nil {
		return fmt.Errorf("fetch follower: %w", err)
	}
	// Find the local account being followed
	localAccount, err := p.store.GetAccountByURI(ctx, object)
	if err != nil {
		return fmt.Errorf("find local account: %w", err)
	}
	if remoteAccount.Domain == nil || localAccount.Domain != nil {
		return permanentActivity("follow must be from a remote actor to a local actor")
	}
	if localAccount.IsServiceActor {
		relay, relayErr := p.store.GetRelayByAccountID(ctx, remoteAccount.ID)
		if relayErr != nil || !relay.Enabled {
			return p.sendReject(ctx, localAccount, remoteAccount, activityID)
		}
		if existing, err := p.store.GetFollowByPair(ctx, remoteAccount.ID, localAccount.ID); err == nil {
			if existing.State != models.FollowAccepted {
				if err := p.store.AcceptFollow(ctx, existing.ID); err != nil {
					return err
				}
			}
			return p.sendAccept(ctx, localAccount, remoteAccount, activityID)
		}
		follow := &models.Follow{AccountID: remoteAccount.ID, TargetAccountID: localAccount.ID, URI: activityID, State: models.FollowAccepted}
		if err := p.store.CreateFollow(ctx, follow); err != nil {
			return fmt.Errorf("create relay reverse follow: %w", err)
		}
		return p.sendAccept(ctx, localAccount, remoteAccount, activityID)
	}

	// Check if already following
	existingFollow, err := p.store.GetFollowByPair(ctx, remoteAccount.ID, localAccount.ID)
	if err == nil {
		slog.Info("follow already exists", "id", existingFollow.ID)
		if existingFollow.State == models.FollowAccepted {
			return p.sendAccept(ctx, localAccount, remoteAccount, activityID)
		}
		return nil
	}

	// Create follow record
	follow := &models.Follow{
		AccountID:       remoteAccount.ID,
		TargetAccountID: localAccount.ID,
		URI:             activityID,
		State:           models.FollowPending,
	}

	if !localAccount.ManuallyApprovesFollowers {
		follow.State = models.FollowAccepted
	}

	if err := p.store.CreateFollow(ctx, follow); err != nil {
		return fmt.Errorf("create follow: %w", err)
	}

	// If auto-accept, send Accept
	var acceptErr error
	if follow.State == models.FollowAccepted {
		if acceptErr = p.sendAccept(ctx, localAccount, remoteAccount, activityID); acceptErr != nil {
			slog.Error("failed to send accept", "error", acceptErr)
		}
	}

	// Create notification
	notifType := "follow"
	if follow.State == models.FollowPending {
		notifType = "follow_request"
	}

	notif := &models.Notification{
		Type:          notifType,
		AccountID:     localAccount.ID,
		FromAccountID: remoteAccount.ID,
	}
	if err := p.store.UpsertNotification(ctx, notif); err != nil {
		slog.Error("failed to upsert notification", "error", err)
	} else {
		p.streamNotification(ctx, notif)
		push.EnqueuePushNotification(ctx, p.store, notif)
		mail.EnqueueEmailNotification(ctx, p.store, p.domain, notif)
	}

	slog.Info("processed follow", "from", actor, "to", object, "state", follow.State)
	return acceptErr
}

func (p *InboxProcessor) processAccept(ctx context.Context, activity map[string]interface{}) error {
	actor, _ := activity["actor"].(string)
	if actor == "" {
		return permanentActivity("accept missing actor")
	}
	// A quote author's server accepting one of our QuoteRequests.
	if handled, err := p.tryProcessQuoteAccept(ctx, activity); handled {
		return err
	}

	object, ok := activity["object"].(map[string]interface{})
	if !ok {
		// Try string
		objectStr, ok := activity["object"].(string)
		if !ok {
			return permanentActivity("accept missing valid object")
		}
		// Find follow by URI
		follow, err := p.store.GetFollowByURI(ctx, objectStr)
		if err != nil {
			slog.Warn("accept for unknown follow", "uri", objectStr)
			return nil
		}
		if !p.followTargetsActor(ctx, follow, actor) {
			return permanentActivity("accept actor is not follow target")
		}
		if err := p.store.AcceptFollow(ctx, follow.ID); err != nil {
			return err
		}
		p.notifyFollowResponse(ctx, follow, "follow_accept")
		return nil
	}

	// Object is an activity - extract the follow URI
	objectID, _ := object["id"].(string)
	if objectID == "" {
		return permanentActivity("accept object missing id")
	}

	follow, err := p.store.GetFollowByURI(ctx, objectID)
	if err != nil {
		slog.Warn("accept for unknown follow", "uri", objectID)
		return nil
	}
	if !p.followTargetsActor(ctx, follow, actor) {
		return permanentActivity("accept actor is not follow target")
	}

	if err := p.store.AcceptFollow(ctx, follow.ID); err != nil {
		return err
	}
	p.notifyFollowResponse(ctx, follow, "follow_accept")
	return nil
}

func (p *InboxProcessor) processReject(ctx context.Context, activity map[string]interface{}) error {
	actor, _ := activity["actor"].(string)
	if actor == "" {
		return permanentActivity("reject missing actor")
	}
	object, ok := activity["object"].(map[string]interface{})
	if !ok {
		objectStr, ok := activity["object"].(string)
		if !ok {
			return permanentActivity("reject missing valid object")
		}
		follow, err := p.store.GetFollowByURI(ctx, objectStr)
		if err != nil {
			slog.Warn("reject for unknown follow", "uri", objectStr)
			return nil
		}
		if !p.followTargetsActor(ctx, follow, actor) {
			return permanentActivity("reject actor is not follow target")
		}
		if err := p.store.RejectFollow(ctx, follow.ID); err != nil {
			return err
		}
		p.notifyFollowResponse(ctx, follow, "follow_reject")
		return nil
	}

	objectID, _ := object["id"].(string)
	if objectID == "" {
		return permanentActivity("reject object missing id")
	}

	follow, err := p.store.GetFollowByURI(ctx, objectID)
	if err != nil {
		slog.Warn("reject for unknown follow", "uri", objectID)
		return nil
	}
	if !p.followTargetsActor(ctx, follow, actor) {
		return permanentActivity("reject actor is not follow target")
	}

	if err := p.store.RejectFollow(ctx, follow.ID); err != nil {
		return err
	}
	p.notifyFollowResponse(ctx, follow, "follow_reject")
	return nil
}

// processQuoteRequest handles an incoming FEP-044f QuoteRequest: a remote actor
// asking permission to quote one of our local statuses. We grant or deny based
// on the quoted author's QuotePolicy and reply with an Accept (carrying a
// QuoteAuthorization) or a Reject.
func (p *InboxProcessor) processQuoteRequest(ctx context.Context, activity map[string]interface{}) error {
	actorURI, _ := activity["actor"].(string)
	quotedURI := activityStreamsLink(activity["object"])
	quotingURI := activityStreamsLink(activity["instrument"])
	requestID, _ := activity["id"].(string)
	if actorURI == "" || quotedURI == "" || quotingURI == "" || requestID == "" ||
		!sameOrigin(actorURI, requestID) || !sameOrigin(actorURI, quotingURI) {
		return permanentActivity("quote request missing or foreign actor, id, object, or instrument")
	}

	quoted, err := p.store.GetStatusByURI(ctx, quotedURI)
	if err != nil {
		return fmt.Errorf("quote request for unknown object %q: %w", quotedURI, err)
	}
	author, err := p.store.GetAccount(ctx, quoted.AccountID)
	if err != nil {
		return fmt.Errorf("quote request: load author: %w", err)
	}
	if author.Domain != nil {
		return fmt.Errorf("quote request target %q is not local", quotedURI)
	}

	requester, err := p.federator.FetchActorLight(ctx, actorURI)
	if err != nil {
		return fmt.Errorf("quote request: fetch requester: %w", err)
	}

	follows := false
	if f, err := p.store.GetFollowByPair(ctx, requester.ID, author.ID); err == nil && f.State == models.FollowAccepted {
		follows = true
	}

	if !QuotePolicyAllows(author.QuotePolicy, quoted.Visibility, follows) {
		slog.Info("quote request denied", "actor", actorURI, "object", quotedURI, "policy", author.QuotePolicy)
		return p.sendQuoteResponse(ctx, author, requester, requestID, quotedURI, quotingURI, "")
	}

	authID := uuid.New().String()
	authURI := fmt.Sprintf("https://%s/objects/%s/quote_authorizations/%s", p.domain, quoted.ID, authID)
	if err := p.store.CreateQuoteAuthorization(ctx, &models.QuoteAuthorization{
		ID:              authID,
		URI:             authURI,
		QuotedStatusID:  quoted.ID,
		QuotingURI:      quotingURI,
		QuotingActorURI: actorURI,
	}); err != nil {
		return fmt.Errorf("quote request: create authorization: %w", err)
	}
	slog.Info("quote request granted", "actor", actorURI, "object", quotedURI, "authorization", authURI)
	return p.sendQuoteResponse(ctx, author, requester, requestID, quotedURI, quotingURI, authURI)
}

// sendQuoteResponse delivers an Accept (when authURI is non-empty) or Reject to
// the requesting actor's inbox.
func (p *InboxProcessor) sendQuoteResponse(ctx context.Context, author, requester *models.Account, requestID, quotedURI, quotingURI, authURI string) error {
	inbox := requester.InboxURI
	if requester.SharedInboxURI != nil && *requester.SharedInboxURI != "" {
		inbox = *requester.SharedInboxURI
	}
	if inbox == "" {
		return nil
	}
	if authURI == "" {
		reject := map[string]interface{}{
			"@context": "https://www.w3.org/ns/activitystreams",
			"id":       author.ActorURI + "/rejects/" + uuid.NewString(),
			"type":     "Reject",
			"actor":    author.ActorURI,
			"object":   requestID,
			"to":       []string{requester.ActorURI},
		}
		return p.federator.Deliver(ctx, inbox, reject)
	}
	accept := map[string]interface{}{
		"@context":   QuoteJSONLDContext(),
		"id":         authURI + "/accept",
		"type":       "Accept",
		"actor":      author.ActorURI,
		"object":     requestID,
		"instrument": quotingURI,
		"result":     buildQuoteAuthorizationAS2(authURI, author.ActorURI, quotingURI, quotedURI),
		"to":         []string{requester.ActorURI},
	}
	return p.federator.Deliver(ctx, inbox, accept)
}

// tryProcessQuoteAccept handles an Accept that authorizes one of our outgoing
// quotes. It reports whether the activity was a quote accept (so the caller can
// stop). When matched, it attaches the authorization to the quoting status and
// re-federates an Update so followers' servers can render the embed.
func (p *InboxProcessor) tryProcessQuoteAccept(ctx context.Context, activity map[string]interface{}) (bool, error) {
	var quotingURI string
	switch obj := activity["object"].(type) {
	case map[string]interface{}:
		if t, _ := obj["type"].(string); t != "QuoteRequest" {
			return false, nil
		}
		quotingURI = activityStreamsLink(obj["instrument"])
		if quotingURI == "" {
			if id, _ := obj["id"].(string); strings.HasSuffix(id, "/quote_request") {
				quotingURI = strings.TrimSuffix(id, "/quote_request")
			}
		}
	case string:
		if strings.HasSuffix(obj, "/quote_request") {
			quotingURI = strings.TrimSuffix(obj, "/quote_request")
		}
	}
	if quotingURI == "" {
		quotingURI = activityStreamsLink(activity["instrument"])
	}
	if quotingURI == "" {
		return false, nil
	}

	status, err := p.store.GetStatusByURI(ctx, quotingURI)
	if err != nil || status.QuoteOfURI == nil {
		return false, nil
	}
	actor, _ := activity["actor"].(string)
	quoted, err := p.store.GetStatusByURI(ctx, *status.QuoteOfURI)
	if err != nil || !p.statusOwnedByActor(ctx, quoted, actor) {
		return true, permanentActivity("quote accept actor does not own quoted status")
	}

	status.QuoteState = "accepted"
	if authURI := activityStreamsLink(activity["result"]); authURI != "" {
		if !sameOrigin(actor, authURI) {
			return true, permanentActivity("quote authorization has foreign origin")
		}
		status.QuoteApprovalURI = &authURI
	}
	if err := p.store.UpdateStatusQuoteState(ctx, status.ID, status.QuoteState, status.QuoteApprovalURI); err != nil {
		return true, err
	}
	slog.Info("quote authorized", "status", status.URI, "authorization", status.QuoteApprovalURI)
	p.sendQuoteUpdate(ctx, status)
	return true, nil
}

// sendQuoteUpdate re-federates a quoting status as an Update once it has been
// authorized, so the now-attached quoteAuthorization reaches followers.
func (p *InboxProcessor) sendQuoteUpdate(ctx context.Context, status *models.Status) {
	account, err := p.store.GetAccount(ctx, status.AccountID)
	if err != nil {
		return
	}
	if media, err := p.store.GetMediaForStatus(ctx, status.ID); err == nil {
		status.MediaAttachments = convertMediaAttachments(media)
	}
	if polls, err := p.store.GetPollsForStatuses(ctx, []string{status.ID}, ""); err == nil {
		status.Poll = polls[status.ID]
	}
	attachStoredPreview(ctx, p.store, status)
	note := buildNoteAS2(p.domain, status, account)
	update := map[string]interface{}{
		"@context": QuoteJSONLDContext(),
		"id":       fmt.Sprintf("%s/quote-update/%s", status.URI, uuid.New().String()),
		"type":     "Update",
		"actor":    account.ActorURI,
		"object":   note,
		"to":       note["to"],
		"cc":       note["cc"],
	}
	dh := NewDeliveryHandler(p.store, p.federator)
	if err := dh.DeliverToFollowers(ctx, update, account.ID); err != nil {
		slog.Warn("quote update delivery failed", "status", status.URI, "error", err)
	}
}

func (p *InboxProcessor) notifyFollowResponse(ctx context.Context, follow *models.Follow, notifType string) {
	from, err := p.store.GetAccount(ctx, follow.TargetAccountID)
	if err != nil {
		return
	}
	p.notifyRelationship(ctx, notifType, follow.AccountID, from)
}

func (p *InboxProcessor) notifyRelationship(ctx context.Context, notifType, accountID string, from *models.Account) {
	if from == nil || accountID == "" || accountID == from.ID {
		return
	}
	target, err := p.store.GetAccount(ctx, accountID)
	if err != nil || target.Domain != nil || target.IsServiceActor {
		return
	}
	if !target.AllowsRelationshipNotification(notifType, p.blockSensitiveRelationshipNotifications) {
		return
	}
	notif := &models.Notification{Type: notifType, AccountID: accountID, FromAccountID: from.ID}
	if err := p.store.UpsertNotification(ctx, notif); err != nil {
		slog.Warn("inbox: failed to create relationship notification", "type", notifType, "error", err)
		return
	}
	p.streamNotification(ctx, notif)
	push.EnqueuePushNotification(ctx, p.store, notif)
	mail.EnqueueEmailNotification(ctx, p.store, p.domain, notif)
}

func (p *InboxProcessor) deleteFollowAndNotify(ctx context.Context, follow *models.Follow, from *models.Account) error {
	if err := p.store.DeleteFollow(ctx, follow.ID); err != nil {
		return err
	}
	p.notifyRelationship(ctx, "unfollow", follow.TargetAccountID, from)
	return nil
}

func (p *InboxProcessor) processUndo(ctx context.Context, activity map[string]interface{}) error {
	actor, _ := activity["actor"].(string)
	if actor == "" {
		return permanentActivity("undo missing actor")
	}

	// Resolve actor
	remoteAccount, err := p.federator.FetchActorLight(ctx, actor)
	if err != nil {
		return fmt.Errorf("fetch actor for undo: %w", err)
	}

	objectValue := activity["object"]
	objectID := activityStreamsLink(objectValue)
	object, embedded := objectValue.(map[string]interface{})
	if embedded {
		objectActor, _ := object["actor"].(string)
		objectType, _ := object["type"].(string)
		content, _ := object["content"].(string)
		isReaction := objectType == "EmojiReact" || (objectType == "Like" && strings.TrimSpace(content) != "")
		if (isReaction && objectActor != actor) || (objectActor != "" && objectActor != actor) {
			return permanentActivity("undo actor does not own object")
		}
	}
	if objectID != "" {
		if reaction, err := p.store.DeleteEmojiReactionByURI(ctx, remoteAccount.ID, objectID); err == nil {
			p.invalidateStatusComponents(reaction.StatusID)
			return nil
		}
		if fave, err := p.store.GetFavouriteByURI(ctx, objectID); err == nil && fave.AccountID == remoteAccount.ID {
			if err := p.store.DeleteFavourite(ctx, fave.AccountID, fave.StatusID); err != nil {
				return err
			}
			p.invalidateStatusComponents(fave.StatusID)
			return nil
		}
	}
	if !embedded {
		if objectID != "" {
			return nil
		}
		return permanentActivity("undo missing object")
	}

	objectType, _ := object["type"].(string)

	switch objectType {
	case "Follow":
		objectID, _ := object["id"].(string)
		if objectID != "" {
			follow, err := p.store.GetFollowByURI(ctx, objectID)
			if err == nil && follow.AccountID == remoteAccount.ID {
				return p.deleteFollowAndNotify(ctx, follow, remoteAccount)
			}
		}

		// Fallback: find by actor pair
		objectActor, _ := object["actor"].(string)
		objectObject, _ := object["object"].(string)

		if objectActor == actor && objectObject != "" {
			targetAccount, err := p.store.GetAccountByURI(ctx, objectObject)
			if err != nil {
				return fmt.Errorf("find target: %w", err)
			}
			follow, err := p.store.GetFollowByPair(ctx, remoteAccount.ID, targetAccount.ID)
			if err == nil {
				return p.deleteFollowAndNotify(ctx, follow, remoteAccount)
			}
		}

		// Try with the undo actor as the follower
		objectObject2, _ := object["object"].(string)
		if objectObject2 != "" {
			target, err := p.store.GetAccountByURI(ctx, objectObject2)
			if err == nil {
				follow, err := p.store.GetFollowByPair(ctx, remoteAccount.ID, target.ID)
				if err == nil {
					return p.deleteFollowAndNotify(ctx, follow, remoteAccount)
				}
			}
		}

	case "Block":
		objectTarget, _ := object["object"].(string)
		if objectTarget != "" {
			target, err := p.store.GetAccountByURI(ctx, objectTarget)
			if err == nil {
				if err := p.store.DeleteBlock(ctx, remoteAccount.ID, target.ID); err != nil {
					return err
				}
				p.notifyRelationship(ctx, "unblock", target.ID, remoteAccount)
			}
		}

	case "Like":
		if content, _ := object["content"].(string); strings.TrimSpace(content) != "" {
			return nil
		}
		objectObject := activityStreamsLink(object["object"])
		if objectObject != "" {
			status, err := p.store.GetStatusByURI(ctx, objectObject)
			if err == nil {
				return p.store.DeleteFavourite(ctx, remoteAccount.ID, status.ID)
			}
		}

	case "Announce":
		objectID, _ := object["id"].(string)
		if objectID != "" {
			announceStatus, err := p.store.GetStatusByURI(ctx, objectID)
			if err == nil && announceStatus.AccountID == remoteAccount.ID {
				originalID := ""
				if announceStatus.BoostOfID != nil {
					originalID = *announceStatus.BoostOfID
				}
				if err := p.store.DeleteStatus(ctx, announceStatus.ID); err != nil {
					return err
				}
				p.invalidateStatusComponents(originalID)
				return nil
			}
		}
	}

	slog.Info("processed undo", "type", objectType)
	return nil
}

// hasLocalAudience reports whether an inbound status object can matter to
// anyone on this instance: its author is followed by a local user, it
// addresses or mentions a local actor, it replies to or quotes a status we
// already have, or it carries a hashtag a local user follows. Everything else
// is shared-inbox noise - processing it costs actor fetches, database rows and
// cache churn for content no local timeline can ever show.
func (p *InboxProcessor) hasLocalAudience(ctx context.Context, actorURI string, object map[string]interface{}) bool {
	if p.actorFollowedLocally(ctx, actorURI) {
		return true
	}
	if p.addressesLocalActor(object) {
		return true
	}
	if parentURI, _ := object["inReplyTo"].(string); parentURI != "" {
		if _, err := p.store.GetStatusByURI(ctx, parentURI); err == nil {
			return true
		}
	}
	for _, key := range []string{"quote", "quoteUri", "_misskey_quote"} {
		if quoted, _ := object[key].(string); quoted != "" {
			if _, err := p.store.GetStatusByURI(ctx, quoted); err == nil {
				return true
			}
		}
	}
	if names := objectHashtagNames(object); len(names) > 0 {
		if followed, err := p.store.AnyTagFollowed(ctx, names); err != nil || followed {
			// On lookup error err on the side of processing.
			return true
		}
	}
	return false
}

// actorFollowedLocally reports whether the actor is followed (accepted) by at
// least one local account. An actor we have never stored cannot be followed.
func (p *InboxProcessor) actorFollowedLocally(ctx context.Context, actorURI string) bool {
	if actorURI == "" {
		return false
	}
	account, err := p.store.GetAccountByURI(ctx, actorURI)
	if err != nil || account == nil {
		return false
	}
	if account.Domain == nil {
		return true
	}
	followed, err := p.store.HasLocalFollowers(ctx, account.ID)
	if err != nil {
		return true
	}
	return followed
}

// addressesLocalActor scans the object's to/cc addressing and Mention tags for
// a URI on this instance's domain.
func (p *InboxProcessor) addressesLocalActor(object map[string]interface{}) bool {
	isLocal := func(uri string) bool {
		if uri == "" {
			return false
		}
		parsed, err := url.Parse(uri)
		return err == nil && strings.EqualFold(parsed.Hostname(), p.domain)
	}
	for _, field := range []string{"to", "cc"} {
		switch v := object[field].(type) {
		case string:
			if isLocal(v) {
				return true
			}
		case []interface{}:
			for _, item := range v {
				if s, ok := item.(string); ok && isLocal(s) {
					return true
				}
			}
		}
	}
	for _, tag := range objectTagEntries(object) {
		if t, _ := tag["type"].(string); t != "Mention" {
			continue
		}
		if href, _ := tag["href"].(string); isLocal(href) {
			return true
		}
	}
	return false
}

// isLocalObjectURI reports whether the URI lives on this instance's domain.
func (p *InboxProcessor) isLocalObjectURI(uri string) bool {
	if uri == "" {
		return false
	}
	parsed, err := url.Parse(uri)
	return err == nil && strings.EqualFold(parsed.Hostname(), p.domain)
}

// objectTagEntries returns the object's tag list, tolerating both the array
// and single-object forms.
func objectTagEntries(object map[string]interface{}) []map[string]interface{} {
	switch v := object["tag"].(type) {
	case []interface{}:
		out := make([]map[string]interface{}, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				out = append(out, m)
			}
		}
		return out
	case map[string]interface{}:
		return []map[string]interface{}{v}
	}
	return nil
}

func objectHashtagNames(object map[string]interface{}) []string {
	var names []string
	for _, tag := range objectTagEntries(object) {
		if t, _ := tag["type"].(string); t != "Hashtag" {
			continue
		}
		if name, _ := tag["name"].(string); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func (p *InboxProcessor) processCreate(ctx context.Context, activity map[string]interface{}) error {
	actor, _ := activity["actor"].(string)
	object, ok := activity["object"].(map[string]interface{})
	if actor == "" || !ok {
		return permanentActivity("create missing actor or object")
	}

	objectType, _ := object["type"].(string)
	if !isStatusObjectType(objectType) {
		slog.Info("unhandled create object type", "type", objectType)
		return nil
	}
	if err := p.validateStatusObjectAuthority(actor, object); err != nil {
		return err
	}

	// Relevance gate: shared inboxes deliver plenty of statuses from actors
	// nobody here follows. Drop them before the actor fetch and the store -
	// they can never surface for a local user.
	relayDelivery, _ := activity["_littlefedi_relay"].(bool)
	if !relayDelivery && !p.hasLocalAudience(ctx, actor, object) {
		objectID, _ := object["id"].(string)
		slog.Debug("inbox: dropping create with no local audience", "actor", actor, "object", objectID)
		return nil
	}

	// A relayed object was already author-validated while dereferencing it.
	// Ordinary wire activities cannot inject this Go pointer through JSON.
	remoteAccount, _ := activity["_littlefedi_author"].(*models.Account)
	if remoteAccount == nil || remoteAccount.ActorURI != actor {
		var err error
		remoteAccount, err = p.federator.FetchActorLight(ctx, actor)
		if err != nil {
			return fmt.Errorf("fetch actor for create: %w", err)
		}
	}
	// ActivityPub poll votes are Create(Note) objects whose inReplyTo points at
	// the Question and whose name/content is the selected option.
	if parentURI, ok := object["inReplyTo"].(string); ok && objectType == "Note" {
		if parent, e := p.store.GetStatusByURI(ctx, parentURI); e == nil {
			if poll, e := p.store.GetPollForStatus(ctx, parent.ID, remoteAccount.ID); e == nil {
				if !p.remoteCanViewStatus(ctx, parent, remoteAccount) {
					return permanentActivity("poll voter is not in status audience")
				}
				choice, _ := object["name"].(string)
				if choice == "" {
					choice, _ = object["content"].(string)
				}
				for i, o := range poll.Options {
					if strings.TrimSpace(choice) == o.Title {
						_, e = p.store.VotePoll(ctx, poll.ID, remoteAccount.ID, []int{i})
						if e == nil {
							p.invalidateStatusComponents(parent.ID)
						}
						return e
					}
				}
			}
		}
	}

	// Parse the status
	status, err := p.parseRemoteStatus(ctx, object, remoteAccount)
	if err != nil {
		return fmt.Errorf("parse remote status: %w", err)
	}

	// Check for duplicates
	existing, err := p.store.GetStatusByURI(ctx, status.URI)
	promoted := false
	if err == nil {
		if !p.statusOwnedByActor(ctx, existing, actor) {
			return permanentActivity("create actor does not own existing object")
		}
		if !existing.ContextOnly {
			// Already stored: a re-delivery from the sender, an object we
			// fetched through some other path (a Like or Announce on it, an
			// Update that arrived first), or this very job re-run after its
			// CreateStatus committed. Replay the notifications rather than
			// returning here - they are keyed per recipient and status, so
			// anything already recorded stays untouched, and the run that died
			// between the status write and its notifications is recovered.
			slog.Info("status already exists", "uri", status.URI, "id", existing.ID)
			mentions, err := p.store.GetMentionsForStatus(ctx, existing.ID)
			if err != nil {
				return fmt.Errorf("load stored status mentions: %w", err)
			}
			existing.Mentions = make([]models.Mention, len(mentions))
			for i, mention := range mentions {
				existing.Mentions[i] = *mention
			}
			return p.notifyForRemoteStatus(ctx, existing, remoteAccount, false)
		}
		if err := p.store.SetStatusContextOnly(ctx, existing.ID, false); err != nil {
			return fmt.Errorf("promote context status: %w", err)
		}
		status.ID = existing.ID
		status.ContextOnly = false
		promoted = true
	}

	// Store the status
	if !promoted {
		if err := p.store.CreateStatus(ctx, status); err != nil {
			return fmt.Errorf("store status: %w", err)
		}
		if objectType == "Question" {
			if err := p.ensureQuestionPoll(ctx, status.ID, object); err != nil {
				return err
			}
			status.Poll, _ = p.store.GetPollForStatus(ctx, status.ID, "")
		}
	}
	_ = p.store.LinkStatusRepliesByURI(ctx, status.URI, status.ID)
	// Connect any earlier-seen quoting statuses now that this object exists.
	_ = p.store.LinkStatusQuotesByURI(ctx, status.URI, status.ID)
	if status.InReplyToURI != nil && *status.InReplyToURI != "" {
		if parent, err := p.store.GetStatusByURI(ctx, *status.InReplyToURI); err == nil {
			_ = p.store.LinkStatusRepliesByURI(ctx, parent.URI, parent.ID)
		}
	}

	// Stream the inbound status to live timelines (home of local followers, plus
	// public/hashtag streams). Context-only statuses pulled for thread backfill
	// are excluded - they must not be injected into timelines. This runs before
	// the notifications so a failing notification write (which fails the job for
	// a retry) does not cost connected clients the live post.
	if p.stream != nil && !status.ContextOnly {
		if status.Account == nil {
			status.Account = remoteAccount
		}
		p.stream.StatusCreated(ctx, status)
	}

	slog.Info("stored remote status", "uri", status.URI, "type", objectType)
	return p.notifyForRemoteStatus(ctx, status, remoteAccount, false)
}

// notifyForRemoteStatus records every notification an inbound status owes a
// local account: mentions, a reply to a local status, a quote of one, edits to
// a post the account boosted, and the bell subscriptions on the author.
//
// Publication notifications are idempotent: an existing row for the same
// (recipient, actor, type, status) key is left alone. Edit notifications are
// refreshed because each Update is a new event for someone who shared the post.
//
// Write failures are returned rather than logged and dropped. By the time this
// runs the status is committed and, through the blog storage decorator, its
// site rebuild is already queued; a notification lost here is lost for good
// unless the inbox job fails and retries. That is the same reasoning
// blog.NotifyRebuilt documents for the rebuild's own notification.
func (p *InboxProcessor) notifyForRemoteStatus(ctx context.Context, status *models.Status, from *models.Account, notifyRebloggers bool) error {
	// Track what this pass already recorded: per type and recipient for the
	// write itself, and per recipient so the bell fan-out skips anyone the
	// mention, reply, quote or update branches already reached (a reply commonly
	// also @-mentions the parent author).
	notified := map[string]bool{}
	notifiedAccounts := map[string]bool{}

	notify := func(notifType, accountID string) error {
		if notified[notifType+"\x00"+accountID] {
			return nil
		}
		notified[notifType+"\x00"+accountID] = true
		notifiedAccounts[accountID] = true
		notif := &models.Notification{
			ID:            uuid.NewString(),
			CreatedAt:     time.Now(),
			Type:          notifType,
			AccountID:     accountID,
			FromAccountID: from.ID,
			StatusID:      &status.ID,
		}
		if notifType == "update" {
			if err := p.store.UpsertNotification(ctx, notif); err != nil {
				return fmt.Errorf("create update notification: %w", err)
			}
			p.streamNotification(ctx, notif)
			return nil
		}
		pushJobs, err := push.NotificationJobs(ctx, p.store, notif)
		if err != nil {
			return err
		}
		emailJob, err := mail.NotificationJob(ctx, p.store, p.domain, notif)
		if err != nil {
			return err
		}
		jobs := pushJobs
		if emailJob != nil {
			jobs = append(jobs, emailJob)
		}
		created, err := p.store.CreateNotificationOnce(ctx, notif, jobs)
		if err != nil {
			return fmt.Errorf("create %s notification: %w", notifType, err)
		}
		if !created {
			return nil
		}
		p.streamNotification(ctx, notif)
		return nil
	}

	// Create notification for mentions
	for _, mention := range status.Mentions {
		mentionedAccount, err := p.store.GetAccountByURI(ctx, mention.URL)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("load mentioned account: %w", err)
		}
		if mentionedAccount.Domain != nil {
			continue
		}
		if err := notify("mention", mentionedAccount.ID); err != nil {
			return err
		}
	}

	// Create notification for reply (when someone replies to a local user's status)
	if status.InReplyToURI != nil && *status.InReplyToURI != "" {
		parent, err := p.store.GetStatusByURI(ctx, *status.InReplyToURI)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("load reply parent: %w", err)
		}
		if err == nil {
			// Don't notify for self-replies
			if parent.Account != nil && parent.Account.Domain == nil && parent.AccountID != from.ID {
				if err := notify("mention", parent.AccountID); err != nil {
					return err
				}
			}
		}
	}

	// Create notification for quote (when someone quotes a local user's status)
	if status.QuoteOfID != nil {
		quoted, err := p.store.GetStatus(ctx, *status.QuoteOfID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("load quoted status: %w", err)
		}
		if err == nil && quoted.Account != nil && quoted.Account.Domain == nil && quoted.AccountID != from.ID {
			if err := notify("quote", quoted.AccountID); err != nil {
				return err
			}
		}
	}

	var updateRecipients []string
	if notifyRebloggers {
		rebloggers, err := p.store.ListReblogAccounts(ctx, status.ID, -1)
		if err != nil {
			return fmt.Errorf("list local rebloggers: %w", err)
		}
		for _, reblogger := range rebloggers {
			if reblogger.Domain == nil && reblogger.ID != from.ID {
				updateRecipients = append(updateRecipients, reblogger.ID)
				notifiedAccounts[reblogger.ID] = true
			}
		}
	}

	// Notify local followers who enabled the bell on this remote author. Only
	// top-level, non-direct posts qualify, and we skip anyone who already got a
	// mention, reply or edit notification for this status.
	isReply := (status.InReplyToURI != nil && *status.InReplyToURI != "") || status.InReplyToID != nil
	if !isReply && status.Visibility != models.VisibilityDirect {
		followerIDs, err := p.store.ListNotifyFollowerIDs(ctx, from.ID)
		if err != nil {
			return fmt.Errorf("list notify followers: %w", err)
		}
		for _, fid := range followerIDs {
			if notifiedAccounts[fid] {
				continue
			}
			if err := notify("status", fid); err != nil {
				return err
			}
		}
	}
	for _, accountID := range updateRecipients {
		if err := notify("update", accountID); err != nil {
			return err
		}
	}

	// A cancelled context (the queue preempting this job for a foreground
	// request) makes every lookup above fail, and a missing recipient is
	// indistinguishable from a skipped one. Report that as an error so the job
	// is retried rather than recorded as a delivery that notified nobody.
	return ctx.Err()
}

func pollFromAS2(statusID string, object map[string]interface{}) *models.Poll {
	raw, ok := object["oneOf"].([]interface{})
	multiple := false
	if !ok {
		raw, ok = object["anyOf"].([]interface{})
		multiple = ok
	}
	if len(raw) < 2 {
		return nil
	}
	p := &models.Poll{ID: uuid.NewString(), StatusID: statusID, Multiple: multiple}
	if end, ok := object["endTime"].(string); ok {
		if t, err := time.Parse(time.RFC3339, end); err == nil {
			p.ExpiresAt = &t
			p.Expired = time.Now().After(t)
		}
	}
	for i, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		title, _ := m["name"].(string)
		o := models.PollOption{Position: i, Title: title}
		if replies, ok := m["replies"].(map[string]interface{}); ok {
			if n, ok := replies["totalItems"].(float64); ok {
				o.VotesCount = int(n)
				p.VotesCount += int(n)
			}
		}
		p.Options = append(p.Options, o)
	}
	if len(p.Options) < 2 {
		return nil
	}
	return p
}

func (p *InboxProcessor) ensureQuestionPoll(ctx context.Context, statusID string, object map[string]interface{}) error {
	poll := pollFromAS2(statusID, object)
	if poll == nil {
		return nil
	}
	if err := p.store.SyncPoll(ctx, poll); err != nil {
		return fmt.Errorf("sync poll: %w", err)
	}
	return nil
}

func (p *InboxProcessor) processDelete(ctx context.Context, activity map[string]interface{}) error {
	actor, _ := activity["actor"].(string)
	if actor == "" {
		return permanentActivity("delete missing actor")
	}
	object, ok := activity["object"].(map[string]interface{})
	if !ok {
		// Object is a string (URI) - delete by URI
		objectStr, ok := activity["object"].(string)
		if !ok {
			return permanentActivity("delete missing object")
		}
		// Check if it's an account
		account, err := p.store.GetAccountByURI(ctx, objectStr)
		if err == nil {
			if !p.accountOwnedByActor(account, actor) {
				return permanentActivity("delete actor does not own account")
			}
			return p.store.DeleteAccount(ctx, account.ID)
		}
		// Check if it's a status
		status, err := p.store.GetStatusByURI(ctx, objectStr)
		if err == nil {
			if !p.statusOwnedByActor(ctx, status, actor) {
				return permanentActivity("delete actor does not own status")
			}
			if err := p.store.DeleteNotificationsForStatus(ctx, status.ID); err != nil {
				slog.Error("failed to delete notifications", "error", err)
			}
			p.streamStatusDeleted(ctx, status)
			return p.store.DeleteStatus(ctx, status.ID)
		}
		if !sameOrigin(actor, objectStr) {
			return permanentActivity("delete actor does not own object origin")
		}
		if u, err := url.Parse(objectStr); err != nil || strings.EqualFold(u.Hostname(), p.domain) {
			return permanentActivity("remote delete attempted local object namespace")
		}
		slog.Info("delete for unknown object", "uri", objectStr)
		return nil
	}

	objectType, _ := object["type"].(string)
	objectID, _ := object["id"].(string)

	switch objectType {
	case "Tombstone":
		// Store tombstone metadata before deleting the content
		if objectID != "" {
			if !sameOrigin(actor, objectID) {
				return permanentActivity("delete actor does not own tombstone origin")
			}
			formerType, _ := object["formerType"].(string)
			if formerType == "" {
				formerType = "Note"
			}
			deletedStr, _ := object["deleted"].(string)
			deletedAt := time.Now()
			if deletedStr != "" {
				if t, err := time.Parse(time.RFC3339, deletedStr); err == nil {
					deletedAt = t
				}
			}
			if p.isLocalObjectURI(objectID) || !isStatusObjectType(formerType) {
				_ = p.store.CreateTombstone(ctx, &models.Tombstone{
					URI:        objectID,
					FormerType: formerType,
					DeletedAt:  deletedAt,
				})
			}
		}
		// Delete local copy
		if objectID != "" {
			status, err := p.store.GetStatusByURI(ctx, objectID)
			if err == nil {
				if !p.statusOwnedByActor(ctx, status, actor) {
					return permanentActivity("delete actor does not own status")
				}
				if err := p.store.DeleteNotificationsForStatus(ctx, status.ID); err != nil {
					slog.Error("failed to delete notifications", "error", err)
				}
				p.streamStatusDeleted(ctx, status)
				return p.store.DeleteStatus(ctx, status.ID)
			}
			account, err := p.store.GetAccountByURI(ctx, objectID)
			if err == nil {
				if !p.accountOwnedByActor(account, actor) {
					return permanentActivity("delete actor does not own account")
				}
				return p.store.DeleteAccount(ctx, account.ID)
			}
		}
	}

	slog.Info("processed delete", "type", objectType)
	return nil
}

func (p *InboxProcessor) processUpdate(ctx context.Context, activity map[string]interface{}) error {
	actor, _ := activity["actor"].(string)
	object, ok := activity["object"].(map[string]interface{})
	if actor == "" || !ok {
		return permanentActivity("update missing actor or object")
	}

	objectID, _ := object["id"].(string)
	objType, _ := object["type"].(string)

	if objType == "Person" || objType == "Service" || objType == "Application" {
		if objectID != actor {
			return permanentActivity("actor update object does not match actor")
		}
		// Actor update - re-fetch
		_, err := p.federator.RefreshActor(ctx, actor, ActorRefreshOptions{Force: true, EnrichCounts: false})
		return err
	}

	if isStatusObjectType(objType) && objectID != "" {
		if err := p.validateStatusObjectAuthority(actor, object); err != nil {
			return err
		}
		// Status update (edit)
		existing, err := p.store.GetStatusByURI(ctx, objectID)
		if err != nil {
			// Don't have the original - store as new, but only when it has a
			// local audience (same gate as Create).
			if !p.hasLocalAudience(ctx, actor, object) {
				slog.Debug("inbox: dropping update with no local audience", "actor", actor, "object", objectID)
				return nil
			}
			remoteAccount, err := p.federator.FetchActorLight(ctx, actor)
			if err != nil {
				return fmt.Errorf("fetch actor for update: %w", err)
			}
			status, err := p.parseRemoteStatus(ctx, object, remoteAccount)
			if err != nil {
				return fmt.Errorf("parse updated status: %w", err)
			}
			if err := p.store.CreateStatus(ctx, status); err != nil {
				return err
			}
			if objType == "Question" {
				if err := p.ensureQuestionPoll(ctx, status.ID, object); err != nil {
					return err
				}
				status.Poll, _ = p.store.GetPollForStatus(ctx, status.ID, "")
			}
			// This Update is the first time we see the object, so it carries the
			// notifications the missing Create would have produced.
			return p.notifyForRemoteStatus(ctx, status, remoteAccount, false)
		}
		if !p.statusOwnedByActor(ctx, existing, actor) {
			return permanentActivity("update actor does not own status")
		}
		remoteAccount, err := p.federator.FetchActorLight(ctx, actor)
		if err != nil {
			return fmt.Errorf("fetch actor for update: %w", err)
		}

		expected := existing.UpdatedAt
		private := existing.Visibility == models.VisibilityPrivate || existing.Visibility == models.VisibilityDirect

		// Update existing status
		newContent, summary, language := naturalLanguageFields(object)
		sensitive, _ := object["sensitive"].(bool)
		contentHTML := contentlib.SanitizeHTMLStr(newContent)
		edited := existing.ContentHTML != contentHTML || existing.Summary != summary || existing.Language != language || existing.Sensitive != sensitive
		if edited {
			now := time.Now()
			existing.ContentHTML = contentHTML
			existing.Summary = summary
			existing.Language = language
			existing.Sensitive = sensitive
			existing.EditedAt = &now
			existing.UpdatedAt = now

			if !private {
				if err := p.store.UpdateStatusContent(ctx, existing); err != nil {
					return fmt.Errorf("update status: %w", err)
				}
			}
		}
		if objType == "Question" {
			if err := p.ensureQuestionPoll(ctx, existing.ID, object); err != nil {
				return err
			}
			existing.Poll, _ = p.store.GetPollForStatus(ctx, existing.ID, "")
		}
		// An edit may add or remove hashtags and mentions; re-sync them so tag
		// timelines and counts stay accurate. Parsing without backfill avoids
		// pulling unrelated thread context on an edit.
		parsed, err := p.parseRemoteStatusInternal(ctx, object, remoteAccount, false)
		if err != nil {
			return fmt.Errorf("parse edited status for tag/mention sync: %w", err)
		}
		if err := p.syncEditedMediaDescriptions(ctx, existing.ID, parsed.MediaAttachments); err != nil {
			return err
		}
		if private {
			existing.UpdatedAt = time.Now()
			existing.Tags, existing.Mentions = parsed.Tags, parsed.Mentions
			media, err := p.store.GetMediaForStatus(ctx, existing.ID)
			if err != nil {
				return err
			}
			existing.MediaAttachments = make([]models.MediaAttachment, len(media))
			for i, attachment := range media {
				existing.MediaAttachments[i] = *attachment
			}
			if err := p.store.EditStatusWithJobs(ctx, existing, expected, nil); err != nil {
				return fmt.Errorf("update private status: %w", err)
			}
		} else {
			tags := make([]*models.Tag, len(parsed.Tags))
			for i := range parsed.Tags {
				tags[i] = &parsed.Tags[i]
			}
			if _, err := p.store.SyncStatusTags(ctx, existing.ID, tags); err != nil {
				return fmt.Errorf("sync edited status tags: %w", err)
			}
			mentions := make([]*models.Mention, len(parsed.Mentions))
			for i := range parsed.Mentions {
				mentions[i] = &parsed.Mentions[i]
			}
			if err := p.store.SyncStatusMentions(ctx, existing.ID, mentions); err != nil {
				return fmt.Errorf("sync edited status mentions: %w", err)
			}
		}
		if err := p.store.SyncStatusPreview(ctx, existing.ID, parsed.Preview); err != nil {
			return fmt.Errorf("sync edited status preview: %w", err)
		}
		existing.Mentions = parsed.Mentions
		existing.Preview = parsed.Preview
		p.invalidateStatusComponents(existing.ID)
		p.streamStatusUpdated(ctx, existing)
		return p.notifyForRemoteStatus(ctx, existing, remoteAccount, edited)
	}

	return nil
}

func (p *InboxProcessor) processLike(ctx context.Context, activity map[string]interface{}) error {
	actor, _ := activity["actor"].(string)
	object, _ := activity["object"].(string)
	activityID, _ := activity["id"].(string)

	if actor == "" || object == "" || activityID == "" || !sameOrigin(actor, activityID) {
		return permanentActivity("like missing or foreign actor, object, or id")
	}

	// Find the status. A like for a status we don't have is only worth a
	// remote fetch when the liker is followed here; otherwise it is
	// shared-inbox noise about content no local user can see.
	status, err := p.store.GetStatusByURI(ctx, object)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err != nil {
		if !p.actorFollowedLocally(ctx, actor) {
			slog.Debug("inbox: dropping like with no local audience", "actor", actor, "object", object)
			return nil
		}
		slog.Info("like for unknown status, fetching", "uri", object)
		status, err = p.FetchAndStoreRemoteStatus(ctx, object)
		if err != nil {
			if isPermanentRemoteFetchError(err) {
				slog.Info("skipping like for unavailable remote status", "uri", object, "error", err)
				return nil
			}
			return fmt.Errorf("fetch and store status for like: %w", err)
		}
	}

	// Resolve the remote actor
	remoteAccount, err := p.federator.FetchActorLight(ctx, actor)
	if err != nil {
		return fmt.Errorf("fetch actor for like: %w", err)
	}
	if !p.remoteCanViewStatus(ctx, status, remoteAccount) {
		return permanentActivity("like actor is not in status audience")
	}

	fave := &models.Favourite{AccountID: remoteAccount.ID, StatusID: status.ID, URI: activityID}
	notif, jobs, err := p.interactionNotification(ctx, "favourite", status, remoteAccount)
	if err != nil {
		return err
	}
	notified, err := p.store.CreateFavouriteWithNotification(ctx, fave, notif, jobs)
	if err != nil {
		return fmt.Errorf("create favourite: %w", err)
	}
	p.invalidateStatusComponents(status.ID)
	if notified && notif != nil {
		p.streamNotification(ctx, notif)
	}

	return nil
}

func (p *InboxProcessor) processEmojiReaction(ctx context.Context, activity map[string]interface{}) error {
	actor, _ := activity["actor"].(string)
	object := activityStreamsLink(activity["object"])
	activityID, _ := activity["id"].(string)
	if actor == "" || object == "" || activityID == "" || !sameOrigin(actor, activityID) {
		return permanentActivity("emoji reaction missing or foreign actor, object, or id")
	}

	status, err := p.store.GetStatusByURI(ctx, object)
	if err != nil {
		if !p.actorFollowedLocally(ctx, actor) {
			return nil
		}
		status, err = p.FetchAndStoreRemoteStatus(ctx, object)
		if err != nil {
			if isPermanentRemoteFetchError(err) {
				return nil
			}
			return fmt.Errorf("fetch status for emoji reaction: %w", err)
		}
	}
	remoteAccount, err := p.federator.FetchActorLight(ctx, actor)
	if err != nil {
		return fmt.Errorf("fetch actor for emoji reaction: %w", err)
	}
	if !p.remoteCanViewStatus(ctx, status, remoteAccount) {
		return permanentActivity("emoji reaction actor is not in status audience")
	}

	reactionDomain := ""
	if remoteAccount.Domain != nil {
		reactionDomain = *remoteAccount.Domain
	}
	if reactionDomain == "" {
		if actorURL, parseErr := url.Parse(actor); parseErr == nil {
			reactionDomain = strings.ToLower(actorURL.Hostname())
		}
	}
	content, key, custom, display, err := p.parseEmojiReaction(ctx, activity, reactionDomain)
	if err != nil {
		return permanentActivity("invalid emoji reaction: %v", err)
	}
	reaction := &models.EmojiReaction{URI: activityID, AccountID: remoteAccount.ID, StatusID: status.ID, Content: content, Key: key}
	if custom != nil {
		reaction.EmojiID = &custom.ID
	}
	created, err := p.store.CreateEmojiReaction(ctx, reaction)
	if err != nil {
		return fmt.Errorf("create emoji reaction: %w", err)
	}
	if !created {
		if existing, err := p.store.GetEmojiReactionByURI(ctx, activityID); err == nil &&
			(existing.AccountID != remoteAccount.ID || existing.StatusID != status.ID || existing.Key != key) {
			return permanentActivity("emoji reaction id was reused")
		}
		return nil
	}
	p.invalidateStatusComponents(status.ID)

	author, err := p.store.GetAccount(ctx, status.AccountID)
	if err != nil || author.Domain != nil || author.IsServiceActor || author.ID == remoteAccount.ID {
		return nil
	}
	notif := &models.Notification{Type: "emoji_reaction", AccountID: author.ID, FromAccountID: remoteAccount.ID, StatusID: &status.ID, Emoji: display}
	if err := p.store.UpsertNotification(ctx, notif); err != nil {
		slog.Error("failed to upsert emoji reaction notification", "error", err)
		return nil
	}
	p.streamNotification(ctx, notif)
	push.EnqueuePushNotification(ctx, p.store, notif)
	mail.EnqueueEmailNotification(ctx, p.store, p.domain, notif)
	return nil
}

func (p *InboxProcessor) parseEmojiReaction(ctx context.Context, activity map[string]interface{}, domain string) (content, key string, custom *models.CustomEmoji, display string, err error) {
	content, _ = activity["content"].(string)
	content = strings.TrimSpace(content)
	if emoji.Known(content) {
		return content, "unicode:" + content, nil, content, nil
	}
	if len(content) < 3 || content[0] != ':' || content[len(content)-1] != ':' {
		return "", "", nil, "", errors.New("content is not one known emoji or shortcode")
	}
	tags, ok := activity["tag"].([]interface{})
	if !ok || len(tags) != 1 {
		return "", "", nil, "", errors.New("custom emoji needs one tag")
	}
	tag, ok := tags[0].(map[string]interface{})
	if !ok {
		return "", "", nil, "", errors.New("custom emoji tag is not an object")
	}
	shortcode := content[1 : len(content)-1]
	if shortcode == "" || strings.ContainsAny(shortcode, ":@/\\\r\n\t ") {
		return "", "", nil, "", errors.New("custom emoji tag does not match content")
	}
	parsed, ok := ParseEmojiTag(tag, nil)
	if !ok {
		return "", "", nil, "", errors.New("malformed custom emoji tag")
	}
	if parsed.Shortcode != shortcode {
		return "", "", nil, "", errors.New("custom emoji tag does not match content")
	}
	u, err := url.Parse(parsed.RemoteURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return "", "", nil, "", errors.New("custom emoji has invalid URL")
	}
	domain = strings.ToLower(domain)
	if domain == "" {
		domain = strings.ToLower(u.Hostname())
	}
	parsed.Domain = &domain
	if err := p.store.UpsertEmoji(ctx, parsed); err != nil {
		return "", "", nil, "", err
	}
	emojis, err := p.store.GetEmojisByShortcodes(ctx, []string{shortcode}, &domain)
	if err != nil || len(emojis) != 1 {
		return "", "", nil, "", errors.New("custom emoji was not stored")
	}
	custom = emojis[0]
	return content, "custom:" + custom.ID, custom, ":" + shortcode + "@" + domain + ":", nil
}

func (p *InboxProcessor) processAnnounce(ctx context.Context, activity map[string]interface{}) error {
	actor, _ := activity["actor"].(string)
	object, _ := activity["object"].(string)
	activityID, _ := activity["id"].(string)

	if actor == "" || object == "" || activityID == "" || !sameOrigin(actor, activityID) {
		return permanentActivity("announce missing or foreign actor, object, or id")
	}
	if relay, err := p.store.GetRelayByActorURI(ctx, actor); err == nil {
		state, stateErr := p.store.GetRelayState(ctx, relay.ID)
		if stateErr != nil || state != models.RelayActive {
			slog.Debug("inbox: dropping announce from inactive relay", "actor", actor, "state", state)
			return nil
		}
		return p.processRelayAnnounce(ctx, object)
	}

	// Relevance gate: a boost matters only when the booster is followed by a
	// local user (it lands in their home) or the boosted object is ours (the
	// author must get a reblog notification). Anything else is shared-inbox
	// noise.
	relevanceStarted := time.Now()
	relevant := p.actorFollowedLocally(ctx, actor) || p.isLocalObjectURI(object)
	metrics.ObserveInboxStage("announce_relevance", time.Since(relevanceStarted))
	if !relevant {
		slog.Debug("inbox: dropping announce with no local audience", "actor", actor, "object", object)
		return nil
	}

	// Resolve the remote actor
	actorStarted := time.Now()
	remoteAccount, err := p.federator.FetchActorLight(ctx, actor)
	metrics.ObserveInboxStage("announce_actor", time.Since(actorStarted))
	if err != nil {
		return fmt.Errorf("fetch actor for announce: %w", err)
	}

	// Find the original status, fetching it if we don't have it locally
	objectStarted := time.Now()
	originalStatus, err := p.store.GetStatusByURI(ctx, object)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err != nil {
		slog.Info("announce for unknown status, fetching", "uri", object)
		originalStatus, err = p.FetchAndStoreRemoteStatus(ctx, object)
		if err != nil {
			if isPermanentRemoteFetchError(err) {
				metrics.ObserveInboxStage("announce_object", time.Since(objectStarted))
				slog.Info("skipping announce for unavailable remote status", "uri", object, "error", err)
				return nil
			}
			metrics.ObserveInboxStage("announce_object", time.Since(objectStarted))
			return fmt.Errorf("fetch and store status for announce: %w", err)
		}
	}
	metrics.ObserveInboxStage("announce_object", time.Since(objectStarted))
	if originalStatus.Visibility != models.VisibilityPublic && originalStatus.Visibility != models.VisibilityUnlisted {
		return permanentActivity("cannot announce a non-public status")
	}

	// Create boost status - only world-readable originals reach this point.
	boostVis := originalStatus.Visibility
	now := time.Now()
	boost := &models.Status{
		URI:        activityID,
		AccountID:  remoteAccount.ID,
		BoostOfID:  &originalStatus.ID,
		Visibility: boostVis,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	notif, jobs, err := p.interactionNotification(ctx, "reblog", originalStatus, remoteAccount)
	if err != nil {
		return err
	}
	notified, err := p.store.CreateBoostWithNotification(ctx, boost, notif, jobs)
	if err != nil {
		return fmt.Errorf("create boost: %w", err)
	}
	p.invalidateStatusComponents(originalStatus.ID)
	if p.stream != nil {
		boost.Account = remoteAccount
		p.stream.StatusCreated(ctx, boost)
	}
	if notified && notif != nil {
		p.streamNotification(ctx, notif)
	}

	return nil
}

func (p *InboxProcessor) interactionNotification(ctx context.Context, kind string, status *models.Status, from *models.Account) (*models.Notification, []*models.Job, error) {
	if !p.isLocalObjectURI(status.URI) {
		return nil, nil, nil
	}
	notif := &models.Notification{ID: uuid.NewString(), CreatedAt: time.Now(), Type: kind, AccountID: status.AccountID, FromAccountID: from.ID, StatusID: &status.ID}
	jobs, err := push.NotificationJobs(ctx, p.store, notif)
	if err != nil {
		return nil, nil, err
	}
	email, err := mail.NotificationJob(ctx, p.store, p.domain, notif)
	if err != nil {
		return nil, nil, err
	}
	if email != nil {
		jobs = append(jobs, email)
	}
	return notif, jobs, nil
}

func (p *InboxProcessor) processRelayAnnounce(ctx context.Context, objectURI string) error {
	fetchStarted := time.Now()
	doc, err := p.fetchStatusObject(ctx, objectURI, true)
	metrics.ObserveInboxStage("relay_fetch", time.Since(fetchStarted))
	if err != nil {
		if isPermanentRemoteFetchError(err) {
			slog.Info("inbox: dropping unavailable relayed object", "object", objectURI, "error", err)
			return nil
		}
		return err
	}
	validateStarted := time.Now()
	author, err := p.validateDereferencedStatusObject(ctx, doc, true)
	metrics.ObserveInboxStage("relay_validate", time.Since(validateStarted))
	if err != nil {
		slog.Info("inbox: dropping invalid relayed object", "object", objectURI, "error", err)
		return nil
	}
	storeStarted := time.Now()
	err = p.processCreate(ctx, map[string]interface{}{
		"type":               "Create",
		"actor":              author.ActorURI,
		"object":             doc,
		"_littlefedi_relay":  true,
		"_littlefedi_author": author,
	})
	metrics.ObserveInboxStage("relay_store", time.Since(storeStarted))
	return err
}

func (p *InboxProcessor) processBlock(ctx context.Context, activity map[string]interface{}) error {
	actor, _ := activity["actor"].(string)
	object, _ := activity["object"].(string)

	if actor == "" || object == "" {
		return permanentActivity("block missing actor or object")
	}

	remoteAccount, err := p.federator.FetchActorLight(ctx, actor)
	if err != nil {
		return fmt.Errorf("fetch actor for block: %w", err)
	}

	targetAccount, err := p.store.GetAccountByURI(ctx, object)
	if err != nil {
		slog.Info("block for unknown account", "uri", object)
		return nil
	}
	if remoteAccount.Domain == nil || targetAccount.Domain != nil {
		return permanentActivity("block must be from a remote actor to a local actor")
	}

	// Create block record
	block := &models.Block{
		AccountID:       remoteAccount.ID,
		TargetAccountID: targetAccount.ID,
	}
	if err := p.store.CreateBlock(ctx, block); err != nil {
		// Might already exist
		slog.Info("block already exists or error", "error", err)
	}

	// Remove follow relationships in both directions. A Block implicitly tears
	// down both sides, but it should still remain visible to the local user.
	follow, err := p.store.GetFollowByPair(ctx, remoteAccount.ID, targetAccount.ID)
	if err == nil {
		if err := p.store.DeleteFollow(ctx, follow.ID); err != nil {
			return err
		}
	}

	follow2, err := p.store.GetFollowByPair(ctx, targetAccount.ID, remoteAccount.ID)
	if err == nil {
		if err := p.store.DeleteFollow(ctx, follow2.ID); err != nil {
			return err
		}
	}
	p.notifyRelationship(ctx, "block", targetAccount.ID, remoteAccount)

	return nil
}

// extractObjectURIs normalises an ActivityPub "object" value (string, array of
// strings, array of objects, or a single object) into a flat list of URIs.
func extractObjectURIs(object interface{}) []string {
	var out []string
	add := func(s string) {
		if s != "" {
			out = append(out, s)
		}
	}
	switch v := object.(type) {
	case string:
		add(v)
	case map[string]interface{}:
		id, _ := v["id"].(string)
		add(id)
	case []interface{}:
		for _, item := range v {
			switch iv := item.(type) {
			case string:
				add(iv)
			case map[string]interface{}:
				id, _ := iv["id"].(string)
				add(id)
			}
		}
	}
	return out
}

// processFlag handles an inbound Flag (federated moderation report). It records a
// local Report against the reported local account/statuses and notifies admins.
func (p *InboxProcessor) processFlag(ctx context.Context, activity map[string]interface{}) error {
	actor, _ := activity["actor"].(string)
	content, _ := activity["content"].(string)
	actorURL, err := url.Parse(actor)
	if err != nil || actorURL.Scheme != "https" || actorURL.Host == "" ||
		strings.EqualFold(actorURL.Hostname(), p.domain) {
		return permanentActivity("flag missing valid remote actor")
	}

	uris := extractObjectURIs(activity["object"])
	if len(uris) == 0 {
		return permanentActivity("flag missing object")
	}

	var target *models.Account
	var statusIDs []string
	for _, uri := range uris {
		if acct, err := p.store.GetAccountByURI(ctx, uri); err == nil && acct != nil && acct.Domain == nil {
			if target == nil {
				target = acct
			}
			continue
		}
		if st, err := p.store.GetStatusByURI(ctx, uri); err == nil && st != nil {
			acct, accountErr := p.store.GetAccount(ctx, st.AccountID)
			if accountErr != nil || acct == nil || acct.Domain != nil {
				continue
			}
			if target == nil {
				target = acct
			}
			if target.ID == acct.ID {
				statusIDs = append(statusIDs, st.ID)
			}
		}
	}
	if target == nil {
		slog.Info("flag for unknown local account", "uris", uris)
		return nil
	}

	host := actorURL.Hostname()
	originDomain := &host
	// Honor the reject_reports domain-block option.
	if block, err := p.store.GetDomainBlock(ctx, host); err == nil && block != nil && block.RejectReports {
		slog.Debug("inbox: dropping flag from reject_reports domain", "domain", host)
		return nil
	}

	report := &models.Report{
		TargetAccountID: target.ID,
		StatusIDs:       strings.Join(statusIDs, ","),
		Comment:         content,
		Category:        "other",
		State:           "open",
		OriginDomain:    originDomain,
	}
	if err := p.store.CreateReport(ctx, report); err != nil {
		return fmt.Errorf("create report from flag: %w", err)
	}

	// Notify admins. The reported (local) account is always a valid FromAccountID.
	admins, _ := p.store.GetAdmins(ctx)
	for _, admin := range admins {
		notif := &models.Notification{Type: "admin.report", AccountID: admin.ID, FromAccountID: target.ID}
		if err := p.store.UpsertNotification(ctx, notif); err == nil {
			p.streamNotification(ctx, notif)
			push.EnqueuePushNotification(ctx, p.store, notif)
			mail.EnqueueEmailNotification(ctx, p.store, p.domain, notif)
		}
	}
	slog.Info("processed inbound flag", "target", target.Username, "statuses", len(statusIDs), "origin", actor)
	return nil
}

// processAdd handles Add activities (e.g., adding a status to the featured/pinned collection).
func (p *InboxProcessor) processAdd(ctx context.Context, activity map[string]interface{}) error {
	actor, _ := activity["actor"].(string)
	object, _ := activity["object"].(string)
	target, _ := activity["target"].(string)

	if actor == "" || object == "" || target == "" || !sameOrigin(actor, target) {
		return permanentActivity("add missing or foreign actor, object, or target")
	}

	// Only handle Add to featured/collections
	if !strings.Contains(target, "/collections/") {
		slog.Info("ignoring Add to non-collection target", "target", target)
		return nil
	}

	status, err := p.store.GetStatusByURI(ctx, object)
	if err != nil {
		slog.Info("Add for unknown status, fetching", "uri", object)
		status, err = p.FetchAndStoreRemoteStatus(ctx, object)
		if err != nil {
			slog.Info("failed to fetch status for Add activity", "uri", object, "error", err)
			return nil
		}
	}

	actorAccount, err := p.store.GetAccountByURI(ctx, actor)
	if err != nil || status.AccountID != actorAccount.ID {
		slog.Info("ignoring Add: status author does not match actor", "uri", object, "actor", actor)
		return nil
	}

	if err := p.store.PinStatus(ctx, status.ID); err != nil {
		return fmt.Errorf("pin status: %w", err)
	}

	slog.Info("pinned remote status", "uri", object, "status_id", status.ID)
	return nil
}

// processRemove handles Remove activities (e.g., removing a status from the featured/pinned collection).
func (p *InboxProcessor) processRemove(ctx context.Context, activity map[string]interface{}) error {
	actor, _ := activity["actor"].(string)
	object, _ := activity["object"].(string)
	target, _ := activity["target"].(string)

	if actor == "" || object == "" || target == "" || !sameOrigin(actor, target) {
		return permanentActivity("remove missing or foreign actor, object, or target")
	}

	// Only handle Remove from featured/collections
	if !strings.Contains(target, "/collections/") {
		slog.Info("ignoring Remove from non-collection target", "target", target)
		return nil
	}

	status, err := p.store.GetStatusByURI(ctx, object)
	if err != nil {
		slog.Info("Remove for unknown status", "uri", object)
		return nil
	}
	if !p.statusOwnedByActor(ctx, status, actor) {
		return permanentActivity("remove actor does not own status")
	}

	if err := p.store.UnpinStatus(ctx, status.ID); err != nil {
		return fmt.Errorf("unpin status: %w", err)
	}

	slog.Info("unpinned remote status", "uri", object, "status_id", status.ID)
	return nil
}

// SyncFeaturedCollection fetches a remote account's featured (pinned posts)
// collection and reconciles local pin state to match it. This covers pins
// made before we ever discovered the account (or while we were unreachable),
// which the live Add/Remove activity path in processAdd/processRemove can't
// see. Statuses listed but not yet known locally are fetched and stored.
func (p *InboxProcessor) SyncFeaturedCollection(ctx context.Context, account *models.Account) error {
	if account.Domain == nil || account.ActorURI == "" {
		return nil
	}

	doc, err := p.federator.FetchObject(ctx, account.ActorURI)
	if err != nil {
		return fmt.Errorf("fetch actor for featured collection: %w", err)
	}
	featuredURI := activityStreamsLink(doc["featured"])
	if featuredURI == "" {
		return nil
	}

	uris, err := p.federator.FetchCollectionURIs(ctx, featuredURI, 50)
	if err != nil {
		return fmt.Errorf("fetch featured collection: %w", err)
	}

	pinned, _ := p.store.GetPinnedStatuses(ctx, account.ID, 50)
	wasPinned := make(map[string]bool, len(pinned))
	for _, status := range pinned {
		wasPinned[status.URI] = true
	}

	current := make(map[string]bool, len(uris))
	for _, uri := range uris {
		status, err := p.store.GetStatusByURI(ctx, uri)
		if err != nil {
			status, err = p.FetchAndStoreRemoteStatus(ctx, uri)
			if err != nil {
				slog.Info("featured sync: could not fetch pinned status", "uri", uri, "error", err)
				continue
			}
		}
		if status.AccountID != account.ID {
			continue
		}
		current[status.URI] = true
		if !wasPinned[status.URI] {
			if err := p.store.PinStatus(ctx, status.ID); err != nil {
				slog.Warn("featured sync: pin failed", "uri", uri, "error", err)
			}
		}
	}

	for _, status := range pinned {
		if !current[status.URI] {
			if err := p.store.UnpinStatus(ctx, status.ID); err != nil {
				slog.Warn("featured sync: unpin failed", "uri", status.URI, "error", err)
			}
		}
	}
	return nil
}

// processMove handles Move activities (account migration).
// When a remote user moves to another instance, they send a Move activity
// to their followers. The receiving instance must:
//  1. Update its local copy of the old actor (movedTo field)
//  2. Unfollow the old account and follow the new one (if not already)
//
// If the Move targets one of our local accounts, that account must already
// advertise the old actor in alsoKnownAs.
// Move shape:
//
//	{
//	  "type": "Move",
//	  "actor": "https://old.example.com/users/alice",
//	  "object": "https://old.example.com/users/alice",
//	  "target": "https://new.example.com/users/alice"
//	}
func (p *InboxProcessor) processMove(ctx context.Context, activity map[string]interface{}) error {
	actor, _ := activity["actor"].(string)
	object, _ := activity["object"].(string)
	target, _ := activity["target"].(string)

	if actor == "" || target == "" {
		return permanentActivity("move missing actor or target")
	}

	// Validate: actor must equal object (moving yourself, not someone else)
	if object != "" && actor != object {
		slog.Warn("move: actor != object, refusing to process", "actor", actor, "object", object)
		return nil
	}

	// Resolve the old account. Refresh only when the cached copy does not yet
	// advertise this Move; this keeps tests and local caches fast while still
	// avoiding stale movedTo data.
	oldAccount, err := p.federator.FetchActorLight(ctx, actor)
	if err != nil {
		return fmt.Errorf("move: fetch old actor %s: %w", actor, err)
	}
	if !accountmigration.SameActorURI(oldAccount.MovedToURI, target) {
		oldAccount, err = p.federator.RefreshActor(ctx, actor, ActorRefreshOptions{Force: true})
		if err != nil {
			return fmt.Errorf("move: refresh old actor %s: %w", actor, err)
		}
	}

	// Resolve the destination locally first. FetchActor refreshes
	// stale remote actors, but trying it for one of our own actors would make an
	// unnecessary HTTPS request back to this instance (and can fail on private
	// deployments).
	newAccount, err := p.store.GetAccountByURI(ctx, target)
	targetIsLocal := err == nil && newAccount.Domain == nil
	if err != nil || newAccount.Domain != nil {
		newAccount, err = p.federator.FetchActorLight(ctx, target)
		if err != nil {
			return fmt.Errorf("move: fetch new actor %s: %w", target, err)
		}
		if !accountmigration.HasAccountAlias(newAccount, actor) {
			newAccount, err = p.federator.RefreshActor(ctx, target, ActorRefreshOptions{Force: true})
			if err != nil {
				return fmt.Errorf("move: refresh new actor %s: %w", target, err)
			}
		}
	}
	if err := accountmigration.VerifyMovePair(oldAccount, newAccount, true); err != nil {
		return fmt.Errorf("move: identity verification failed: %w", err)
	}

	// Update the old account's movedTo field in our local cache
	oldAccount.MovedToURI = target
	now := time.Now()
	oldAccount.MovedAt = &now
	oldAccount.FetchedAt = &now
	if err := p.store.UpdateAccount(ctx, oldAccount); err != nil {
		slog.Warn("move: failed to update old account movedTo", "error", err)
	}

	// Case 1: The target is a local account on our instance. The alias must
	// already be present; Settings exposes that setup step for move-ins.
	if targetIsLocal {
		slog.Info("move: verified alsoKnownAs on local account",
			"local_user", newAccount.Username,
			"old_actor", actor,
		)
		p.notifyRelationship(ctx, "move", newAccount.ID, oldAccount)
	}

	// In both move-in and remote-to-remote cases, migrate every local account
	// that followed the old actor. Local targets need no AP delivery; remote
	// targets receive a new Follow whose ID matches the stored follow record.
	if err := p.migrateLocalFollowers(ctx, oldAccount.ID, newAccount.ID); err != nil {
		slog.Warn("move: failed to migrate local followers", "error", err)
	}

	slog.Info("processed move", "from", actor, "to", target)
	return nil
}

// migrateLocalFollowers finds all local accounts following oldAccountID and
// migrates their follows to newAccountID.
func (p *InboxProcessor) migrateLocalFollowers(ctx context.Context, oldAccountID, newAccountID string) error {
	// Find all local accounts that follow the old account
	followers, err := p.store.GetFollowersToMigrate(ctx, oldAccountID, 0) // 0 = no limit
	if err != nil {
		return fmt.Errorf("migrateLocalFollowers: get followers: %w", err)
	}

	slog.Info("move: migrating local followers", "count", len(followers), "old", oldAccountID, "new", newAccountID)

	for _, follower := range followers {
		if follower.Domain != nil {
			continue // only process local accounts
		}
		existingFollow, _ := p.store.GetFollowByPair(ctx, follower.ID, newAccountID)
		if err := p.store.MigrateFollower(ctx, follower.ID, oldAccountID, newAccountID); err != nil {
			slog.Warn("move: failed to migrate follower",
				"follower_id", follower.ID,
				"follower_username", follower.Username,
				"error", err,
			)
			continue
		}
		newAccount, err := p.store.GetAccount(ctx, newAccountID)
		if err == nil && existingFollow == nil && newAccount.Domain != nil {
			if follow, err := p.store.GetFollowByPair(ctx, follower.ID, newAccountID); err == nil {
				if err := SendFollowActivity(ctx, p.federator, follower, newAccount, follow.URI); err != nil {
					slog.Warn("move: failed to send migrated Follow", "follower_id", follower.ID, "error", err)
				}
			}
		}
		oldAccount, err := p.store.GetAccount(ctx, oldAccountID)
		if err == nil {
			p.notifyRelationship(ctx, "move", follower.ID, oldAccount)
		}
	}

	return nil
}

// Helper methods

func (p *InboxProcessor) sendAccept(ctx context.Context, localAccount, remoteAccount *models.Account, followActivityID string) error {
	acceptID := localAccount.ActorURI + "#accepts/" + uuid.NewSHA1(uuid.NameSpaceURL, []byte(followActivityID)).String()
	accept := map[string]interface{}{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       acceptID,
		"type":     "Accept",
		"actor":    localAccount.ActorURI,
		"to":       []string{remoteAccount.ActorURI},
		"object": map[string]interface{}{
			"id":     followActivityID,
			"type":   "Follow",
			"actor":  remoteAccount.ActorURI,
			"object": localAccount.ActorURI,
		},
	}

	// Deliver to remote actor's inbox
	inbox := remoteAccount.InboxURI
	if remoteAccount.SharedInboxURI != nil && *remoteAccount.SharedInboxURI != "" {
		inbox = *remoteAccount.SharedInboxURI
	}
	if inbox == "" {
		return fmt.Errorf("remote account %s has no inbox URI", remoteAccount.ActorURI)
	}

	if localAccount.IsServiceActor {
		return EnqueueDelivery(ctx, p.store, inbox, accept)
	}
	return p.federator.Deliver(ctx, inbox, accept)
}

func (p *InboxProcessor) sendReject(ctx context.Context, localAccount, remoteAccount *models.Account, followActivityID string) error {
	reject := map[string]interface{}{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       localAccount.ActorURI + "#rejects/" + uuid.NewSHA1(uuid.NameSpaceURL, []byte(followActivityID)).String(),
		"type":     "Reject",
		"actor":    localAccount.ActorURI,
		"to":       []string{remoteAccount.ActorURI},
		"object": map[string]interface{}{
			"id": followActivityID, "type": "Follow", "actor": remoteAccount.ActorURI, "object": localAccount.ActorURI,
		},
	}
	inbox := remoteAccount.InboxURI
	if inbox == "" {
		return fmt.Errorf("remote account %s has no inbox URI", remoteAccount.ActorURI)
	}
	return EnqueueDelivery(ctx, p.store, inbox, reject)
}

func (p *InboxProcessor) parseRemoteStatus(ctx context.Context, object map[string]interface{}, account *models.Account) (*models.Status, error) {
	return p.parseRemoteStatusInternal(ctx, object, account, true)
}

func (p *InboxProcessor) parseRemoteStatusInternal(ctx context.Context, object map[string]interface{}, account *models.Account, allowBackfill bool) (*models.Status, error) {
	objectID, _ := object["id"].(string)
	content, summary, language := naturalLanguageFields(object)
	sensitive, _ := object["sensitive"].(bool)
	published, _ := object["published"].(string)

	if objectID == "" {
		return nil, permanentActivity("remote status missing id")
	}
	// A photo posted without a caption sends content as the empty string, and
	// contentMap holds the same empty string. Those are ordinary Mastodon and
	// Pixelfed posts, and rejecting on empty content dropped all of them, boosts
	// included, while the attachment they were made for parses fine below.
	if content == "" && !hasParseableAttachment(object) {
		return nil, permanentActivity("remote status missing id or content")
	}

	pubTime := time.Now()
	if published != "" {
		if t, err := time.Parse(time.RFC3339, published); err == nil {
			pubTime = t
		}
	}

	// Determine visibility from addressing (Mastodon-compatible)
	// public:  Public in to
	// unlisted: Public in cc but not in to
	// private: followers in to/cc, no Public anywhere
	// direct:  only specific user URIs in to
	toStrs := toStringSlice(object["to"])
	ccStrs := toStringSlice(object["cc"])

	toPublic := false
	ccPublic := false
	toFollowers := false
	ccFollowers := false
	hasSpecific := false
	audienceURIs := make(map[string]bool)
	followersURI := account.FollowersURI

	for _, s := range toStrs {
		if s == "https://www.w3.org/ns/activitystreams#Public" {
			toPublic = true
		} else if followersURI != "" && s == followersURI {
			toFollowers = true
		} else if s != "" {
			hasSpecific = true
			audienceURIs[s] = true
		}
	}
	for _, s := range ccStrs {
		if s == "https://www.w3.org/ns/activitystreams#Public" {
			ccPublic = true
		} else if followersURI != "" && s == followersURI {
			ccFollowers = true
		} else if s != "" {
			hasSpecific = true
			audienceURIs[s] = true
		}
	}

	var visibility models.Visibility
	if toPublic {
		visibility = models.VisibilityPublic
	} else if ccPublic {
		visibility = models.VisibilityUnlisted
	} else if toFollowers || ccFollowers {
		visibility = models.VisibilityPrivate
	} else if hasSpecific {
		visibility = models.VisibilityDirect
	} else {
		return nil, permanentActivity("remote status has no audience")
	}

	// Parse inReplyTo
	var inReplyToURI *string
	var inReplyToID *string
	if irt, ok := object["inReplyTo"].(string); ok && irt != "" {
		inReplyToURI = &irt

		// Try to fetch the parent if we don't have it
		parent, err := p.store.GetStatusByURI(ctx, irt)
		if err != nil {
			if allowBackfill {
				// Backfill: fetch parent (with concurrency cap)
				select {
				case p.backfillSlots <- struct{}{}:
					go func() {
						defer func() { <-p.backfillSlots }()
						backfillCtx, cancel := context.WithTimeout(p.backfillCtx, 30*time.Second)
						defer cancel()
						if _, err := p.FetchAndStoreRemoteStatus(backfillCtx, irt); err != nil {
							slog.Warn("failed to backfill parent", "uri", irt, "error", err)
						}
					}()
				default:
					slog.Warn("backfill semaphore full, skipping", "uri", irt)
				}
			}
		} else {
			inReplyToID = &parent.ID
		}
	}

	// Parse quote (FEP-044f with Fedibird/Misskey aliases). The quoting status
	// references the quoted object; resolve it locally, backfilling if needed.
	var quoteOfURI *string
	var quoteOfID *string
	var quoteApprovalURI *string
	var quoteState string
	if qURI := firstQuoteLink(object); qURI != "" {
		quoteOfURI = &qURI
		if auth := activityStreamsLink(object["quoteAuthorization"]); auth != "" {
			quoteApprovalURI = &auth
			quoteState = "accepted"
		} else {
			quoteState = "pending"
		}
		if quoted, err := p.store.GetStatusByURI(ctx, qURI); err == nil {
			quoteOfID = &quoted.ID
		} else if allowBackfill {
			select {
			case p.backfillSlots <- struct{}{}:
				go func() {
					defer func() { <-p.backfillSlots }()
					bgCtx, cancel := context.WithTimeout(p.backfillCtx, 30*time.Second)
					defer cancel()
					if _, err := p.FetchAndStoreRemoteStatus(bgCtx, qURI); err != nil {
						slog.Warn("failed to backfill quoted status", "uri", qURI, "error", err)
					}
				}()
			default:
				slog.Warn("backfill semaphore full, skipping quoted status", "uri", qURI)
			}
		}
	}

	// Parse tags/mentions
	var mentions []*models.Mention
	var tags []*models.Tag
	var statusTags []*models.StatusTag

	if tagList, ok := object["tag"].([]interface{}); ok {
		for _, t := range tagList {
			tag, ok := t.(map[string]interface{})
			if !ok {
				continue
			}
			tagType, _ := tag["type"].(string)

			switch tagType {
			case "Mention":
				href, _ := tag["href"].(string)
				name, _ := tag["name"].(string)
				if href != "" {
					if (visibility == models.VisibilityPrivate || visibility == models.VisibilityDirect) && !audienceURIs[href] {
						continue
					}
					mentionedAccount, err := p.store.GetAccountByURI(ctx, href)
					if err != nil {
						// Try to fetch
						continue
					}
					mention := &models.Mention{
						AccountID: mentionedAccount.ID,
						Username:  mentionedAccount.Username,
						URL:       href,
						Acct:      name,
					}
					mentions = append(mentions, mention)
				}

			case "Hashtag":
				tagName, _ := tag["name"].(string)
				tagName = strings.TrimPrefix(tagName, "#")
				if tagName != "" {
					tagObj, err := p.store.GetOrCreateTag(ctx, tagName)
					if err == nil {
						tags = append(tags, tagObj)
						statusTags = append(statusTags, &models.StatusTag{
							TagID: tagObj.ID,
						})
					}
				}
			}
		}

		// Custom emojis referenced by the post. Stored globally keyed by
		// (shortcode, domain); the post's content carries the `:shortcode:` text
		// that resolves to these rows at render time.
		upsertEmojiTags(ctx, p.store, object["tag"], account.Domain)
	}
	if visibility == models.VisibilityPrivate || visibility == models.VisibilityDirect {
		seen := make(map[string]bool, len(mentions))
		for _, mention := range mentions {
			seen[mention.AccountID] = true
		}
		for uri := range audienceURIs {
			mentionedAccount, err := p.store.GetAccountByURI(ctx, uri)
			if err != nil || seen[mentionedAccount.ID] {
				continue
			}
			acct := mentionedAccount.Username
			if mentionedAccount.Domain != nil {
				acct += "@" + *mentionedAccount.Domain
			}
			mentions = append(mentions, &models.Mention{
				AccountID: mentionedAccount.ID,
				Username:  mentionedAccount.Username,
				URL:       uri,
				Acct:      acct,
			})
			seen[mentionedAccount.ID] = true
		}
	}

	// Parse media attachments, plus any FEP-8967 link preview travelling in
	// the same property. Only the first preview is kept: the Mastodon API has one
	// card per status, and picking the first is what a reader sees anyway.
	var mediaAttachments []*models.MediaAttachment
	var preview *models.StatusPreview
	for _, att := range objectAttachmentEntries(object) {
		if card, ok := opengraph.PreviewFromLinkAttachment(att); ok {
			if preview == nil {
				preview = card
			}
			continue
		}
		if media, ok := ParseMediaAttachment(att, account.ID); ok {
			mediaAttachments = append(mediaAttachments, media)
		}
	}

	now := time.Now()
	status := &models.Status{
		URI:              objectID,
		URL:              objectID,
		AccountID:        account.ID,
		ContentHTML:      contentlib.SanitizeHTMLStr(content),
		Summary:          summary,
		Language:         language,
		Sensitive:        sensitive,
		Visibility:       visibility,
		InReplyToID:      inReplyToID,
		InReplyToURI:     inReplyToURI,
		QuoteOfID:        quoteOfID,
		QuoteOfURI:       quoteOfURI,
		QuoteState:       quoteState,
		QuoteApprovalURI: quoteApprovalURI,
		CreatedAt:        pubTime,
		UpdatedAt:        now,
		Preview:          preview,
		MediaAttachments: convertMediaAttachments(mediaAttachments),
		Mentions:         convertMentions(mentions),
		Tags:             convertTags(tags),
		FavouritesCount:  inlineCollectionTotal(object["likes"]),
		ReblogsCount:     inlineCollectionTotal(object["shares"]),
		RepliesCount:     inlineCollectionTotal(object["replies"]),
	}

	return status, nil
}

// inlineCollectionTotal reads totalItems off an inline AS2 collection summary
// (e.g. a Note's "likes"/"shares"/"replies" property). It never dereferences a
// bare collection IRI - this runs on the synchronous inbox parse path, so a
// remote fetch here would hold up activity processing. This only seeds the
// counter at first-fetch time; it is not kept in sync afterwards.
func inlineCollectionTotal(value interface{}) int {
	doc, ok := value.(map[string]interface{})
	if !ok {
		return 0
	}
	total, _ := interactionInt(doc["totalItems"])
	return total
}

// firstQuoteLink extracts the quoted object's URI from a Note, accepting the
// FEP-044f `quote` property and the Fedibird `quoteUri` / Misskey
// `_misskey_quote` aliases, in that order of preference.
func firstQuoteLink(object map[string]interface{}) string {
	for _, key := range []string{"quote", "quoteUri", "_misskey_quote"} {
		if uri := activityStreamsLink(object[key]); uri != "" {
			return uri
		}
	}
	return ""
}

// syncEditedMediaDescriptions carries alt text changes from a remote edit
// onto the attachments we already stored, matched by their origin URL. People
// often add alt text after posting, and without this the post stays without
// it here (and stays hidden from anyone filtering out media without alt).
// Attachments added or removed by the edit are left alone.
func (p *InboxProcessor) syncEditedMediaDescriptions(ctx context.Context, statusID string, edited []models.MediaAttachment) error {
	if len(edited) == 0 {
		return nil
	}
	descriptions := make(map[string]string, len(edited))
	for _, m := range edited {
		descriptions[m.RemoteURL] = m.Description
	}
	stored, err := p.store.GetMediaForStatus(ctx, statusID)
	if err != nil {
		return fmt.Errorf("load media for edited status: %w", err)
	}
	for _, m := range stored {
		description, ok := descriptions[m.RemoteURL]
		if !ok || m.RemoteURL == "" || description == m.Description {
			continue
		}
		m.Description = description
		if err := p.store.UpdateMediaAttachment(ctx, m); err != nil {
			return fmt.Errorf("update media description: %w", err)
		}
	}
	return nil
}

func convertMediaAttachments(in []*models.MediaAttachment) []models.MediaAttachment {
	out := make([]models.MediaAttachment, len(in))
	for i, m := range in {
		if m != nil {
			out[i] = *m
		}
	}
	return out
}

func convertMentions(in []*models.Mention) []models.Mention {
	out := make([]models.Mention, len(in))
	for i, m := range in {
		if m != nil {
			out[i] = *m
		}
	}
	return out
}

// toStringSlice converts a to/cc field (which can be a JSON array or a single string)
// into a string slice.
func toStringSlice(v interface{}) []string {
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case []interface{}:
		out := make([]string, 0, len(val))
		for _, item := range val {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{val}
	default:
		return nil
	}
}

func convertTags(in []*models.Tag) []models.Tag {
	out := make([]models.Tag, len(in))
	for i, t := range in {
		if t != nil {
			out[i] = *t
		}
	}
	return out
}

// FetchAndStoreRemoteStatus fetches a remote object by URI, parses it as a status,
// and stores it locally. Returns the stored status. If the object is already known
// (e.g. from a concurrent backfill), returns the existing status. Exported so
// callers outside the inbox path (e.g. the web UI's search-by-URL resolve) can
// reuse the same validated fetch/parse/link logic instead of duplicating it.
// Permalinks must be confirmed by the canonical object before they are stored.
func (p *InboxProcessor) FetchAndStoreRemoteStatus(ctx context.Context, objectURI string) (*models.Status, error) {
	return p.fetchAndStoreRemoteStatus(ctx, objectURI, true)
}

// ResolveRemoteStatusByURL resolves a user-supplied URL without requiring the
// canonical object to link back to it. The object is still read from its
// canonical id, and type and author checks still apply.
func (p *InboxProcessor) ResolveRemoteStatusByURL(ctx context.Context, url string) (*models.Status, error) {
	return p.fetchAndStoreRemoteStatus(ctx, url, false)
}

func (p *InboxProcessor) fetchAndStoreRemoteStatus(ctx context.Context, objectURI string, requireCanonical bool) (*models.Status, error) {
	doc, err := p.fetchStatusObject(ctx, objectURI, requireCanonical)
	if err != nil {
		return nil, fmt.Errorf("fetch remote object %s: %w", objectURI, err)
	}

	objType, _ := doc["type"].(string)
	author, err := p.validateDereferencedStatusObject(ctx, doc, false)
	if err != nil {
		return nil, err
	}

	status, err := p.parseRemoteStatus(ctx, doc, author)
	if err != nil {
		return nil, fmt.Errorf("parse remote status: %w", err)
	}

	// Check again - a concurrent fetch may have stored it
	existing, err := p.store.GetStatusByURI(ctx, status.URI)
	if err == nil {
		slog.Info("status already stored (concurrent backfill)", "uri", status.URI, "id", existing.ID)
		return existing, nil
	}

	if err := p.store.CreateStatus(ctx, status); err != nil {
		return nil, fmt.Errorf("store fetched status: %w", err)
	}
	if objType == "Question" {
		if err := p.ensureQuestionPoll(ctx, status.ID, doc); err != nil {
			return nil, err
		}
		status.Poll, _ = p.store.GetPollForStatus(ctx, status.ID, "")
	}
	_ = p.store.LinkStatusRepliesByURI(ctx, status.URI, status.ID)
	// Connect any earlier-seen quoting statuses now that this object exists.
	_ = p.store.LinkStatusQuotesByURI(ctx, status.URI, status.ID)
	if status.InReplyToURI != nil {
		if parent, err := p.store.GetStatusByURI(ctx, *status.InReplyToURI); err == nil {
			_ = p.store.LinkStatusRepliesByURI(ctx, parent.URI, parent.ID)
		}
	}

	slog.Info("backfilled remote status", "uri", status.URI, "type", objType)
	return status, nil
}

func (p *InboxProcessor) fetchStatusObject(ctx context.Context, requested string, requireCanonical bool) (map[string]interface{}, error) {
	doc, err := p.federator.FetchObject(ctx, requested)
	if err != nil {
		return nil, err
	}
	objectID, _ := doc["id"].(string)
	if objectID == "" {
		return nil, permanentActivity("fetched object has no id")
	}
	if objectID == requested {
		return doc, nil
	}
	if requireCanonical && (!sameOrigin(requested, objectID) || activityStreamsLink(doc["url"]) != requested) {
		return nil, permanentActivity("fetched object id %q does not match %q", objectID, requested)
	}

	// Any server can claim any id, so only trust what the id's own server returns.
	doc, err = p.federator.FetchObject(ctx, objectID)
	if err != nil {
		return nil, err
	}
	if doc["id"] != objectID {
		return nil, permanentActivity("canonical object %q returned a different id", objectID)
	}
	if requireCanonical && activityStreamsLink(doc["url"]) != requested {
		return nil, permanentActivity("canonical object %q does not confirm permalink %q", objectID, requested)
	}
	return doc, nil
}

func (p *InboxProcessor) validateDereferencedStatusObject(ctx context.Context, doc map[string]interface{}, requirePublic bool) (*models.Account, error) {
	objectID, _ := doc["id"].(string)
	if objectID == "" {
		return nil, permanentActivity("fetched object has no id")
	}
	objectURL, err := url.Parse(objectID)
	if err != nil || objectURL.Scheme != "https" || objectURL.Host == "" || p.isLocalObjectURI(objectID) {
		return nil, permanentActivity("remote object must have a non-local HTTPS id")
	}
	objType, _ := doc["type"].(string)
	if !isStatusObjectType(objType) {
		return nil, permanentActivity("unexpected remote object type: %s", objType)
	}
	authorURI := activityStreamsLink(doc["attributedTo"])
	authorURL, err := url.Parse(authorURI)
	if err != nil || authorURL.Scheme != "https" || authorURL.Host == "" {
		return nil, permanentActivity("remote object has no HTTPS attributedTo")
	}
	if !strings.EqualFold(objectURL.Host, authorURL.Host) {
		return nil, permanentActivity("remote object and author origins differ")
	}
	objectHost := objectURL.Hostname()
	if err := p.federator.EnsureInstanceExists(ctx, objectHost); err != nil {
		slog.Warn("inbox: failed to ensure dereferenced object instance", "domain", objectHost, "error", err)
	}
	if hostInboundBlocked(ctx, p.store, objectHost) {
		return nil, permanentActivity("remote object origin is blocked or suspended")
	}
	if requirePublic {
		public := false
		for _, audience := range toStringSlice(doc["to"]) {
			if audience == "https://www.w3.org/ns/activitystreams#Public" {
				public = true
				break
			}
		}
		if !public {
			return nil, permanentActivity("relayed object is not public")
		}
	}
	author, err := p.federator.FetchActorLight(ctx, authorURI)
	if err != nil {
		return nil, fmt.Errorf("fetch author %s: %w", authorURI, err)
	}
	if author.Domain == nil || author.SuspendedAt != nil || author.ModerationState == "suspended" {
		return nil, permanentActivity("remote author is local or suspended")
	}
	canonical, err := url.Parse(author.ActorURI)
	if err != nil || !strings.EqualFold(canonical.Host, objectURL.Host) {
		return nil, permanentActivity("canonical author origin differs from object")
	}
	return author, nil
}

func isStatusObjectType(objectType string) bool {
	switch objectType {
	case "Note", "Article", "Page", "Question":
		return true
	default:
		return false
	}
}

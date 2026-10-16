package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/metrics"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/queue"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// DeliveryHandler handles outbound delivery of activities.
type DeliveryHandler struct {
	store     storage.Storage
	federator *Federator
}

// NewDeliveryHandler creates a new delivery handler.
func NewDeliveryHandler(store storage.Storage, federator *Federator) *DeliveryHandler {
	return &DeliveryHandler{
		store:     store,
		federator: federator,
	}
}

// DeliverToFollowers sends an activity to all followers of an account.
func (d *DeliveryHandler) DeliverToFollowers(ctx context.Context, activity map[string]interface{}, accountID string) error {
	body, err := json.Marshal(activity)
	if err != nil {
		return err
	}
	return ExpandFollowerDelivery(ctx, d.store, accountID, body, "")
}

// ExpandFollowerDelivery is shared by queued fan-out and direct callers.
func ExpandFollowerDelivery(ctx context.Context, store storage.Storage, accountID string, activityJSON json.RawMessage, extraInbox string) error {
	jobs, err := PrepareFollowerDeliveries(ctx, store, accountID, activityJSON, extraInbox)
	if err != nil {
		return err
	}
	return store.EnqueueJobs(ctx, jobs)
}

// PrepareFollowerDeliveries shares signing and destination selection with atomic edits.
func PrepareFollowerDeliveries(ctx context.Context, store storage.Storage, accountID string, activityJSON json.RawMessage, extraInboxes ...string) ([]*models.Job, error) {
	inboxes, err := store.GetFollowerDestinations(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("get follower destinations: %w", err)
	}

	for _, inbox := range extraInboxes {
		if inbox != "" {
			inboxes = append(inboxes, inbox)
		}
	}
	return PrepareInboxDeliveries(ctx, store, accountID, activityJSON, inboxes)
}

// PrepareInboxDeliveries applies the same signing, deduplication and destination
// policy to an explicit audience, including direct-message deletion.
func PrepareInboxDeliveries(ctx context.Context, store storage.Storage, accountID string, activityJSON json.RawMessage, inboxes []string) ([]*models.Job, error) {
	if len(inboxes) == 0 {
		slog.Debug("no followers to deliver to", "account_id", accountID)
		return nil, nil
	}

	// Enqueue delivery jobs. Attach an Object Integrity Proof (FEP-8b32) when
	// enabled and the actor has an Ed25519 key; the proof is durable because
	// the signed JSON is what the queue stores and later POSTs verbatim.
	var activity map[string]interface{}
	if err := json.Unmarshal(activityJSON, &activity); err != nil {
		return nil, err
	}
	if signed, err := maybeSignWithOIP(ctx, store, activity); err != nil {
		slog.Warn("delivery: OIP signing failed, falling back to HTTP signature", "account_id", accountID, "error", err)
	} else {
		activity = signed
	}
	activityJSON, err := json.Marshal(activity)
	if err != nil {
		return nil, fmt.Errorf("marshal activity: %w", err)
	}

	// Circuit breaker, resolved once per unique host: many followers share an
	// instance, and each reachability check is a storage lookup.
	hostReachable := make(map[string]bool)
	eligible := make([]string, 0, len(inboxes))
	seen := make(map[string]bool, len(inboxes))
	for _, inbox := range inboxes {
		if seen[inbox] {
			continue
		}
		seen[inbox] = true
		if inbox == "" {
			slog.Warn("skipping delivery to empty inbox", "account_id", accountID)
			continue
		}
		parsed, err := url.Parse(inbox)
		if err != nil || parsed.Hostname() == "" {
			return nil, fmt.Errorf("invalid delivery inbox %q", inbox)
		}
		{
			host := parsed.Hostname()
			allowed, reason, err := store.FederationDomainAllowed(ctx, host)
			if err != nil {
				return nil, err
			}
			if !allowed {
				metrics.IncrementFederationPolicyRejected("outbound", reason)
				slog.Info("skipping delivery blocked by federation policy", "domain", host)
				continue
			}
			reachable, ok := hostReachable[host]
			if !ok {
				var err error
				reachable, err = store.IsInstanceReachable(ctx, host)
				if err != nil {
					slog.Warn("failed to check instance reachability", "domain", host, "error", err)
					reachable = true // fail open, matching the previous behavior
				}
				hostReachable[host] = reachable
			}
			if !reachable {
				slog.Info("skipping delivery to unreachable instance", "domain", host, "inbox", inbox)
				continue
			}
		}
		eligible = append(eligible, inbox)
	}

	payloadRef := ""
	if len(eligible) > 1 {
		if id, err := store.CreateJobPayload(ctx, string(activityJSON)); err == nil {
			payloadRef = id
		} else {
			slog.Warn("shared delivery payload store failed, embedding inline", "error", err)
		}
	}
	jobs := make([]*models.Job, 0, len(eligible))
	for _, inbox := range eligible {
		payload := deliveryPayload{Inbox: inbox, PayloadRef: payloadRef}
		if payloadRef == "" {
			payload.Activity = activityJSON
		}
		payloadJSON, _ := json.Marshal(payload)

		jobs = append(jobs, &models.Job{
			Kind:        "delivery",
			Payload:     string(payloadJSON),
			RunAt:       time.Now(),
			MaxAttempts: models.DeliveryMaxAttempts,
		})
	}

	return jobs, nil
}

// ProcessDelivery processes a single delivery job.
func (d *DeliveryHandler) ProcessDelivery(ctx context.Context, payload string) error {
	prepareStarted := time.Now()
	var dp deliveryPayload
	if err := json.Unmarshal([]byte(payload), &dp); err != nil {
		return fmt.Errorf("unmarshal delivery payload: %w", err)
	}

	if len(dp.Activity) == 0 && dp.PayloadRef != "" {
		body, err := d.store.GetJobPayload(ctx, dp.PayloadRef)
		if err != nil {
			return fmt.Errorf("load shared delivery payload %s: %w", dp.PayloadRef, err)
		}
		dp.Activity = json.RawMessage(body)
	}

	// Only the actor field is needed (signing key selection); the activity
	// bytes are delivered as-is, so the full unmarshal + re-marshal per
	// destination inbox is skipped. A missing/invalid actor falls back to the
	// instance signer, matching the old failed-type-assert behavior.
	var actorField struct {
		Actor string `json:"actor"`
	}
	_ = json.Unmarshal(dp.Activity, &actorField)
	metrics.ObserveDeliveryStage("prepare", time.Since(prepareStarted))

	if dp.Inbox == "" {
		slog.Warn("delivery: empty inbox, dropping job")
		return nil
	}

	// Recheck policy at dequeue time, then ensure the target has an Instance row.
	eligibilityStarted := time.Now()
	if parsed, err := url.Parse(dp.Inbox); err == nil && parsed.Hostname() != "" {
		host := parsed.Hostname()
		if hostDeliveryBlocked(ctx, d.store, host) {
			metrics.ObserveDeliveryStage("eligibility", time.Since(eligibilityStarted))
			slog.Info("delivery: skipping suspended host", "domain", host, "inbox", dp.Inbox)
			return nil
		}
		if err := d.federator.EnsureInstanceExists(ctx, host); err != nil {
			slog.Warn("delivery: failed to ensure instance record", "domain", host, "error", err)
		}
	}
	metrics.ObserveDeliveryStage("eligibility", time.Since(eligibilityStarted))

	if err := d.federator.DeliverRaw(ctx, dp.Inbox, dp.Activity, actorField.Actor); err != nil {
		// Being preempted is not a delivery failure: the queue puts the job
		// back without spending an attempt, so logging it at warn level only
		// buries the real failures under foreground traffic.
		if ctx.Err() != nil {
			slog.Debug("delivery yielded to foreground", "inbox", dp.Inbox, "error", err)
		} else {
			slog.Warn("delivery failed", "inbox", dp.Inbox, "error", err)
		}
		var statusErr *deliveryStatusError
		if errors.As(err, &statusErr) && statusErr.permanent() {
			return queue.Permanent(err)
		}
		if errors.Is(err, storage.ErrFederationBlocked) {
			return queue.Permanent(err)
		}
		if errors.Is(err, errDeliveryRedirect) {
			return queue.Permanent(err)
		}
		return err
	}

	return nil
}

// ProcessSelfDestructDelivery bypasses reachability and federation
// policy. A shutdown notice is the exception: every peer known at the start gets
// the queue's full retry budget.
func (d *DeliveryHandler) ProcessSelfDestructDelivery(ctx context.Context, payload string) error {
	var dp deliveryPayload
	if err := json.Unmarshal([]byte(payload), &dp); err != nil {
		return fmt.Errorf("unmarshal self-destruct delivery payload: %w", err)
	}
	if dp.Inbox == "" {
		return fmt.Errorf("self-destruct delivery inbox is empty")
	}
	var actor struct {
		Actor string `json:"actor"`
	}
	if err := json.Unmarshal(dp.Activity, &actor); err != nil {
		return fmt.Errorf("unmarshal self-destruct activity: %w", err)
	}
	if err := d.federator.deliverRaw(ctx, dp.Inbox, dp.Activity, actor.Actor, true); err != nil {
		slog.Warn("self-destruct delivery failed", "inbox", dp.Inbox, "error", err)
		return err
	}
	return nil
}

type deliveryPayload struct {
	Inbox    string          `json:"inbox"`
	Activity json.RawMessage `json:"activity,omitempty"`
	// PayloadRef points at a shared job_payloads row holding the activity
	// JSON. Fan-out writes the body once and each per-inbox job carries only
	// this reference, instead of duplicating a multi-KB activity into every
	// job row (follower-count × body WAL writes per post).
	PayloadRef string `json:"payload_ref,omitempty"`
}

// EnqueueDelivery durably queues one activity for one inbox. It is used for
// relay handshakes, where a CLI process must be free to exit immediately.
func EnqueueDelivery(ctx context.Context, store storage.Storage, inbox string, activity map[string]interface{}) error {
	job, err := PrepareDelivery(ctx, store, inbox, activity)
	if err != nil {
		return err
	}
	return store.EnqueueJob(ctx, job)
}

// PrepareDelivery signs and validates before a caller opens its transaction.
func PrepareDelivery(ctx context.Context, store storage.Storage, inbox string, activity map[string]interface{}) (*models.Job, error) {
	if inbox == "" {
		return nil, fmt.Errorf("delivery inbox is empty")
	}
	if parsed, err := url.Parse(inbox); err != nil || parsed.Hostname() == "" {
		return nil, fmt.Errorf("invalid delivery inbox %q", inbox)
	} else if allowed, reason, err := store.FederationDomainAllowed(ctx, parsed.Hostname()); err != nil {
		return nil, err
	} else if !allowed {
		return nil, &FederationPolicyError{Domain: parsed.Hostname(), Reason: reason}
	}
	if signed, err := maybeSignWithOIP(ctx, store, activity); err != nil {
		slog.Warn("delivery: OIP signing failed, falling back to HTTP signature", "inbox", inbox, "error", err)
	} else {
		activity = signed
	}
	activityJSON, err := json.Marshal(activity)
	if err != nil {
		return nil, fmt.Errorf("marshal activity: %w", err)
	}
	payloadJSON, err := json.Marshal(deliveryPayload{Inbox: inbox, Activity: activityJSON})
	if err != nil {
		return nil, fmt.Errorf("marshal delivery payload: %w", err)
	}
	return &models.Job{
		Kind:        "delivery",
		Payload:     string(payloadJSON),
		RunAt:       time.Now(),
		MaxAttempts: models.DeliveryMaxAttempts,
		State:       "queued",
	}, nil
}

// SendFollowActivity sends a Follow activity to a remote actor.
func SendFollowActivity(ctx context.Context, federator *Federator, localAccount *models.Account, targetAccount *models.Account, followID string) error {
	if followID == "" {
		followID = fmt.Sprintf("%s#follows/%s", localAccount.ActorURI, targetAccount.ID)
	}

	activity := map[string]interface{}{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       followID,
		"type":     "Follow",
		"actor":    localAccount.ActorURI,
		"object":   targetAccount.ActorURI,
	}

	inbox := targetAccount.InboxURI
	if targetAccount.SharedInboxURI != nil && *targetAccount.SharedInboxURI != "" {
		inbox = *targetAccount.SharedInboxURI
	}

	if inbox == "" {
		return fmt.Errorf("target account %s has no inbox URI", targetAccount.ActorURI)
	}

	slog.Info("sending follow", "target", targetAccount.ActorURI, "inbox", inbox)
	return federator.Deliver(ctx, inbox, activity)
}

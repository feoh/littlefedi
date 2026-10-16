package federation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// CollectionSyncJobKind is the queue kind for asynchronous follower-list
// reconciliation (see CollectionSyncer.ProcessJob).
const CollectionSyncJobKind = "collection_sync"

// collectionSyncMaxFollowers caps how many URIs FetchCollectionURIs will read
// from a followers_synchronization endpoint. The collection is already
// filtered to our domain by the remote server, so this is generous headroom,
// not a realistic ceiling. A var (not const) only so tests can shrink it
// instead of generating tens of thousands of fixture accounts.
//
// If the fetch hits this cap the collection is treated as
// incomplete and the destructive (unfollow) half of reconciliation is
// skipped that round; raise the cap if a real instance ever legitimately
// exceeds it.
var collectionSyncMaxFollowers = 20000

type collectionSyncPayload struct {
	AccountID string `json:"account_id"`
	URL       string `json:"url"`
}

// localFollowersDigestHex computes Mastodon's local_followers_hash: the XOR
// of SHA-256(actor URI) over every local account with an accepted follow of
// targetAccountID, hex-encoded. This must match
// Account#local_followers_hash in Mastodon's app/models/concerns/account/interactions.rb
// byte-for-byte or every comparison mismatches.
func localFollowersDigestHex(ctx context.Context, store storage.Storage, targetAccountID string) (string, error) {
	followers, err := store.ListLocalFollowers(ctx, targetAccountID)
	if err != nil {
		return "", err
	}
	var digest [32]byte
	for _, a := range followers {
		h := sha256.Sum256([]byte(a.ActorURI))
		for i := range digest {
			digest[i] ^= h[i]
		}
	}
	return hex.EncodeToString(digest[:]), nil
}

// evaluateCollectionSync is the synchronous half of Collection-Synchronization
// handling, run inline in the inbox request path (mirrors Mastodon's
// PrepareFollowersSynchronizationService, which also runs inline): parse the
// header, check the collectionId belongs to the signer, compute our own
// digest, and report whether it disagrees with the sender's. It never
// performs network I/O - a mismatch is left for the caller to enqueue.
//
// signerActorURI is the actor that signed the HTTP request (the header is
// only meaningful for the collection the signer itself owns; accepting an
// arbitrary collectionId would let any signed sender trigger reconciliation
// on someone else's followers).
func evaluateCollectionSync(ctx context.Context, store storage.Storage, signerActorURI string, headerValue string) (target *models.Account, syncURL string, mismatch bool) {
	if headerValue == "" {
		return nil, "", false
	}
	params := parseSignatureHeader(headerValue)
	if params == nil {
		return nil, "", false
	}
	collectionID := params["collectionId"]
	digest := params["digest"]
	url := params["url"]
	if collectionID == "" || digest == "" || url == "" {
		return nil, "", false
	}

	account, err := store.GetAccountByURI(ctx, signerActorURI)
	if err != nil || account.Domain == nil {
		// Unknown or local signer: nothing we could possibly have drifted on.
		return nil, "", false
	}
	if collectionID != account.FollowersURI {
		slog.Debug("collection-sync: collectionId does not belong to signer", "signer", signerActorURI, "collection_id", collectionID)
		return nil, "", false
	}

	ours, err := localFollowersDigestHex(ctx, store, account.ID)
	if err != nil {
		slog.Warn("collection-sync: failed to compute local digest", "target", account.ActorURI, "error", err)
		return nil, "", false
	}
	if strings.EqualFold(ours, digest) {
		return account, "", false
	}
	return account, url, true
}

// CollectionSyncer performs the asynchronous half: fetching the authoritative
// followers_synchronization collection and reconciling local Follow rows
// against it. Never destructive on a truncated fetch (see
// collectionSyncMaxFollowers).
type CollectionSyncer struct {
	store     storage.Storage
	federator *Federator
}

func NewCollectionSyncer(store storage.Storage, federator *Federator) *CollectionSyncer {
	return &CollectionSyncer{store: store, federator: federator}
}

func (cs *CollectionSyncer) ProcessJob(ctx context.Context, payload string) error {
	var p collectionSyncPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil || p.AccountID == "" || p.URL == "" {
		return fmt.Errorf("collection-sync: invalid payload")
	}
	target, err := cs.store.GetAccount(ctx, p.AccountID)
	if err != nil {
		return fmt.Errorf("collection-sync: load target: %w", err)
	}
	if target.Domain == nil {
		return nil
	}

	uris, err := cs.federator.FetchCollectionURIs(ctx, p.URL, collectionSyncMaxFollowers)
	if err != nil {
		return fmt.Errorf("collection-sync: fetch followers_synchronization: %w", err)
	}
	truncated := len(uris) >= collectionSyncMaxFollowers

	expected := make(map[string]*models.Account, len(uris))
	for _, uri := range uris {
		acct, err := cs.store.GetAccountByURI(ctx, uri)
		if err != nil || acct.Domain != nil {
			// Not a local account we know of - nothing to reconcile for it,
			// matching Mastodon's filter_map over unresolvable entries.
			continue
		}
		expected[acct.ID] = acct
	}

	for id, local := range expected {
		follow, err := cs.store.GetFollowByPair(ctx, id, target.ID)
		switch {
		case err == nil && follow.State == models.FollowAccepted:
			// Already agree.
		case err == nil && follow.State == models.FollowPending:
			if aerr := cs.store.AcceptFollow(ctx, follow.ID); aerr != nil {
				slog.Warn("collection-sync: accept pending follow failed", "follow", follow.ID, "error", aerr)
			} else {
				slog.Info("collection-sync: accepted pending follow to resolve drift", "account", local.ActorURI, "target", target.ActorURI)
			}
		default:
			// The remote believes we follow it; we have no record at all.
			// Correct its belief rather than fabricate a local follow.
			cs.sendUndo(ctx, local, target, syntheticFollowURI(local, target))
		}
	}

	if truncated {
		slog.Warn("collection-sync: followers_synchronization collection was truncated, skipping removal pass", "target", target.ActorURI, "cap", collectionSyncMaxFollowers)
		return nil
	}

	locals, err := cs.store.ListLocalFollowers(ctx, target.ID)
	if err != nil {
		return fmt.Errorf("collection-sync: list local followers: %w", err)
	}
	for _, local := range locals {
		if _, ok := expected[local.ID]; ok {
			continue
		}
		follow, err := cs.store.GetFollowByPair(ctx, local.ID, target.ID)
		if err != nil || follow.State != models.FollowAccepted {
			continue
		}
		if err := cs.store.DeleteFollow(ctx, follow.ID); err != nil {
			slog.Warn("collection-sync: delete stale follow failed", "follow", follow.ID, "error", err)
			continue
		}
		slog.Info("collection-sync: removed stale local follow to resolve drift", "account", local.ActorURI, "target", target.ActorURI)
		cs.sendUndo(ctx, local, target, follow.URI)
	}

	return nil
}

func syntheticFollowURI(local, target *models.Account) string {
	return fmt.Sprintf("%s#follows/%s", local.ActorURI, target.ID)
}

// sendUndo tells target that local does not (or no longer does) follow it.
// followURI is best-effort: for a follow we never had a record of, receivers
// that match strictly on activity ID may ignore the Undo, same limitation
// Mastodon accepts in this same situation. Delivery goes through the durable
// "delivery" job queue (EnqueueDelivery) rather than a direct signed POST,
// matching how the rest of the async inbox path sends outbound activities.
func (cs *CollectionSyncer) sendUndo(ctx context.Context, local, target *models.Account, followURI string) {
	if target.Domain == nil {
		return
	}
	if reachable, err := cs.store.IsInstanceReachable(ctx, *target.Domain); err == nil && !reachable {
		return
	}
	inbox := target.InboxURI
	if target.SharedInboxURI != nil && *target.SharedInboxURI != "" {
		inbox = *target.SharedInboxURI
	}
	if inbox == "" {
		return
	}
	activity := map[string]interface{}{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       followURI + "/undo",
		"type":     "Undo",
		"actor":    local.ActorURI,
		"object": map[string]interface{}{
			"id":     followURI,
			"type":   "Follow",
			"actor":  local.ActorURI,
			"object": target.ActorURI,
		},
	}
	if err := EnqueueDelivery(ctx, cs.store, inbox, activity); err != nil {
		slog.Warn("collection-sync: undo enqueue failed", "local", local.ActorURI, "target", target.ActorURI, "error", err)
	}
}

// enqueueCollectionSync persists a job for CollectionSyncer.ProcessJob. Kept
// separate from evaluateCollectionSync so the inbox handler's synchronous
// path never itself does network I/O.
func enqueueCollectionSync(ctx context.Context, store storage.Storage, target *models.Account, url string) {
	payload, err := json.Marshal(collectionSyncPayload{AccountID: target.ID, URL: url})
	if err != nil {
		return
	}
	job := &models.Job{Kind: CollectionSyncJobKind, Payload: string(payload), RunAt: time.Now(), MaxAttempts: 3, Priority: -50}
	if err := store.EnqueueJob(ctx, job); err != nil {
		slog.Warn("collection-sync: failed to enqueue reconciliation job", "target", target.ActorURI, "error", err)
	}
}

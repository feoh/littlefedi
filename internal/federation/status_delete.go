package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// PrepareStatusDeletion captures the audience before the version-checked delete.
func PrepareStatusDeletion(ctx context.Context, store storage.Storage, domain string, author *models.Account, status *models.Status) (*models.Tombstone, []*models.Job, error) {
	if author == nil || author.Domain != nil || status.Visibility == models.VisibilityLocal || status.Visibility == models.VisibilityLocalUnlisted {
		return nil, nil, nil
	}
	switch status.Visibility {
	case models.VisibilityPublic, models.VisibilityUnlisted, models.VisibilityPrivate, models.VisibilityDirect:
	default:
		return nil, nil, fmt.Errorf("invalid visibility")
	}
	now := time.Now()
	var tombstone *models.Tombstone
	var activity map[string]interface{}
	if status.BoostOfID != nil {
		original, err := store.GetStatus(ctx, *status.BoostOfID)
		if err != nil {
			return nil, nil, err
		}
		announce := BuildAnnounceAS2(domain, status, author, original.URI)
		activity = map[string]interface{}{"@context": "https://www.w3.org/ns/activitystreams", "id": status.URI + "/undo", "type": "Undo", "actor": author.ActorURI, "to": announce["to"], "cc": announce["cc"], "object": announce}
	} else {
		tombstone = &models.Tombstone{URI: status.URI, FormerType: "Note", DeletedAt: now}
		activity = map[string]interface{}{
			"@context": "https://www.w3.org/ns/activitystreams", "id": status.URI + "/delete", "type": "Delete", "actor": author.ActorURI,
			"object": map[string]interface{}{"type": "Tombstone", "id": status.URI, "formerType": "Note", "deleted": now.Format(time.RFC3339)},
		}
	}
	mentions, err := store.GetMentionsForStatus(ctx, status.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("load delete recipients: %w", err)
	}
	var inboxes []string
	for _, mention := range mentions {
		target, err := store.GetAccount(ctx, mention.AccountID)
		if err != nil {
			return nil, nil, err
		}
		if target.Domain == nil {
			continue
		}
		inbox := target.InboxURI
		if target.SharedInboxURI != nil && *target.SharedInboxURI != "" {
			inbox = *target.SharedInboxURI
		}
		if inbox == "" {
			return nil, nil, fmt.Errorf("delete recipient %s has no inbox", target.ID)
		}
		inboxes = append(inboxes, inbox)
	}
	body, err := json.Marshal(activity)
	if err != nil {
		return nil, nil, err
	}
	var jobs []*models.Job
	if status.Visibility == models.VisibilityDirect {
		jobs, err = PrepareInboxDeliveries(ctx, store, author.ID, body, inboxes)
	} else {
		jobs, err = PrepareFollowerDeliveries(ctx, store, author.ID, body, inboxes...)
	}
	return tombstone, jobs, err
}

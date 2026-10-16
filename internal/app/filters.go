package app

import (
	"context"
	"strings"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/statusfilter"
)

// FilterStatuses applies one account's active filters. Storage failures are
// returned so authenticated callers can fail closed.
func (s *Service) FilterStatuses(ctx context.Context, scope string, statuses []*models.Status, account *models.Account) ([]*models.Status, error) {
	statuses = s.VisibleStatuses(ctx, statuses, account)
	if account == nil {
		return statuses, nil
	}
	filters, err := s.store.ListFilters(ctx, account.ID)
	if err != nil {
		return nil, err
	}
	statuses = statusfilter.Apply(filters, scope, statuses, time.Now())
	if !account.HidePostsWithoutAlt {
		return statuses, nil
	}
	filtered := statuses[:0]
	for _, status := range statuses {
		if !hasImageWithoutAlt(status) {
			filtered = append(filtered, status)
		}
	}
	return filtered, nil
}

func hasImageWithoutAlt(status *models.Status) bool {
	if status == nil {
		return false
	}
	if status.BoostOf != nil {
		status = status.BoostOf
	}
	for _, candidate := range []*models.Status{status, status.QuoteOf} {
		if candidate == nil {
			continue
		}
		for _, media := range candidate.MediaAttachments {
			if media.Type == "image" && strings.TrimSpace(media.Description) == "" {
				return true
			}
		}
	}
	return false
}

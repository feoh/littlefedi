package federation

import (
	"context"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// attachStoredPreview loads the link preview already held for a status so the
// outbound Note can ship it as an FEP-8967 Link. Nothing is fetched here: a
// post whose card has never been built simply travels without one.
func attachStoredPreview(ctx context.Context, store storage.Storage, status *models.Status) {
	if status == nil || status.Preview != nil || status.ID == "" {
		return
	}
	preview, err := store.GetStatusPreview(ctx, status.ID)
	if err != nil || preview == nil || preview.SourceURL == "" {
		return
	}
	status.Preview = preview
}

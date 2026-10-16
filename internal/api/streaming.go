package api

import (
	"context"
	"encoding/json"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/cache"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/streaming"
)

// HydrateStreamingStatus restores relations omitted from cross-process broker
// events before serialization.
func (h *Handler) HydrateStreamingStatus(ctx context.Context, status *models.Status) {
	h.app.EnrichStatus(ctx, status, nil)
}

// This file adapts the API entity serializers to the streaming.Serializer
// interface and lets main.go wire a streaming publisher into the API's shared
// app.Service. The API layer owns the Mastodon entity shapes, so streaming
// payloads are produced here to stay byte-for-byte identical with REST
// responses, while the streaming package stays free of an api import cycle.

// SerializeStatus renders a status as the Mastodon status entity JSON streamed to
// clients. A nil viewer yields the public (viewer-neutral) view.
func (h *Handler) SerializeStatus(viewer *models.Account, status *models.Status) ([]byte, error) {
	return json.Marshal(h.statusToMastodon(status, viewer))
}

// SerializeNotification renders a notification as the Mastodon notification
// entity JSON streamed to its recipient.
func (h *Handler) SerializeNotification(viewer *models.Account, n *models.Notification) ([]byte, error) {
	result := h.notificationsToMastodon(context.Background(), []*models.Notification{n}, viewer)
	if len(result) == 0 {
		return json.Marshal(nil)
	}
	return json.Marshal(result[0])
}

func (h *Handler) SerializeFilteredStatus(ctx context.Context, scope string, viewer *models.Account, status *models.Status) ([]byte, bool, error) {
	filtered, err := h.app.FilterStatuses(ctx, scope, []*models.Status{status}, viewer)
	if err != nil || len(filtered) == 0 {
		return nil, false, err
	}
	payload, err := json.Marshal(h.statusToMastodon(filtered[0], viewer))
	return payload, err == nil, err
}

func (h *Handler) SerializeFilteredNotification(ctx context.Context, viewer *models.Account, n *models.Notification) ([]byte, bool, error) {
	result, err := h.notificationsToMastodonChecked(ctx, []*models.Notification{n}, viewer)
	if err != nil || len(result) == 0 {
		return nil, false, err
	}
	payload, err := json.Marshal(result[0])
	return payload, err == nil, err
}

// SetStreamPublisher wires the streaming fan-out into the API's shared
// app.Service so locally created statuses and notifications are streamed live.
func (h *Handler) SetStreamPublisher(p streaming.Publisher) {
	h.app.SetStreamPublisher(p)
}

// SetPageCache wires the optional per-viewer page cache into the shared
// app.Service. A nil cache leaves page caching disabled.
func (h *Handler) SetPageCache(c *cache.MemoryCache, pageTTL, componentTTL time.Duration) {
	h.app.SetPageCache(c, pageTTL, componentTTL)
}

// SetServeStale enables stale-while-revalidate for cached pages.
func (h *Handler) SetServeStale(v bool) { h.app.SetServeStale(v) }

// Compile-time check that the API handler satisfies the streaming serializer.
var _ streaming.Serializer = (*Handler)(nil)
var _ streaming.StatusHydrator = (*Handler)(nil)

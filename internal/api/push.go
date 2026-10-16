package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	pushpkg "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/push"
)

// API Types

// PushSubscriptionRequest is the body for creating/updating a push subscription.
type PushSubscriptionRequest struct {
	Subscription struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256DH string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	} `json:"subscription"`
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256DH string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	Data struct {
		Alerts map[string]bool `json:"alerts,omitempty"`
		Policy string          `json:"policy,omitempty"`
	} `json:"data,omitempty"`
}

// MastodonPushSubscription is the Mastodon API representation of a push subscription.
type MastodonPushSubscription struct {
	ID       string                         `json:"id"`
	Endpoint string                         `json:"endpoint"`
	Alerts   MastodonPushSubscriptionAlerts `json:"alerts"`
	Policy   string                         `json:"policy"`
}

type MastodonPushSubscriptionAlerts struct {
	Mention       bool `json:"mention"`
	Follow        bool `json:"follow"`
	Reblog        bool `json:"reblog"`
	Favourite     bool `json:"favourite"`
	EmojiReaction bool `json:"pleroma:emoji_reaction"`
	Poll          bool `json:"poll"`
	FollowRequest bool `json:"follow_request"`
	Status        bool `json:"status"`
	Quote         bool `json:"quote"`
	// BlogRebuild is a littleFedi extension: the account's own static site
	// finished regenerating.
	BlogRebuild bool `json:"blog_rebuild"`
}

// Endpoints

// PushSubscriptionCreate creates a new push subscription.
// POST /api/v1/push/subscription
func (h *Handler) PushSubscriptionCreate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	tokenID := getAuthTokenID(r.Context())

	// Check if this token already has a subscription (re-registering the
	// same app login updates it in place rather than adding a duplicate).
	existing, _ := h.store.GetPushSubscriptionByOAuthTokenID(r.Context(), tokenID)
	if existing != nil {
		// Update instead
		if err := h.updatePushSubFromRequest(existing, r); err != nil {
			logAPIValidationError(r, http.StatusUnprocessableEntity, err.Error(), "subscription_id", existing.ID)
			writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if err := h.store.UpdatePushSubscription(r.Context(), existing); err != nil {
			slog.Error("api: update push subscription error", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "error updating push subscription")
			return
		}
		writeAPIJSON(w, http.StatusOK, h.pushSubToMastodon(existing))
		return
	}

	req, err := parsePushSubscriptionRequest(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	endpoint, p256dh, auth := req.endpointAndKeys()
	if endpoint == "" || p256dh == "" || auth == "" {
		logAPIValidationError(r, http.StatusUnprocessableEntity, "endpoint, p256dh, and auth are required")
		writeAPIError(w, http.StatusUnprocessableEntity, "endpoint, p256dh, and auth are required")
		return
	}
	endpoint, err = pushpkg.NormalizeEndpoint(endpoint)
	if err != nil {
		logAPIValidationError(r, http.StatusUnprocessableEntity, "invalid push subscription endpoint")
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid push subscription endpoint")
		return
	}
	if owner, _ := h.store.GetPushSubscriptionByEndpoint(r.Context(), endpoint); owner != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "push endpoint is already registered")
		return
	}
	p256dh, auth, err = pushpkg.NormalizeClientKeys(p256dh, auth)
	if err != nil {
		logAPIValidationError(r, http.StatusUnprocessableEntity, "invalid push subscription keys", "key_error", err)
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid push subscription keys")
		return
	}

	sub := &models.PushSubscription{
		ID:        uuid.New().String(),
		AccountID: account.ID,
		Endpoint:  endpoint,
		P256DH:    p256dh,
		Auth:      auth,
		Policy:    "all",
		// Notify bell (type "status"), quote and blog-rebuild pushes are on
		// unless the client opts out.
		NotifyStatus:      true,
		NotifyQuote:       true,
		NotifyBlogRebuild: true,
	}
	if tokenID != "" {
		sub.OAuthTokenID = &tokenID
		sub.Label = h.oauthAppLabel(r.Context())
	}

	// Apply alert preferences
	if req.Data.Alerts != nil {
		sub.NotifyMention = req.Data.Alerts["mention"]
		sub.NotifyFollow = req.Data.Alerts["follow"]
		sub.NotifyReblog = req.Data.Alerts["reblog"]
		sub.NotifyFavourite = req.Data.Alerts["favourite"] || req.Data.Alerts["pleroma:emoji_reaction"]
		sub.NotifyPoll = req.Data.Alerts["poll"]
		sub.NotifyFollowRequest = req.Data.Alerts["follow_request"]
		// Preserve the model default (on) when the client omits the
		// status/quote/blog_rebuild alert.
		if v, ok := req.Data.Alerts["status"]; ok {
			sub.NotifyStatus = v
		}
		if v, ok := req.Data.Alerts["quote"]; ok {
			sub.NotifyQuote = v
		}
		if v, ok := req.Data.Alerts["blog_rebuild"]; ok {
			sub.NotifyBlogRebuild = v
		}
	}
	if req.Data.Policy != "" && !pushpkg.ValidPolicy(req.Data.Policy) {
		logAPIValidationError(r, http.StatusUnprocessableEntity, "invalid push policy")
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid push policy")
		return
	}
	if req.Data.Policy != "" {
		sub.Policy = req.Data.Policy
	}

	if err := h.store.CreatePushSubscription(r.Context(), sub); err != nil {
		slog.Error("api: create push subscription error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error creating push subscription")
		return
	}

	writeAPIJSON(w, http.StatusOK, h.pushSubToMastodon(sub))
}

// PushSubscriptionGet returns the current app's push subscription.
// GET /api/v1/push/subscription
func (h *Handler) PushSubscriptionGet(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	sub, err := h.store.GetPushSubscriptionByOAuthTokenID(r.Context(), getAuthTokenID(r.Context()))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "no push subscription")
		return
	}

	writeAPIJSON(w, http.StatusOK, h.pushSubToMastodon(sub))
}

// PushSubscriptionUpdate updates the current app's push subscription.
// PUT /api/v1/push/subscription
func (h *Handler) PushSubscriptionUpdate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	sub, err := h.store.GetPushSubscriptionByOAuthTokenID(r.Context(), getAuthTokenID(r.Context()))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "no push subscription")
		return
	}

	if err := h.updatePushSubFromRequest(sub, r); err != nil {
		logAPIValidationError(r, http.StatusUnprocessableEntity, err.Error(), "subscription_id", sub.ID)
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := h.store.UpdatePushSubscription(r.Context(), sub); err != nil {
		slog.Error("api: update push subscription error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error updating push subscription")
		return
	}

	writeAPIJSON(w, http.StatusOK, h.pushSubToMastodon(sub))
}

// PushSubscriptionDelete deletes the current app's push subscription only -
// other apps and the web UI's own subscription on this account are
// untouched.
// DELETE /api/v1/push/subscription
func (h *Handler) PushSubscriptionDelete(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	sub, err := h.store.GetPushSubscriptionByOAuthTokenID(r.Context(), getAuthTokenID(r.Context()))
	if err != nil {
		writeAPIJSON(w, http.StatusOK, map[string]interface{}{})
		return
	}
	if err := h.store.DeletePushSubscription(r.Context(), sub.ID); err != nil {
		slog.Error("api: delete push subscription error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error deleting push subscription")
		return
	}

	writeAPIJSON(w, http.StatusOK, map[string]interface{}{})
}

// oauthAppLabel resolves a human-readable label for the OAuth application
// tied to the current request's token, for display in Settings. Best-effort:
// an empty label just means Settings falls back to a generic name.
func (h *Handler) oauthAppLabel(ctx context.Context) string {
	appID := getAuthAppID(ctx)
	if appID == "" {
		return ""
	}
	app, err := h.store.GetOAuthApp(ctx, appID)
	if err != nil || app == nil {
		return ""
	}
	return app.Name
}

// Helpers

func (h *Handler) pushSubToMastodon(sub *models.PushSubscription) *MastodonPushSubscription {
	return &MastodonPushSubscription{
		ID:       sub.ID,
		Endpoint: sub.Endpoint,
		Alerts: MastodonPushSubscriptionAlerts{
			Mention:       sub.NotifyMention,
			Follow:        sub.NotifyFollow,
			Reblog:        sub.NotifyReblog,
			Favourite:     sub.NotifyFavourite,
			EmojiReaction: sub.NotifyFavourite,
			Poll:          sub.NotifyPoll,
			FollowRequest: sub.NotifyFollowRequest,
			Status:        sub.NotifyStatus,
			Quote:         sub.NotifyQuote,
			BlogRebuild:   sub.NotifyBlogRebuild,
		},
		Policy: sub.Policy,
	}
}

func parsePushSubscriptionRequest(r *http.Request) (*PushSubscriptionRequest, error) {
	var req PushSubscriptionRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		return nil, err
	}
	return &req, nil
}

func (req *PushSubscriptionRequest) endpointAndKeys() (endpoint, p256dh, auth string) {
	endpoint = req.Subscription.Endpoint
	if endpoint == "" {
		endpoint = req.Endpoint
	}
	p256dh = req.Subscription.Keys.P256DH
	if p256dh == "" {
		p256dh = req.Keys.P256DH
	}
	auth = req.Subscription.Keys.Auth
	if auth == "" {
		auth = req.Keys.Auth
	}
	return endpoint, p256dh, auth
}

func (h *Handler) updatePushSubFromRequest(sub *models.PushSubscription, r *http.Request) error {
	req, err := parsePushSubscriptionRequest(r)
	if err != nil {
		return err
	}

	endpoint, p256dh, auth := req.endpointAndKeys()
	if endpoint != "" {
		normalized, err := pushpkg.NormalizeEndpoint(endpoint)
		if err != nil {
			return fmt.Errorf("invalid push subscription endpoint")
		}
		if owner, _ := h.store.GetPushSubscriptionByEndpoint(r.Context(), normalized); owner != nil && owner.ID != sub.ID {
			return fmt.Errorf("push endpoint is already registered")
		}
		sub.Endpoint = normalized
	}
	if p256dh != "" {
		sub.P256DH = p256dh
	}
	if auth != "" {
		sub.Auth = auth
	}
	if req.Data.Alerts != nil {
		if v, ok := req.Data.Alerts["mention"]; ok {
			sub.NotifyMention = v
		}
		if v, ok := req.Data.Alerts["follow"]; ok {
			sub.NotifyFollow = v
		}
		if v, ok := req.Data.Alerts["reblog"]; ok {
			sub.NotifyReblog = v
		}
		if v, ok := req.Data.Alerts["favourite"]; ok {
			sub.NotifyFavourite = v
		}
		if v, ok := req.Data.Alerts["pleroma:emoji_reaction"]; ok {
			sub.NotifyFavourite = v
		}
		if v, ok := req.Data.Alerts["poll"]; ok {
			sub.NotifyPoll = v
		}
		if v, ok := req.Data.Alerts["follow_request"]; ok {
			sub.NotifyFollowRequest = v
		}
		if v, ok := req.Data.Alerts["status"]; ok {
			sub.NotifyStatus = v
		}
		if v, ok := req.Data.Alerts["quote"]; ok {
			sub.NotifyQuote = v
		}
		if v, ok := req.Data.Alerts["blog_rebuild"]; ok {
			sub.NotifyBlogRebuild = v
		}
	}
	if req.Data.Policy != "" {
		if !pushpkg.ValidPolicy(req.Data.Policy) {
			return fmt.Errorf("invalid push policy")
		}
		sub.Policy = req.Data.Policy
	}

	normalizedP256DH, normalizedAuth, err := pushpkg.NormalizeClientKeys(sub.P256DH, sub.Auth)
	if err != nil {
		return err
	}
	sub.P256DH = normalizedP256DH
	sub.Auth = normalizedAuth
	return nil
}

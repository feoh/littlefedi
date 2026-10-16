package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/keycrypt"
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
		// Standard asks for RFC 8291 aes128gcm instead of the aesgcm draft.
		Standard *bool `json:"standard,omitempty"`
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
	Standard bool                           `json:"standard"`
	Alerts   MastodonPushSubscriptionAlerts `json:"alerts"`
	Policy   string                         `json:"policy"`
	// ServerKey is the instance's VAPID public key. Mastodon's own apps
	// refuse a subscription response without it.
	ServerKey string `json:"server_key"`
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
	sealedToken, err := sealedBearerToken(r)
	if err != nil {
		slog.Error("api: seal push subscription token", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error creating push subscription")
		return
	}

	existing, _ := h.store.GetPushSubscriptionByOAuthTokenID(r.Context(), tokenID)
	if existing != nil {
		// Update instead. A POST describes the whole subscription, so an
		// omitted standard means the legacy encoding again, as in Mastodon.
		existing.AccessToken = sealedToken
		existing.Standard = false
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
	// The same device signing in to the same account again (a new app
	// login) replaces its old subscription, as Mastodon does. Endpoints stay
	// unique, so another account's subscription still blocks it.
	owner, _ := h.store.GetPushSubscriptionByEndpoint(r.Context(), endpoint)
	if owner != nil && owner.AccountID != account.ID {
		writeAPIError(w, http.StatusUnprocessableEntity, "push endpoint is already registered")
		return
	}
	p256dh, auth, err = pushpkg.NormalizeClientKeys(p256dh, auth)
	if err != nil {
		logAPIValidationError(r, http.StatusUnprocessableEntity, "invalid push subscription keys", "key_error", err)
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid push subscription keys")
		return
	}
	if owner != nil {
		if err := h.store.DeletePushSubscription(r.Context(), owner.ID); err != nil {
			slog.Error("api: replace push subscription error", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "error creating push subscription")
			return
		}
	}

	sub := &models.PushSubscription{
		ID:        uuid.New().String(),
		AccountID: account.ID,
		Endpoint:  endpoint,
		P256DH:    p256dh,
		Auth:      auth,
		Policy:    "all",
		// Mastodon sends the aesgcm draft encoding unless the client asks
		// for RFC 8291.
		Standard:    req.Subscription.Standard != nil && *req.Subscription.Standard,
		AccessToken: sealedToken,
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
		ID:        sub.ID,
		Endpoint:  sub.Endpoint,
		Standard:  sub.Standard,
		ServerKey: h.vapidPublicKey,
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

// parsePushSubscriptionRequest reads a subscription request the way Mastodon
// does: as JSON, or as form fields named subscription[endpoint],
// subscription[keys][p256dh], data[alerts][mention] and so on, which is how
// clients such as Tuba send it. Booleans may arrive as strings either way.
func parsePushSubscriptionRequest(r *http.Request) (*PushSubscriptionRequest, error) {
	const maxBody = 16 << 10
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.HasPrefix(contentType, "application/x-www-form-urlencoded") || strings.HasPrefix(contentType, "multipart/form-data") {
		r.Body = http.MaxBytesReader(nil, r.Body, maxBody)
		return parsePushSubscriptionForm(r)
	}

	var raw struct {
		Subscription struct {
			Endpoint string `json:"endpoint"`
			Keys     struct {
				P256DH string `json:"p256dh"`
				Auth   string `json:"auth"`
			} `json:"keys"`
			Standard *flexBool `json:"standard"`
		} `json:"subscription"`
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256DH string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
		Data struct {
			Alerts map[string]flexBool `json:"alerts"`
			Policy string              `json:"policy"`
		} `json:"data"`
		Policy string `json:"policy"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&raw); err != nil {
		return nil, err
	}
	req := &PushSubscriptionRequest{Endpoint: raw.Endpoint}
	req.Subscription.Endpoint = raw.Subscription.Endpoint
	req.Subscription.Keys.P256DH = raw.Subscription.Keys.P256DH
	req.Subscription.Keys.Auth = raw.Subscription.Keys.Auth
	if raw.Subscription.Standard != nil {
		standard := bool(*raw.Subscription.Standard)
		req.Subscription.Standard = &standard
	}
	req.Keys.P256DH, req.Keys.Auth = raw.Keys.P256DH, raw.Keys.Auth
	if raw.Data.Alerts != nil {
		req.Data.Alerts = make(map[string]bool, len(raw.Data.Alerts))
		for name, v := range raw.Data.Alerts {
			req.Data.Alerts[name] = bool(v)
		}
	}
	req.Data.Policy = raw.Data.Policy
	if req.Data.Policy == "" {
		req.Data.Policy = raw.Policy
	}
	return req, nil
}

func parsePushSubscriptionForm(r *http.Request) (*PushSubscriptionRequest, error) {
	var err error
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		err = r.ParseMultipartForm(16 << 10)
	} else {
		err = r.ParseForm()
	}
	if err != nil {
		return nil, err
	}
	form := r.PostForm
	if r.MultipartForm != nil {
		form = r.MultipartForm.Value
	}
	get := func(key string) string {
		if values := form[key]; len(values) > 0 {
			return values[0]
		}
		return ""
	}

	req := &PushSubscriptionRequest{Endpoint: get("endpoint")}
	req.Subscription.Endpoint = get("subscription[endpoint]")
	req.Subscription.Keys.P256DH = get("subscription[keys][p256dh]")
	req.Subscription.Keys.Auth = get("subscription[keys][auth]")
	if raw := get("subscription[standard]"); raw != "" {
		standard, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid subscription[standard]")
		}
		req.Subscription.Standard = &standard
	}
	req.Data.Policy = get("data[policy]")
	if req.Data.Policy == "" {
		req.Data.Policy = get("policy")
	}
	for key, values := range form {
		name, ok := strings.CutPrefix(key, "data[alerts][")
		if !ok || !strings.HasSuffix(name, "]") || len(values) == 0 {
			continue
		}
		enabled, err := strconv.ParseBool(values[0])
		if err != nil {
			return nil, fmt.Errorf("invalid %s", key)
		}
		if req.Data.Alerts == nil {
			req.Data.Alerts = make(map[string]bool)
		}
		req.Data.Alerts[strings.TrimSuffix(name, "]")] = enabled
	}
	return req, nil
}

// flexBool accepts JSON booleans and the strings clients sometimes send
// instead ("true", "false", "1", "0").
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	v, err := strconv.ParseBool(strings.Trim(string(data), `"`))
	if err != nil {
		return fmt.Errorf("invalid boolean %s", data)
	}
	*b = flexBool(v)
	return nil
}

// bearerToken returns the raw access token the request authenticated with.
func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	for _, prefix := range []string{"Bearer ", "token "} {
		if token, ok := strings.CutPrefix(header, prefix); ok {
			return strings.TrimSpace(token)
		}
	}
	return ""
}

// sealedBearerToken seals the request's access token for storage on a push
// subscription; Mastodon push payloads carry it back to the client.
func sealedBearerToken(r *http.Request) (string, error) {
	token := bearerToken(r)
	if token == "" {
		return "", nil
	}
	return keycrypt.Seal(token)
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
	if req.Subscription.Standard != nil {
		sub.Standard = *req.Subscription.Standard
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

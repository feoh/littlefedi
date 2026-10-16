package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/accounts"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/app"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/auth"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/federation"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/i18n"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mail"
	medialib "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media"
	accountmigration "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/migration"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// Account Endpoints

// AccountCreate creates a new account via the Mastodon API.
// POST /api/v1/accounts
func (h *Handler) AccountCreate(w http.ResponseWriter, r *http.Request) {
	params, parseMode, err := decodeMastodonParams(r, 0)
	if err != nil {
		logAPIValidationError(r, http.StatusUnprocessableEntity, "invalid request body", "parse_mode", parseMode, "parse_error", err)
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid request body")
		return
	}

	// Check if registration is enabled
	if !h.Registrations {
		writeAPIError(w, http.StatusForbidden, "registration is not enabled")
		return
	}

	username := params.First("username")
	email := params.First("email")
	password := params.First("password")
	agreement := params.First("agreement")
	locale := params.First("locale")
	inviteCode := params.First("invite_code")

	if !i18n.IsSupported(locale) {
		locale = h.config.DefaultLocale
	}

	if username == "" || password == "" {
		logAPIValidationError(r, http.StatusUnprocessableEntity, "username and password are required", "parse_mode", parseMode)
		writeAPIError(w, http.StatusUnprocessableEntity, "username and password are required")
		return
	}
	if agreement != "true" && agreement != "1" {
		logAPIValidationError(r, http.StatusUnprocessableEntity, "agreement must be accepted", "parse_mode", parseMode)
		writeAPIError(w, http.StatusUnprocessableEntity, "agreement must be accepted")
		return
	}

	// Build registration config
	minPasswordLength := h.MinPasswordLength
	if minPasswordLength <= 0 {
		minPasswordLength = accounts.DefaultMinPasswordLength
	}
	regCfg := &accounts.ConfigAdapter{
		MultiUser:         true,
		Registrations:     h.Registrations,
		InviteRequired:    h.InviteRequired,
		MinPasswordLength: minPasswordLength,
		RequireApproval:   h.RequireApproval,
		RequireEmail:      h.RequireEmail,
	}

	req := &accounts.RegistrationRequest{
		Username:        username,
		Email:           email,
		Password:        password,
		PasswordConfirm: password,
		InviteCode:      inviteCode,
	}

	account, err := accounts.Register(r.Context(), h.store, h.domain, regCfg, req)
	if err != nil {
		slog.Warn("api: account creation failed", "error", err)
		description := "registration failed"
		for _, publicErr := range []error{
			accounts.ErrRegistrationClosed, accounts.ErrUsernameTaken, accounts.ErrInvalidUsername,
			accounts.ErrPasswordTooShort, accounts.ErrInviteRequired, accounts.ErrInviteInvalid,
			accounts.ErrInviteExhausted, accounts.ErrEmailRequired, accounts.ErrInvalidEmail, accounts.ErrEmailTaken,
		} {
			if errors.Is(err, publicErr) {
				description = err.Error()
				break
			}
		}
		writeAPIError(w, http.StatusUnprocessableEntity, description)
		return
	}
	if h.mailer != nil && account.Email != "" {
		if err := mail.EnqueueEmailVerification(r.Context(), h.store, h.domain, account); err != nil {
			slog.Error("api: enqueue email verification", "error", err)
		}
	}

	if locale != "" && locale != account.Locale {
		account.Locale = locale
		if err := h.store.UpdateAccount(r.Context(), account); err != nil {
			slog.Warn("api: persist registration locale failed", "error", err)
		}
	}
	if h.mailer != nil {
		mail.NotifyAccountCreated(r.Context(), h.store, h.domain, account, req.InviteCode)
	}

	result := h.accountToMastodon(account, nil, false)
	writeAPIJSON(w, http.StatusOK, result)
}

// AccountVerifyCredentials returns the authenticated user's account.
// GET /api/v1/accounts/verify_credentials
func (h *Handler) AccountVerifyCredentials(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	result := h.accountToMastodon(account, account, true)
	writeAPIJSON(w, http.StatusOK, result)
}

// AccountUpdateCredentials updates the authenticated user's profile.
// PATCH /api/v1/accounts/update_credentials
func (h *Handler) AccountUpdateCredentials(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	maxSize := h.config.MaxMediaSize
	if maxSize <= 0 {
		maxSize = 10 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSize+(1<<20))
	params, parseMode, err := decodeMastodonParams(r, maxSize)
	if err != nil {
		logAPIValidationError(r, http.StatusBadRequest, "invalid request body", "parse_mode", parseMode, "parse_error", err)
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	displayName := params.First("display_name")
	note := params.First("note")
	lockedStr := params.First("locked")
	botStr := params.First("bot")

	if displayName != "" {
		account.DisplayName = displayName
	}
	if note != "" {
		// NoteSource keeps the raw text (returned as source[note]); Note holds the
		// rendered, sanitized HTML (returned as note and federated as summary),
		// matching the web UI's UpdateProfile so bios render consistently and raw
		// user input never lands in an HTML field unsanitized.
		account.NoteSource = note
		account.Note = h.app.RenderProfileNote(r.Context(), account, note)
	}
	if lockedStr != "" {
		account.ManuallyApprovesFollowers = parseAPIBool(lockedStr)
		account.Locked = account.ManuallyApprovesFollowers
	}
	if botStr != "" {
		account.IsBot = parseAPIBool(botStr)
	}

	// Handle avatar upload
	if avatarURL, err := h.saveProfileImage(r, account, "avatar", maxSize); err == nil && avatarURL != "" {
		account.Avatar = avatarURL
	} else if params.First("avatar_remove") == "true" && account.Avatar != "" {
		account.Avatar = ""
	}

	// Handle header upload
	if headerURL, err := h.saveProfileImage(r, account, "header", maxSize); err == nil && headerURL != "" {
		account.Header = headerURL
	} else if params.First("header_remove") == "true" && account.Header != "" {
		account.Header = ""
	}

	if err := h.store.UpdateAccount(r.Context(), account); err != nil {
		slog.Error("api: update account error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error updating account")
		return
	}
	h.app.InvalidateAccountComponents(account.ID)

	// Federate the profile update so remote instances see avatar/header changes
	if account.Domain == nil && h.federator != nil {
		h.app.FederateProfileUpdate(r.Context(), account)
	}

	result := h.accountToMastodon(account, account, true)
	writeAPIJSON(w, http.StatusOK, result)
}

// saveProfileImage saves an avatar or header image for an account and returns the URL.
func (h *Handler) saveProfileImage(r *http.Request, account *models.Account, fieldName string, maxSize int64) (string, error) {
	file, header, err := r.FormFile(fieldName)
	if err == http.ErrMissingFile {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()

	if header.Size <= 0 {
		return "", nil
	}
	if header.Size > maxSize {
		return "", fmt.Errorf("%s file too large (%d > %d)", fieldName, header.Size, maxSize)
	}

	mimeType := header.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	if !strings.HasPrefix(mimeType, "image/") {
		return "", fmt.Errorf("%s must be an image", fieldName)
	}

	media := &models.MediaAttachment{
		AccountID: account.ID,
		Type:      "image",
		FileSize:  header.Size,
		MIMEType:  mimeType,
	}

	if err := h.store.CreateMediaAttachment(r.Context(), media); err != nil {
		return "", err
	}

	// Use the renamed filename if configured
	safeFilename := medialib.MediaFilename(header.Filename, mimeType, h.config.RenameMedia)
	err = medialib.StoreUpload(r.Context(), h.config.MediaStore, file, header.Size, media, safeFilename, h.config.StripEXIF)
	if err != nil {
		_ = h.store.DeleteMediaAttachment(r.Context(), media.ID)
		return "", err
	}
	if err := h.store.UpdateMediaAttachment(r.Context(), media); err != nil {
		return "", err
	}

	return media.URL, nil
}

// AccountGet returns an account by ID.
// GET /api/v1/accounts/{id}
func (h *Handler) AccountGet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	account, err := h.store.GetAccount(r.Context(), id)
	if err != nil {
		slog.Warn("api: account not found", "id", id, "error", err)
		writeAPIError(w, http.StatusNotFound, "account not found")
		return
	}
	account = h.refreshRemoteAccountCountsIfMissing(r.Context(), account)

	result := h.accountToMastodon(account, getAuthAccount(r.Context()), false)
	writeAPIJSON(w, http.StatusOK, result)
}

func (h *Handler) refreshRemoteAccountCountsIfMissing(ctx context.Context, account *models.Account) *models.Account {
	if h.federator == nil || account == nil || account.Domain == nil {
		return account
	}
	if account.CountsFetchedAt != nil || account.FollowersCount != 0 || account.FollowingCount != 0 || account.StatusesCount != 0 {
		h.federator.RevalidateActorAsync(ctx, account)
		return account
	}

	refreshed, err := h.federator.RefreshActor(ctx, account.ActorURI, federation.ActorRefreshOptions{EnrichCounts: true})
	if err != nil {
		slog.Debug("api: remote account count refresh failed", "account_id", account.ID, "uri", account.ActorURI, "error", err)
		h.federator.RevalidateActorAsync(ctx, account)
		return account
	}
	return refreshed
}

// AccountStatuses returns statuses for an account.
// GET /api/v1/accounts/{id}/statuses
func (h *Handler) AccountStatuses(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	onlyMedia := r.URL.Query().Get("only_media") == "true"
	excludeReplies := r.URL.Query().Get("exclude_replies") == "true"
	excludeReblogs := r.URL.Query().Get("exclude_reblogs") == "true"
	pinned := r.URL.Query().Get("pinned") == "true"

	params := parsePagination(r)

	authAccount := getAuthAccount(r.Context())
	res, err := h.app.PageCached(
		r.Context(),
		h.app.PageKey(viewerIDOf(authAccount), "api:acct:"+id, pageKeyBool(excludeReplies), pageKeyBool(excludeReblogs), pageKeyBool(onlyMedia), pageKeyBool(pinned), pageKeyPtr(params.MaxID), pageKeyPtr(params.MinID), pageKeyPtr(params.SinceID), strconv.Itoa(params.Limit)),
		func(ctx context.Context) (app.PageResult, error) {
			statuses, err := h.store.GetAccountStatuses(ctx, viewerIDOf(authAccount), id, params.MaxID, params.MinID, params.SinceID, params.Limit, excludeReplies, excludeReblogs, onlyMedia, pinned)
			if err != nil {
				return app.PageResult{}, err
			}
			statuses = h.app.VisibleStatuses(ctx, statuses, authAccount)
			h.app.EnrichStatuses(ctx, statuses, authAccount)
			cursor, count := pageCursor(statuses)
			statuses, err = h.app.FilterStatuses(ctx, "account", statuses, authAccount)
			return app.PageResult{Statuses: statuses, Cursor: cursor, RawCount: count}, err
		})
	if err != nil {
		slog.Error("api: account statuses error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching statuses")
		return
	}
	statuses := res.Statuses

	result := make([]*MastodonStatus, 0, len(statuses))
	for _, s := range statuses {
		result = append(result, h.statusToMastodon(s, authAccount))
	}

	setLinkHeaderIDs(w, "/api/v1/accounts/"+id+"/statuses", params, firstStatusID(statuses), res.Cursor, res.RawCount)
	writeAPIJSON(w, http.StatusOK, result)
}

// AccountFollowers returns followers of an account.
// GET /api/v1/accounts/{id}/followers
func (h *Handler) AccountFollowers(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	account, err := h.store.GetAccount(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "account not found")
		} else {
			writeAPIError(w, http.StatusInternalServerError, "error fetching account")
		}
		return
	}
	if app.FollowListHidden(account, getAuthAccount(r.Context()), true) {
		writeAPIError(w, http.StatusForbidden, "account list is hidden")
		return
	}

	params := parsePagination(r)

	followers, err := h.store.ListFollowers(r.Context(), id, params.MaxID, params.Limit)
	if err != nil {
		slog.Error("api: followers error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching followers")
		return
	}

	result := make([]*MastodonAccount, 0, len(followers))
	for _, f := range followers {
		result = append(result, h.accountToMastodon(f, getAuthAccount(r.Context()), false))
	}

	writeAPIJSON(w, http.StatusOK, result)
}

// AccountFollowing returns who an account follows.
// GET /api/v1/accounts/{id}/following
func (h *Handler) AccountFollowing(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	account, err := h.store.GetAccount(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "account not found")
		} else {
			writeAPIError(w, http.StatusInternalServerError, "error fetching account")
		}
		return
	}
	if app.FollowListHidden(account, getAuthAccount(r.Context()), false) {
		writeAPIError(w, http.StatusForbidden, "account list is hidden")
		return
	}

	params := parsePagination(r)

	following, err := h.store.ListFollowing(r.Context(), id, params.MaxID, params.Limit)
	if err != nil {
		slog.Error("api: following error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching following")
		return
	}

	result := make([]*MastodonAccount, 0, len(following))
	for _, f := range following {
		result = append(result, h.accountToMastodon(f, getAuthAccount(r.Context()), false))
	}

	writeAPIJSON(w, http.StatusOK, result)
}

// AccountFollow follows an account.
// POST /api/v1/accounts/{id}/follow
func (h *Handler) AccountFollow(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	targetID := chi.URLParam(r, "id")
	if targetID == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	params, parseMode, err := decodeMastodonParams(r, 0)
	if err != nil {
		logAPIValidationError(r, http.StatusUnprocessableEntity, "invalid request body", "parse_mode", parseMode, "parse_error", err)
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid request body")
		return
	}

	if _, err := h.app.Follow(r.Context(), account, targetID); err != nil {
		if writeFederationPolicyError(w, err) {
			return
		}
		slog.Error("api: follow error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error following account")
		return
	}

	// Honour the notify "bell" param when supplied (Mastodon-compatible).
	if v := params.First("notify"); v != "" {
		if err := h.app.SetFollowNotify(r.Context(), account, targetID, parseAPIBool(v)); err != nil {
			slog.Error("api: set follow notify failed", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "error updating follow notification preference")
			return
		}
	}

	// Return relationship
	rel := h.getRelationship(r.Context(), account.ID, targetID)
	writeAPIJSON(w, http.StatusOK, rel)
}

// AccountUnfollow unfollows an account.
// POST /api/v1/accounts/{id}/unfollow
func (h *Handler) AccountUnfollow(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	targetID := chi.URLParam(r, "id")
	if targetID == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	// Use the application service so remote accounts receive Undo(Follow).
	if _, err := h.store.GetFollowByPair(r.Context(), account.ID, targetID); err == nil {
		if err := h.app.Unfollow(r.Context(), account, targetID); err != nil {
			slog.Error("api: unfollow error", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "error unfollowing account")
			return
		}
	}

	// Return relationship
	rel := h.getRelationship(r.Context(), account.ID, targetID)
	writeAPIJSON(w, http.StatusOK, rel)
}

// AccountBlock blocks an account.
// POST /api/v1/accounts/{id}/block
func (h *Handler) AccountBlock(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	targetID := chi.URLParam(r, "id")
	if targetID == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}
	if err := h.app.Block(r.Context(), account, targetID); err != nil {
		slog.Error("api: block error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error blocking account")
		return
	}
	writeAPIJSON(w, http.StatusOK, h.getRelationship(r.Context(), account.ID, targetID))
}

// AccountUnblock removes a block.
// POST /api/v1/accounts/{id}/unblock
func (h *Handler) AccountUnblock(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	targetID := chi.URLParam(r, "id")
	if targetID == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}
	if err := h.app.Unblock(r.Context(), account, targetID); err != nil {
		slog.Error("api: unblock error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error unblocking account")
		return
	}
	writeAPIJSON(w, http.StatusOK, h.getRelationship(r.Context(), account.ID, targetID))
}

// AccountMute mutes an account. Accepts form params notifications (bool, default
// true) and duration (seconds, 0 = indefinite).
// POST /api/v1/accounts/{id}/mute
func (h *Handler) AccountMute(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	targetID := chi.URLParam(r, "id")
	if targetID == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	// Mastodon defaults notifications=true (mute notifications too).
	hideNotifications := true
	if v := r.FormValue("notifications"); v == "false" || v == "0" {
		hideNotifications = false
	}
	var durationMinutes *int
	if v := r.FormValue("duration"); v != "" {
		var seconds int
		if _, err := fmt.Sscanf(v, "%d", &seconds); err == nil && seconds > 0 {
			minutes := seconds / 60
			if minutes < 1 {
				minutes = 1
			}
			durationMinutes = &minutes
		}
	}

	if err := h.app.Mute(r.Context(), account, targetID, hideNotifications, durationMinutes); err != nil {
		slog.Error("api: mute error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error muting account")
		return
	}
	writeAPIJSON(w, http.StatusOK, h.getRelationship(r.Context(), account.ID, targetID))
}

// AccountUnmute removes a mute.
// POST /api/v1/accounts/{id}/unmute
func (h *Handler) AccountUnmute(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	targetID := chi.URLParam(r, "id")
	if targetID == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}
	if err := h.app.Unmute(r.Context(), account, targetID); err != nil {
		slog.Error("api: unmute error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error unmuting account")
		return
	}
	writeAPIJSON(w, http.StatusOK, h.getRelationship(r.Context(), account.ID, targetID))
}

// AccountRelationships returns relationships for a set of accounts.
// GET /api/v1/accounts/relationships
func (h *Handler) AccountRelationships(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	idStrs := r.URL.Query()["id[]"]
	if len(idStrs) == 0 {
		idStrs = r.URL.Query()["id"]
	}

	results := make([]*MastodonRelationship, 0, len(idStrs))
	for _, id := range idStrs {
		results = append(results, h.getRelationship(r.Context(), account.ID, id))
	}

	writeAPIJSON(w, http.StatusOK, results)
}

// FollowRequestsList lists pending follow requests.
// GET /api/v1/follow_requests
func (h *Handler) FollowRequestsList(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	requests, err := h.store.ListFollowRequests(r.Context(), account.ID, 40)
	if err != nil {
		slog.Error("api: follow requests error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching follow requests")
		return
	}

	result := make([]*MastodonAccount, 0, len(requests))
	for _, req := range requests {
		follower, err := h.store.GetAccount(r.Context(), req.AccountID)
		if err == nil {
			result = append(result, h.accountToMastodon(follower, account, false))
		}
	}

	writeAPIJSON(w, http.StatusOK, result)
}

// FollowRequestAuthorize authorizes a follow request.
// POST /api/v1/follow_requests/{id}/authorize
func (h *Handler) FollowRequestAuthorize(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	if err := h.app.AnswerFollowRequest(r.Context(), account, chi.URLParam(r, "id"), true); err != nil {
		writeAPIError(w, http.StatusNotFound, "follow request not found")
		return
	}

	writeAPIJSON(w, http.StatusOK, map[string]string{"status": "accepted"})
}

// FollowRequestReject rejects a follow request.
// POST /api/v1/follow_requests/{id}/reject
func (h *Handler) FollowRequestReject(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	if err := h.app.AnswerFollowRequest(r.Context(), account, chi.URLParam(r, "id"), false); err != nil {
		writeAPIError(w, http.StatusNotFound, "follow request not found")
		return
	}

	writeAPIJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}

// FavouritesList returns the user's favourited statuses.
// GET /api/v1/favourites
func (h *Handler) FavouritesList(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	params := parsePagination(r)

	res, err := h.app.PageCached(
		r.Context(),
		h.app.PageKey(account.ID, "api:favourites", pageKeyPtr(params.MaxID), strconv.Itoa(params.Limit)),
		func(ctx context.Context) (app.PageResult, error) {
			statuses, err := h.store.ListFavourites(ctx, account.ID, params.MaxID, params.Limit)
			if err != nil {
				return app.PageResult{}, err
			}
			statuses = h.app.VisibleStatuses(ctx, statuses, account)
			h.app.EnrichStatuses(ctx, statuses, account)
			cursor, count := pageCursor(statuses)
			statuses, err = h.app.FilterStatuses(ctx, "home", statuses, account)
			return app.PageResult{Statuses: statuses, Cursor: cursor, RawCount: count}, err
		})
	if err != nil {
		slog.Error("api: favourites error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching favourites")
		return
	}
	statuses := res.Statuses

	result := make([]*MastodonStatus, 0, len(statuses))
	for _, s := range statuses {
		result = append(result, h.statusToMastodon(s, account))
	}

	setLinkHeaderIDs(w, "/api/v1/favourites", params, firstStatusID(statuses), res.Cursor, res.RawCount)
	writeAPIJSON(w, http.StatusOK, result)
}

// BookmarksList returns the user's bookmarked statuses.
// GET /api/v1/bookmarks
func (h *Handler) BookmarksList(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	params := parsePagination(r)
	res, err := h.app.PageCached(
		r.Context(),
		h.app.PageKey(account.ID, "api:bookmarks", pageKeyPtr(params.MaxID), strconv.Itoa(params.Limit)),
		func(ctx context.Context) (app.PageResult, error) {
			statuses, err := h.store.ListBookmarks(ctx, account.ID, params.MaxID, params.Limit)
			if err != nil {
				return app.PageResult{}, err
			}
			statuses = h.app.VisibleStatuses(ctx, statuses, account)
			h.app.EnrichStatuses(ctx, statuses, account)
			cursor, count := pageCursor(statuses)
			statuses, err = h.app.FilterStatuses(ctx, "home", statuses, account)
			return app.PageResult{Statuses: statuses, Cursor: cursor, RawCount: count}, err
		})
	if err != nil {
		slog.Error("api: bookmarks error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching bookmarks")
		return
	}
	statuses := res.Statuses

	result := make([]*MastodonStatus, 0, len(statuses))
	for _, s := range statuses {
		result = append(result, h.statusToMastodon(s, account))
	}

	setLinkHeaderIDs(w, "/api/v1/bookmarks", params, firstStatusID(statuses), res.Cursor, res.RawCount)
	writeAPIJSON(w, http.StatusOK, result)
}

// BlocksList returns accounts the user has blocked.
// GET /api/v1/blocks
func (h *Handler) BlocksList(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	params := parsePagination(r)

	blocked, err := h.store.ListBlocks(r.Context(), account.ID, params.MaxID, params.Limit)
	if err != nil {
		slog.Error("api: blocks error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching blocks")
		return
	}

	result := make([]*MastodonAccount, 0, len(blocked))
	for _, b := range blocked {
		result = append(result, h.accountToMastodon(b, account, false))
	}

	writeAPIJSON(w, http.StatusOK, result)
}

// MutesList returns accounts the user has muted.
// GET /api/v1/mutes
func (h *Handler) MutesList(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	params := parsePagination(r)

	muted, err := h.store.ListMutes(r.Context(), account.ID, params.MaxID, params.Limit)
	if err != nil {
		slog.Error("api: mutes error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching mutes")
		return
	}

	result := make([]*MastodonAccount, 0, len(muted))
	for _, m := range muted {
		result = append(result, h.accountToMastodon(m, account, false))
	}

	writeAPIJSON(w, http.StatusOK, result)
}

// DomainBlocksList returns the domains the user has blocked.
// GET /api/v1/domain_blocks
func (h *Handler) DomainBlocksList(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	domains, err := h.store.ListAccountDomainBlocks(r.Context(), account.ID)
	if err != nil {
		slog.Error("api: domain blocks list error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching domain blocks")
		return
	}
	if domains == nil {
		domains = []string{}
	}
	writeAPIJSON(w, http.StatusOK, domains)
}

// DomainBlockCreate blocks a domain for the user.
// POST /api/v1/domain_blocks  (param: domain)
func (h *Handler) DomainBlockCreate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	domain := normalizeDomainParam(r.FormValue("domain"))
	if domain == "" {
		writeAPIError(w, http.StatusUnprocessableEntity, "domain is required")
		return
	}
	if err := h.store.CreateAccountDomainBlock(r.Context(), account.ID, domain); err != nil {
		slog.Error("api: domain block create error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error creating domain block")
		return
	}
	writeAPIJSON(w, http.StatusOK, struct{}{})
}

// DomainBlockDelete unblocks a domain for the user.
// DELETE /api/v1/domain_blocks  (param: domain)
func (h *Handler) DomainBlockDelete(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	domain := normalizeDomainParam(r.FormValue("domain"))
	if domain == "" {
		writeAPIError(w, http.StatusUnprocessableEntity, "domain is required")
		return
	}
	if err := h.store.DeleteAccountDomainBlock(r.Context(), account.ID, domain); err != nil {
		slog.Error("api: domain block delete error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error deleting domain block")
		return
	}
	writeAPIJSON(w, http.StatusOK, struct{}{})
}

// normalizeDomainParam lowercases and trims a domain, stripping any scheme or
// leading "@" a client might send.
func normalizeDomainParam(d string) string {
	d = strings.TrimSpace(strings.ToLower(d))
	d = strings.TrimPrefix(d, "@")
	d = strings.TrimPrefix(d, "https://")
	d = strings.TrimPrefix(d, "http://")
	if i := strings.IndexByte(d, '/'); i >= 0 {
		d = d[:i]
	}
	return d
}

func (h *Handler) getRelationship(ctx context.Context, accountID, targetID string) *MastodonRelationship {
	rel := &MastodonRelationship{
		ID: targetID,
	}

	following, _ := h.store.IsFollowing(ctx, accountID, targetID)
	rel.Following = following

	followedBy, _ := h.store.IsFollowing(ctx, targetID, accountID)
	rel.FollowedBy = followedBy

	blocking, _ := h.store.BlockExists(ctx, accountID, targetID)
	rel.Blocking = blocking

	blockedBy, _ := h.store.BlockExists(ctx, targetID, accountID)
	rel.BlockedBy = blockedBy

	muted, _ := h.store.IsMuted(ctx, accountID, targetID)
	rel.Muting = muted

	// Reflect follow preferences (pending state, boost visibility, notify bell).
	rel.ShowingReblogs = true
	if follow, err := h.store.GetFollowByPair(ctx, accountID, targetID); err == nil {
		if follow.State == models.FollowPending {
			rel.Requested = true
		}
		rel.ShowingReblogs = follow.ShowReblogs
		rel.Notifying = follow.Notify
	}

	return rel
}

// AccountMove initiates account migration to another instance.
// POST /api/v1/accounts/{id}/move
// Body: { "target_account": "user@domain", "password": "current-password" }
// This is the Mastodon-compatible migration endpoint.
func (h *Handler) AccountMove(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	// Only the account owner can migrate (not admins via this endpoint)
	id := chi.URLParam(r, "id")
	if id != account.ID {
		writeAPIError(w, http.StatusForbidden, "you can only migrate your own account")
		return
	}
	if h.config.MigrationConfigured && !h.config.MigrationEnabled {
		writeAPIError(w, http.StatusForbidden, "account migration is disabled on this instance")
		return
	}

	params, parseMode, err := decodeMastodonParams(r, 0)
	if err != nil {
		logAPIValidationError(r, http.StatusUnprocessableEntity, "invalid request body", "parse_mode", parseMode, "parse_error", err)
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid request body")
		return
	}

	targetAcct := params.First("target_account")
	password := params.First("password")

	if targetAcct == "" || password == "" {
		writeAPIError(w, http.StatusUnprocessableEntity, "target_account and password are required")
		return
	}

	// Verify password before allowing migration
	if !auth.CheckPassword(password, account.PasswordHash) {
		slog.Warn("api: move password verification failed", "account_id", account.ID)
		writeAPIError(w, http.StatusForbidden, "invalid password")
		return
	}

	// Parse and validate target
	parts := strings.SplitN(targetAcct, "@", 2)
	if len(parts) != 2 {
		writeAPIError(w, http.StatusUnprocessableEntity, "target_account must be in username@domain format")
		return
	}
	targetDomain := parts[1]

	// Prevent moving to the same instance
	if strings.EqualFold(targetDomain, h.localDomain) {
		writeAPIError(w, http.StatusUnprocessableEntity, "cannot move to the same instance")
		return
	}
	if h.federator == nil {
		writeAPIError(w, http.StatusInternalServerError, "federation not configured")
		return
	}

	// Resolve the target account via WebFinger
	targetAccount, err := h.federator.ResolveAccount(r.Context(), targetAcct)
	if err != nil {
		slog.Warn("api: move target resolution failed", "target", targetAcct, "error", err)
		writeAPIError(w, http.StatusNotFound, "target account not found: "+err.Error())
		return
	}

	newActorURI := targetAccount.ActorURI
	if h.config.MigrationRequireTargetAlias {
		refreshedTarget, err := h.federator.RefreshActor(r.Context(), targetAccount.ActorURI, federation.ActorRefreshOptions{Force: true})
		if err != nil {
			writeAPIError(w, http.StatusUnprocessableEntity, "target account could not be refreshed for alias verification: "+err.Error())
			return
		}
		targetAccount = refreshedTarget
		if err := accountmigration.VerifyMovePair(account, targetAccount, false); err != nil {
			writeAPIError(w, http.StatusUnprocessableEntity, "target account must list this account in alsoKnownAs before moving: "+err.Error())
			return
		}
	}

	// Check cooldown (prevent rapid moves)
	if account.MovedAt != nil {
		cooldown := h.config.MigrationCooldownAfterMove
		if cooldown <= 0 {
			cooldown = 30 * 24 * time.Hour
		}
		if time.Since(*account.MovedAt) < cooldown {
			writeAPIError(w, http.StatusUnprocessableEntity, "account was already moved recently, please wait before moving again")
			return
		}
	}

	slog.Info("api: initiating account move",
		"account_id", account.ID,
		"username", account.Username,
		"from", fmt.Sprintf("https://%s/users/%s", h.domain, account.Username),
		"to", newActorURI,
	)

	// Mark the account as moved
	if err := h.store.SetAccountMoved(r.Context(), account.ID, newActorURI); err != nil {
		slog.Error("api: failed to mark account as moved", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "failed to move account")
		return
	}
	h.app.NotifyAdminsAccountMoved(r.Context(), account)

	// Build and deliver the Move activity to followers
	moveActivity := map[string]interface{}{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       account.ActorURI + "/move/" + uuid.New().String(),
		"type":     "Move",
		"actor":    account.ActorURI,
		"object":   account.ActorURI,
		"target":   newActorURI,
	}

	// Deliver to all followers asynchronously
	go func() {
		ctx := context.Background()
		if _, err := app.MigrateLocalFollowers(ctx, h.store, h.federator, account, targetAccount); err != nil {
			slog.Warn("move: failed to migrate local followers", "error", err)
		}
		inboxes, err := h.store.GetFollowerDestinations(ctx, account.ID)
		if err != nil {
			slog.Error("move: failed to get follower destinations", "error", err)
			return
		}
		for _, inbox := range inboxes {
			host := inboxHost(inbox)

			// Circuit breaker: check instance reachability
			if host != "" {
				reachable, err := h.store.IsInstanceReachable(ctx, host)
				if err != nil || !reachable {
					continue
				}
			}

			activityJSON, _ := json.Marshal(moveActivity)
			payload, _ := json.Marshal(struct {
				Inbox    string          `json:"inbox"`
				Activity json.RawMessage `json:"activity"`
			}{
				Inbox:    inbox,
				Activity: activityJSON,
			})
			job := &models.Job{
				Kind:        "delivery",
				Payload:     string(payload),
				RunAt:       time.Now(),
				MaxAttempts: models.DeliveryMaxAttempts,
				State:       "queued",
			}
			if err := h.store.EnqueueJob(ctx, job); err != nil {
				slog.Warn("move: enqueue delivery failed", "inbox", inbox, "error", err)
			}
		}

		// Also deliver to the target server's shared inbox for alias setup
		if targetAccount.InboxURI != "" {
			targetInbox := targetAccount.InboxURI
			if targetAccount.SharedInboxURI != nil && *targetAccount.SharedInboxURI != "" {
				targetInbox = *targetAccount.SharedInboxURI
			}
			if err := h.federator.Deliver(ctx, targetInbox, moveActivity); err != nil {
				slog.Warn("move: failed to deliver to target inbox", "target", targetInbox, "error", err)
			}
		}

		slog.Info("move: delivered Move activity to followers",
			"account", account.Username,
			"follower_count", len(inboxes),
		)
	}()

	// Return the target account info
	result := h.accountToMastodon(targetAccount, account, false)
	writeAPIJSON(w, http.StatusOK, result)
}

// inboxHost extracts the hostname used by instance records from an inbox URI.
func inboxHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// AccountImportFollows imports follows from an old account into the local account.
// POST /api/v1/accounts/{id}/import_follows
// Body (optional): { "old_account": "user@oldserver.example.com" }
// If no body, uses the account's alsoKnownAs field.
func (h *Handler) AccountImportFollows(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	id := chi.URLParam(r, "id")
	if id != account.ID {
		writeAPIError(w, http.StatusForbidden, "you can only import follows for your own account")
		return
	}

	if account.Domain != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "follow import only available for local accounts")
		return
	}
	if h.config.MigrationConfigured && !h.config.MigrationEnabled {
		writeAPIError(w, http.StatusForbidden, "account migration is disabled on this instance")
		return
	}

	// Determine the old account URI
	var oldActorURI string

	// Check for manual old_account in request body (if present)
	if r.Body != nil && r.ContentLength != 0 {
		params, _, _ := decodeMastodonParams(r, 0)
		if manualAcct := params.First("old_account"); manualAcct != "" {
			// If it's a user@domain handle, resolve it via WebFinger
			if !strings.HasPrefix(manualAcct, "http://") && !strings.HasPrefix(manualAcct, "https://") {
				if h.federator == nil {
					writeAPIError(w, http.StatusInternalServerError, "federation not configured")
					return
				}
				resolved, err := h.federator.ResolveAccount(r.Context(), manualAcct)
				if err != nil {
					writeAPIError(w, http.StatusNotFound, "old account not found: "+err.Error())
					return
				}
				oldActorURI = resolved.ActorURI
			} else {
				oldActorURI = manualAcct
			}
			// Store the alias for future reference
			if account.AlsoKnownAs == "" {
				_ = h.store.SetAccountAlias(r.Context(), account.ID, oldActorURI)
			}
		}
	}

	if oldActorURI == "" && account.AlsoKnownAs != "" {
		oldActorURI = account.AlsoKnownAs
	}

	if oldActorURI == "" {
		writeAPIError(w, http.StatusUnprocessableEntity, "no old account specified and alsoKnownAs not set")
		return
	}

	if h.federator == nil {
		writeAPIError(w, http.StatusInternalServerError, "federation not configured")
		return
	}

	slog.Info("api: starting follow import",
		"account_id", account.ID,
		"username", account.Username,
		"old_actor", oldActorURI,
	)

	// Create a FollowImporter and run the import
	importer := app.NewFollowImporterWithOptions(h.store, h.federator, h.domain, app.MigrationOptions{
		MaxFollows: h.config.MigrationMaxFollowsImport,
		ImportRate: h.config.MigrationImportRate,
	})
	bgCtx := context.Background()

	// Run import in background, return accepted immediately
	go func() {
		imported, err := importer.ImportFollows(bgCtx, account, oldActorURI)
		if err != nil {
			slog.Error("api: follow import failed",
				"account_id", account.ID,
				"old_actor", oldActorURI,
				"error", err,
			)
			return
		}
		slog.Info("api: follow import completed",
			"account_id", account.ID,
			"imported", imported,
		)
	}()

	writeAPIJSON(w, http.StatusAccepted, map[string]interface{}{
		"status":      "import_started",
		"old_account": oldActorURI,
		"message":     "Follow import started. This may take several minutes.",
	})
}

// AccountExportFollowsCSV exports the user's following list as Mastodon-compatible CSV.
// GET /api/v1/accounts/{id}/following.csv
func (h *Handler) AccountExportFollowsCSV(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	id := chi.URLParam(r, "id")
	if id != account.ID {
		writeAPIError(w, http.StatusForbidden, "you can only export your own follows")
		return
	}

	// Fetch all accounts this user follows
	following, err := h.store.GetFollowingToMigrate(r.Context(), account.ID, 0)
	if err != nil {
		slog.Error("api: export follows failed", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error exporting follows")
		return
	}

	var buf strings.Builder
	buf.WriteString("Account address,Show boosts,Notify on new posts,Languages\n")

	for _, target := range following {
		acct := target.Username
		if target.Domain != nil && *target.Domain != "" {
			acct = target.Username + "@" + *target.Domain
		}
		buf.WriteString(fmt.Sprintf("%s,true,false,\n", acct))
	}

	filename := fmt.Sprintf("%s_following.csv", account.Username)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(buf.String()))
}

// AccountImportFollowsCSV imports follows from a Mastodon-compatible CSV file.
// POST /api/v1/accounts/{id}/import_follows_csv (multipart form with "csv_file" field)
func (h *Handler) AccountImportFollowsCSV(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	id := chi.URLParam(r, "id")
	if id != account.ID {
		writeAPIError(w, http.StatusForbidden, "you can only import follows for your own account")
		return
	}
	if h.config.MigrationConfigured && !h.config.MigrationEnabled {
		writeAPIError(w, http.StatusForbidden, "account migration is disabled on this instance")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 11<<20)
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid form data")
		return
	}

	file, _, err := r.FormFile("csv_file")
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "missing csv_file field")
		return
	}
	defer file.Close()
	defer r.MultipartForm.RemoveAll()

	handles, err := app.ParseFollowCSV(file, h.config.MigrationMaxFollowsImport)
	if err != nil {
		if errors.Is(err, app.ErrFollowImportLimit) {
			writeAPIError(w, http.StatusBadRequest, err.Error())
		} else {
			writeAPIError(w, http.StatusBadRequest, "invalid follows CSV")
		}
		return
	}

	if len(handles) == 0 {
		writeAPIError(w, http.StatusBadRequest, "no valid account handles found in CSV")
		return
	}

	imp, err := app.RequestFollowImport(r.Context(), h.store, account, handles)
	if err != nil {
		if errors.Is(err, storage.ErrAccountImportActive) {
			writeAPIError(w, http.StatusConflict, err.Error())
		} else {
			slog.Error("api: request follows import", "account_id", account.ID, "error", err)
			writeAPIError(w, http.StatusInternalServerError, "could not save follows import")
		}
		return
	}
	writeAPIJSON(w, http.StatusAccepted, map[string]interface{}{
		"status": "import_started", "import_id": imp.ID, "handles": len(handles),
		"status_url": fmt.Sprintf("/api/v1/accounts/%s/import_follows_csv/%s", account.ID, imp.ID),
		"message":    "Your follows import is queued and will resume after a restart.",
	})
}

func (h *Handler) AccountFollowImportGet(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	if chi.URLParam(r, "id") != account.ID {
		writeAPIError(w, http.StatusForbidden, "you can only view your own import")
		return
	}
	imp, err := h.store.GetAccountImport(r.Context(), chi.URLParam(r, "import_id"), account.ID)
	if errors.Is(err, sql.ErrNoRows) {
		writeAPIError(w, http.StatusNotFound, "import not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "could not read import")
		return
	}
	var result models.AccountImportResult
	if err := json.Unmarshal([]byte(imp.ResultJSON), &result); err != nil || result.FollowImport == nil {
		writeAPIError(w, http.StatusNotFound, "CSV import not found")
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeAPIJSON(w, http.StatusOK, map[string]any{
		"id": imp.ID, "state": imp.State, "progress": result.FollowImport,
		"following": result.Following, "updated_at": imp.UpdatedAt,
	})
}

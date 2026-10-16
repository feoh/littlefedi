package webui

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/accounts"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/app"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/auth"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/blogslug"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/blogtheme"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/content"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/federation"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/i18n"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mail"
	medialib "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media"
	accountmigration "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/migration"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	pushpkg "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/push"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/statusfilter"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// Settings Page

// SettingsPage renders the settings page.
func (h *Handler) SettingsPage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
		return
	}
	unreadCount, _ := h.store.CountUnreadNotificationsFiltered(r.Context(), account.ID, notificationExcludedTypes(account))
	filters, err := h.store.ListFilters(r.Context(), account.ID)
	if err != nil {
		h.renderError(w, r, "Error fetching filters")
		return
	}

	data := &SettingsData{
		SensitiveRelationshipNotificationsBlocked: h.cfg.Instance.BlockSensitiveRelationshipNotifications,
		Title:               "Settings",
		Account:             account,
		UnreadCount:         unreadCount,
		CSRFToken:           getCSRFToken(r.Context()),
		Domain:              h.localDomain,
		NotificationOptions: notificationOptionsFor(account, h.resolveLocale(r, account)),
		SMTPConfigured:      h.mailer != nil,
		CSPNonce:            r.Header.Get("X-littleFedi-CSP-Nonce"),
		Theme:               h.resolveTheme(r, account).ID,
		Appearance:          h.resolveAppearance(r, account),
		CustomCSS:           account.CustomCSS,
		BlogTheme:           account.BlogTheme,
		BlogCustomCSS:       account.BlogCustomCSS,
		BlogRebuildSent:     r.URL.Query().Get("blog_rebuild") == "queued",
		Filters:             filters,
		FilterFlash:         r.URL.Query().Get("filter"),
	}
	switch action := r.URL.Query().Get("filters"); action {
	case "created", "saved", "deleted":
		data.FilterNotice = h.pageTitle(r, "settings.filters.notice_"+action)
	}
	switch code := r.URL.Query().Get("custom_css_error"); code {
	case "too_large", "markup", "encoding", "read":
		data.CustomCSSError = h.pageTitle(r, "settings.customcss.error."+code)
	}
	// The blog stylesheet is checked by the same validator, so it reuses the
	// same messages rather than translating four near-identical strings.
	switch code := r.URL.Query().Get("blog_css_error"); code {
	case "too_large", "markup", "encoding", "read":
		data.BlogCSSError = h.pageTitle(r, "settings.customcss.error."+code)
	case "theme":
		data.BlogCSSError = h.pageTitle(r, "settings.blogappearance.error.theme")
	}
	if h.vapidPublicKey != "" {
		data.VAPIDPublicKey = h.vapidPublicKey
		data.WebPushSubscription, _ = h.store.GetPushSubscriptionBySessionID(r.Context(), getSessionID(r.Context()))
		if data.WebPushSubscription != nil {
			data.WebPushEndpoint = data.WebPushSubscription.Endpoint
			data.CurrentPushID = data.WebPushSubscription.ID
		}
		data.PushSubscriptions, _ = h.store.ListPushSubscriptionsByAccountID(r.Context(), account.ID)
	}
	data.Import, _ = h.store.GetLatestAccountImport(r.Context(), account.ID)
	if data.Import != nil && data.Import.ResultJSON != "" {
		data.ImportResult = &models.AccountImportResult{}
		if err := json.Unmarshal([]byte(data.Import.ResultJSON), data.ImportResult); err != nil {
			data.ImportResult = nil
		}
	}
	switch code := r.URL.Query().Get("csv_error"); code {
	case "parse", "limit", "active", "save":
		data.ImportError = h.pageTitle(r, "settings.migration.csv_error."+code)
	}
	switch code := r.URL.Query().Get("alias_error"); code {
	case "missing", "resolve", "save":
		data.ImportError = h.pageTitle(r, "settings.migration.alias_error."+code)
	}

	h.renderPage(w, r, h.pageTitle(r, "nav.settings"), data, "settings.html")
}

var filterContexts = []string{"home", "notifications", "public", "thread", "account"}

func settingsFilterFromRequest(r *http.Request, accountID, id string) (*models.Filter, error) {
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	filter := &models.Filter{ID: id, AccountID: accountID, Title: strings.TrimSpace(r.FormValue("title")), FilterAction: r.FormValue("filter_action"), Statuses: []any{}}
	if filter.Title == "" || (filter.FilterAction != "warn" && filter.FilterAction != "hide") {
		return nil, errors.New("invalid filter")
	}
	if r.FormValue("all_posts") == "on" {
		filter.Context = append(filter.Context, filterContexts...)
	} else {
		for _, context := range filterContexts {
			if r.FormValue("context_"+context) == "on" {
				filter.Context = append(filter.Context, context)
			}
		}
	}
	if len(filter.Context) == 0 {
		return nil, errors.New("missing context")
	}
	for _, keyword := range strings.FieldsFunc(r.FormValue("keywords"), func(r rune) bool { return r == '\n' || r == ',' }) {
		if keyword = strings.TrimSpace(keyword); keyword != "" {
			filter.Keywords = append(filter.Keywords, models.FilterKeyword{Keyword: keyword, WholeWord: true})
		}
	}
	filter.Languages = statusfilter.NormalizeLanguages(r.Form["languages"])
	filter.InvertLanguages = r.FormValue("invert_languages") == "on"
	filter.ShowUnknownLanguages = r.FormValue("show_unknown_languages") == "on"
	if len(filter.Keywords) == 0 && len(filter.Languages) == 0 {
		return nil, errors.New("missing criteria")
	}
	if raw := r.FormValue("expires_at"); raw != "" {
		expiresAt, err := time.ParseInLocation("2006-01-02T15:04", raw, time.Local)
		if err != nil {
			return nil, err
		}
		filter.ExpiresAt = &expiresAt
	}
	return filter, nil
}

func (h *Handler) SettingsFilterCreate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	filter, err := settingsFilterFromRequest(r, account.ID, uuid.NewString())
	if err != nil || h.store.CreateFilter(r.Context(), filter) != nil {
		h.renderError(w, r, "Error saving filter")
		return
	}
	h.app.InvalidateFilterPages(account.ID)
	h.redirectToSettings(w, r, "/settings?filters=created&filter="+url.QueryEscape(filter.ID)+"#filters")
}

func (h *Handler) SettingsFilterUpdate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	filter, err := settingsFilterFromRequest(r, account.ID, chi.URLParam(r, "id"))
	if err != nil || h.store.UpdateFilter(r.Context(), filter) != nil {
		h.renderError(w, r, "Error saving filter")
		return
	}
	h.app.InvalidateFilterPages(account.ID)
	h.redirectToSettings(w, r, "/settings?filters=saved&filter="+url.QueryEscape(filter.ID)+"#filters")
}

func (h *Handler) SettingsFilterDelete(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if err := h.store.DeleteFilter(r.Context(), chi.URLParam(r, "id"), account.ID); err != nil {
		h.renderError(w, r, "Error deleting filter")
		return
	}
	h.app.InvalidateFilterPages(account.ID)
	h.redirectToSettings(w, r, "/settings?filters=deleted#filters")
}

// SettingsProfile handles profile update.
func (h *Handler) SettingsProfile(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	maxSize := h.maxMediaSize
	if maxSize <= 0 {
		maxSize = 10 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSize+(1<<20))
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(maxSize); err != nil {
			h.renderError(w, r, "Invalid form data")
			return
		}
	} else {
		if err := r.ParseForm(); err != nil {
			h.renderError(w, r, "Invalid form data")
			return
		}
	}

	displayName := r.FormValue("display_name")
	note := r.FormValue("note")
	account.RSSFeedEnabled = r.FormValue("rss_feed_enabled") == "on"

	// Apply avatar/header changes to the account struct FIRST,
	// so the single UpdateProfile call below saves and federates
	// with the correct data (not the stale cached values).

	// Handle avatar upload (creates media record, returns URL, or "" if none)
	if avatarURL, err := h.saveProfileImage(r, account, "avatar", maxSize); err == nil && avatarURL != "" {
		account.Avatar = avatarURL
	} else if r.FormValue("remove_avatar") == "1" && account.Avatar != "" {
		account.Avatar = ""
	}

	// Handle header upload
	if headerURL, err := h.saveProfileImage(r, account, "header", maxSize); err == nil && headerURL != "" {
		account.Header = headerURL
	} else if r.FormValue("remove_header") == "1" && account.Header != "" {
		account.Header = ""
	}

	// Now save everything (display_name, note, avatar, header) and federate
	if err := h.app.UpdateProfile(r.Context(), account, displayName, note); err != nil {
		slog.Error("webui: update profile error", "error", err)
		h.renderError(w, r, "Error updating profile")
		return
	}
	h.rssCache.Delete(account.ID)

	// For htmx requests, use the HX-Redirect header so htmx navigates
	// properly instead of relying on XHR's transparent 302 redirect following,
	// which can cause issues with URL and content swap.
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/settings")
		w.WriteHeader(http.StatusOK)
		return
	}

	http.Redirect(w, r, "/settings", http.StatusFound)
}

// saveProfileImage saves an avatar or header upload and returns the URL path.
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
		return "", fmt.Errorf("%s file too large", fieldName)
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
	safeFilename := medialib.MediaFilename(header.Filename, mimeType, h.renameMedia)
	err = medialib.StoreUpload(r.Context(), h.mediaStore, file, header.Size, media, safeFilename, h.stripEXIF)
	if err != nil {
		_ = h.store.DeleteMediaAttachment(r.Context(), media.ID)
		return "", err
	}
	if err := h.store.UpdateMediaAttachment(r.Context(), media); err != nil {
		return "", err
	}

	return media.URL, nil
}

// SettingsPasswordPage renders the password change form.
func (h *Handler) SettingsPasswordPage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
		return
	}

	unreadCount, _ := h.store.CountUnreadNotificationsFiltered(r.Context(), account.ID, notificationExcludedTypes(account))

	var errMsg string
	switch code := r.URL.Query().Get("error"); code {
	case "short":
		errMsg = h.pageTitle(r, "password.error.short", minPasswordLen(h.cfg))
	case "current", "mismatch", "internal":
		errMsg = h.pageTitle(r, "password.error."+code)
	}

	data := &SettingsData{
		Title:             "Change Password - Settings",
		Account:           account,
		UnreadCount:       unreadCount,
		CSRFToken:         getCSRFToken(r.Context()),
		Error:             errMsg,
		MinPasswordLength: minPasswordLen(h.cfg),
	}

	h.renderPage(w, r, h.pageTitle(r, "page.change_password_settings"), data, "settings_password.html")
}

// SettingsPasswordChange processes a password change.
func (h *Handler) SettingsPasswordChange(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if err := r.ParseForm(); err != nil {
		h.renderError(w, r, "Invalid form data")
		return
	}

	currentPassword := r.FormValue("current_password")
	newPassword := r.FormValue("new_password")
	confirmPassword := r.FormValue("confirm_password")

	// Validate current password
	if !h.allowLogin(r, account, currentPassword) {
		slog.Warn("webui: password change rejected, current password mismatch", "username", account.Username)
		http.Redirect(w, r, "/settings/password?error=current", http.StatusFound)
		return
	}

	// Validate new password
	if err := accounts.ValidatePassword(newPassword, minPasswordLen(h.cfg)); err != nil {
		http.Redirect(w, r, "/settings/password?error=short", http.StatusFound)
		return
	}
	if newPassword != confirmPassword {
		http.Redirect(w, r, "/settings/password?error=mismatch", http.StatusFound)
		return
	}

	// Hash and store
	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		slog.Error("webui: password hash error", "error", err)
		http.Redirect(w, r, "/settings/password?error=internal", http.StatusFound)
		return
	}

	account.PasswordHash = hash
	if err := h.store.UpdateAccount(r.Context(), account); err != nil {
		slog.Error("webui: password update error", "error", err)
		http.Redirect(w, r, "/settings/password?error=internal", http.StatusFound)
		return
	}

	slog.Info("webui: password changed", "username", account.Username)
	http.Redirect(w, r, "/settings?password-changed=1", http.StatusFound)
}

func (h *Handler) SettingsMFAPage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
		return
	}
	data := map[string]interface{}{"Account": account, "CSRFToken": getCSRFToken(r.Context()), "Configured": h.cfg != nil && h.cfg.Auth.MFAEncryptionKey != ""}
	if !account.MFAEnabled && h.cfg != nil && h.cfg.Auth.MFAEncryptionKey != "" {
		key, err := totp.Generate(totp.GenerateOpts{Issuer: h.cfg.Instance.Name, AccountName: account.Username + "@" + h.localDomain})
		if err != nil {
			h.renderError(w, r, "Could not generate an MFA secret")
			return
		}
		data["Secret"], data["OTPAuthURL"] = key.Secret(), key.URL()
	}
	w.Header().Set("Cache-Control", "private, no-store")
	h.renderPage(w, r, "Multi-factor authentication", data, "settings_mfa.html")
}

func (h *Handler) SettingsMFAEnroll(w http.ResponseWriter, r *http.Request) {
	account, err := h.store.GetAccount(r.Context(), getAuthAccount(r.Context()).ID)
	if err != nil || h.cfg == nil || h.cfg.Auth.MFAEncryptionKey == "" || !h.allowLogin(r, account, r.FormValue("current_password")) {
		http.Error(w, "Invalid password or MFA configuration", http.StatusForbidden)
		return
	}
	secret := strings.TrimSpace(r.FormValue("secret"))
	if account.MFAEnabled || !auth.ValidateTOTP(secret, r.FormValue("mfa_code")) {
		http.Error(w, "Invalid authenticator code", http.StatusForbidden)
		return
	}
	encrypted, err := auth.EncryptTOTPSecret(h.cfg.Auth.MFAEncryptionKey, secret)
	if err != nil {
		http.Error(w, "Could not encrypt MFA secret", http.StatusInternalServerError)
		return
	}
	plain, hashes, err := auth.GenerateRecoveryCodes()
	if err != nil || h.store.SetAccountMFA(r.Context(), account.ID, encrypted, hashes) != nil {
		http.Error(w, "Could not enable MFA", http.StatusInternalServerError)
		return
	}
	h.recordMFAEvent(r.Context(), account, "mfa_enable")
	h.renderRecoveryCodes(w, r, plain)
}

func (h *Handler) SettingsMFADisable(w http.ResponseWriter, r *http.Request) {
	account, err := h.store.GetAccount(r.Context(), getAuthAccount(r.Context()).ID)
	if err != nil || !h.allowLogin(r, account, r.FormValue("current_password")) || !h.verifyMFA(r.Context(), account, r.FormValue("mfa_code")) {
		http.Error(w, "Invalid password or second factor", http.StatusForbidden)
		return
	}
	if err := h.store.DisableAccountMFA(r.Context(), account.ID); err != nil {
		http.Error(w, "Could not disable MFA", http.StatusInternalServerError)
		return
	}
	h.recordMFAEvent(r.Context(), account, "mfa_disable")
	http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
}

func (h *Handler) SettingsMFARegenerateRecovery(w http.ResponseWriter, r *http.Request) {
	account, err := h.store.GetAccount(r.Context(), getAuthAccount(r.Context()).ID)
	if err != nil || !h.allowLogin(r, account, r.FormValue("current_password")) || !h.verifyMFA(r.Context(), account, r.FormValue("mfa_code")) {
		http.Error(w, "Invalid password or second factor", http.StatusForbidden)
		return
	}
	plain, hashes, err := auth.GenerateRecoveryCodes()
	if err != nil || h.store.ReplaceMFARecoveryCodes(r.Context(), account.ID, hashes) != nil {
		http.Error(w, "Could not regenerate recovery codes", http.StatusInternalServerError)
		return
	}
	h.recordMFAEvent(r.Context(), account, "mfa_recovery_regenerated")
	h.renderRecoveryCodes(w, r, plain)
}

// SettingsExportPage shows the most recent private account export and its
// short-lived download state.
func (h *Handler) SettingsExportPage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
		return
	}
	export, _ := h.store.GetLatestAccountExport(r.Context(), account.ID)
	h.renderPage(w, r, "Export your data", map[string]any{
		"Account": account, "CSRFToken": getCSRFToken(r.Context()), "Export": export,
		"Error": r.URL.Query().Get("error"),
	}, "settings_export.html")
}

func (h *Handler) SettingsExportRequest(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil || r.ParseForm() != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if !h.allowLogin(r, account, r.FormValue("current_password")) ||
		(account.MFAEnabled && !h.verifyMFA(r.Context(), account, r.FormValue("mfa_code"))) {
		http.Redirect(w, r, "/settings/export?error=authentication", http.StatusFound)
		return
	}
	_, _, err := app.RequestAccountExport(r.Context(), h.store, account)
	if errors.Is(err, app.ErrAccountExportRateLimited) {
		http.Redirect(w, r, "/settings/export?error=rate", http.StatusFound)
		return
	}
	if err != nil {
		slog.Error("webui: request account export", "error", err)
		http.Redirect(w, r, "/settings/export?error=internal", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/settings/export", http.StatusFound)
}

func (h *Handler) SettingsExportDownload(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil || h.mediaStore == nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	id := chi.URLParam(r, "id")
	export, err := h.store.GetAccountExport(r.Context(), id, account.ID)
	if err != nil || export.State != "ready" || export.StorageKey == "" {
		http.Error(w, "Export is not available", http.StatusNotFound)
		return
	}
	token, tokenHash := generateSessionToken()
	if err := h.store.SetAccountExportDownloadToken(r.Context(), id, account.ID, tokenHash); err != nil {
		http.Error(w, "Export is not available", http.StatusGone)
		return
	}
	reader, object, err := h.mediaStore.Open(r.Context(), export.StorageKey)
	if err != nil {
		slog.Error("webui: open account export", "export_id", id, "error", err)
		http.Error(w, "Export is temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	defer reader.Close()
	if _, err := h.store.ConsumeAccountExportDownload(r.Context(), id, account.ID, sha256Hex(token), time.Now().UTC()); err != nil {
		http.Error(w, "Export download was already used or expired", http.StatusGone)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="littlefedi-%s-export.zip"`, account.Username))
	w.Header().Set("Content-Length", strconv.FormatInt(object.Size, 10))
	w.Header().Set("Cache-Control", "private, no-store")
	if _, err := io.Copy(w, reader); err != nil {
		slog.Warn("webui: stream account export", "export_id", id, "error", err)
	}
}

// SettingsImportPage shows the selected import, or the latest one after an upload.
func (h *Handler) SettingsImportPage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
		return
	}
	var imp *models.AccountImport
	if id := r.URL.Query().Get("import_id"); id != "" {
		var err error
		imp, err = h.store.GetAccountImport(r.Context(), id, account.ID)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "Import not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "Could not read import", http.StatusInternalServerError)
			return
		}
	} else {
		imp, _ = h.store.GetLatestAccountImport(r.Context(), account.ID)
	}
	var result *models.AccountImportResult
	if imp != nil && imp.ResultJSON != "" {
		result = &models.AccountImportResult{}
		if err := json.Unmarshal([]byte(imp.ResultJSON), result); err != nil {
			result = nil
		}
	}
	h.renderPage(w, r, h.pageTitle(r, "settings.import.heading"), map[string]any{
		"Account": account, "CSRFToken": getCSRFToken(r.Context()), "Import": imp, "ImportResult": result,
		"Error": r.URL.Query().Get("error"),
	}, "settings_import.html")
}

// SettingsImportUpload accepts the exported archive, reverifies the
// password/MFA the same way export does, and enqueues the import job.
func (h *Handler) SettingsImportUpload(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, app.AccountImportMaxUploadSize+(1<<20))
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Redirect(w, r, "/settings/import?error=file", http.StatusFound)
		return
	}
	if !h.allowLogin(r, account, r.FormValue("current_password")) ||
		(account.MFAEnabled && !h.verifyMFA(r.Context(), account, r.FormValue("mfa_code"))) {
		http.Redirect(w, r, "/settings/import?error=authentication", http.StatusFound)
		return
	}
	file, header, err := r.FormFile("import_file")
	if err != nil {
		http.Redirect(w, r, "/settings/import?error=file", http.StatusFound)
		return
	}
	defer file.Close()
	_, err = app.RequestAccountImport(r.Context(), h.store, h.mediaStore, account, file, header.Size)
	if errors.Is(err, app.ErrAccountImportRateLimited) {
		http.Redirect(w, r, "/settings/import?error=rate", http.StatusFound)
		return
	}
	if err != nil {
		slog.Error("webui: request account import", "error", err)
		http.Redirect(w, r, "/settings/import?error=internal", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/settings/import", http.StatusFound)
}

func (h *Handler) SettingsDeletePage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
		return
	}
	h.renderPage(w, r, "Delete account", map[string]any{
		"Account": account, "CSRFToken": getCSRFToken(r.Context()), "Error": r.URL.Query().Get("error"),
	}, "settings_delete.html")
}

func (h *Handler) SettingsDeleteSubmit(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil || r.ParseForm() != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.FormValue("confirm_username") != account.Username {
		http.Redirect(w, r, "/settings/delete?error=confirmation", http.StatusFound)
		return
	}
	if !h.allowLogin(r, account, r.FormValue("current_password")) ||
		(account.MFAEnabled && !h.verifyMFA(r.Context(), account, r.FormValue("mfa_code"))) {
		http.Redirect(w, r, "/settings/delete?error=authentication", http.StatusFound)
		return
	}
	run, _, err := h.app.RequestAccountDeletion(r.Context(), account, account)
	if err != nil {
		slog.Warn("webui: self-service account deletion rejected", "account_id", account.ID, "error", err)
		http.Redirect(w, r, "/settings/delete?error=internal", http.StatusFound)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: h.secureCookie(r), SameSite: http.SameSiteLaxMode})
	h.renderPage(w, r, "Deletion requested", map[string]any{"DeletionRun": run}, "deletion_requested.html")
}

func (h *Handler) recordMFAEvent(ctx context.Context, account *models.Account, action string) {
	if err := h.store.CreateModerationLog(ctx, &models.ModerationLog{AdminAccountID: account.ID, TargetType: "account", TargetID: account.ID, Action: action}); err != nil {
		slog.Error("webui: MFA audit failed", "account_id", account.ID, "action", action, "error", err)
	}
}

func (h *Handler) renderRecoveryCodes(w http.ResponseWriter, r *http.Request, codes []string) {
	w.Header().Set("Cache-Control", "private, no-store")
	h.renderPage(w, r, "MFA recovery codes", map[string]interface{}{"Codes": codes}, "settings_mfa_recovery.html")
}

// Session Management

// sessionRecord holds display info for a session.
type sessionRecord struct {
	ID        string
	UserAgent string
	IP        string
	CreatedAt string
	ExpiresAt string
	IsCurrent bool
}

// SettingsSessionsPage renders the active sessions list.
func (h *Handler) SettingsSessionsPage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
		return
	}

	sessions, err := h.store.ListSessionsByAccount(r.Context(), account.ID)
	if err != nil {
		slog.Error("webui: list sessions error", "error", err)
		sessions = []*models.Session{}
	}

	// Get the current session token to mark it
	currentTokenHash := ""
	if cookie, err := r.Cookie("session"); err == nil {
		currentTokenHash = sha256Hex(cookie.Value)
	}

	records := make([]sessionRecord, 0, len(sessions))
	for _, s := range sessions {
		records = append(records, sessionRecord{
			ID:        s.ID,
			UserAgent: s.UserAgent,
			IP:        s.IPAddress,
			CreatedAt: s.CreatedAt.Format("Jan 2, 2006, 15:04"),
			ExpiresAt: s.ExpiresAt.Format("Jan 2, 2006, 15:04"),
			IsCurrent: s.TokenHash == currentTokenHash,
		})
	}

	h.renderPage(w, r, h.pageTitle(r, "page.sessions_settings"), map[string]interface{}{
		"Account":   account,
		"Sessions":  records,
		"CSRFToken": getCSRFToken(r.Context()),
	}, "settings_sessions.html")
}

// SettingsModerationPage lists the user's blocked accounts, muted accounts and
// blocked domains, with controls to lift each.
func (h *Handler) SettingsModerationPage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
		return
	}

	blocked, err := h.store.ListBlocks(r.Context(), account.ID, nil, 200)
	if err != nil {
		slog.Error("webui: list blocks error", "error", err)
		blocked = []*models.Account{}
	}
	muted, err := h.store.ListMutes(r.Context(), account.ID, nil, 200)
	if err != nil {
		slog.Error("webui: list mutes error", "error", err)
		muted = []*models.Account{}
	}
	domains, err := h.store.ListAccountDomainBlocks(r.Context(), account.ID)
	if err != nil {
		slog.Error("webui: list domain blocks error", "error", err)
		domains = []string{}
	}
	h.app.LoadAccountsEmojis(r.Context(), blocked)
	h.app.LoadAccountsEmojis(r.Context(), muted)

	h.renderPage(w, r, h.pageTitle(r, "page.moderation_settings"), map[string]interface{}{
		"Account":         account,
		"BlockedAccounts": blocked,
		"MutedAccounts":   muted,
		"BlockedDomains":  domains,
		"CSRFToken":       getCSRFToken(r.Context()),
	}, "settings_moderation.html")
}

// SettingsDomainBlockCreate blocks a domain for the user from the settings page.
func (h *Handler) SettingsDomainBlockCreate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	domain := normalizeDomainParam(r.FormValue("domain"))
	if domain != "" {
		if err := h.store.CreateAccountDomainBlock(r.Context(), account.ID, domain); err != nil {
			slog.Error("webui: create domain block error", "error", err)
		}
	}
	http.Redirect(w, r, "/settings/moderation", http.StatusFound)
}

// SettingsDomainBlockDelete unblocks a domain for the user.
func (h *Handler) SettingsDomainBlockDelete(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	domain := normalizeDomainParam(r.FormValue("domain"))
	if domain != "" {
		if err := h.store.DeleteAccountDomainBlock(r.Context(), account.ID, domain); err != nil {
			slog.Error("webui: delete domain block error", "error", err)
		}
	}
	http.Redirect(w, r, "/settings/moderation", http.StatusFound)
}

// SettingsSessionRevoke revokes a specific session.
func (h *Handler) SettingsSessionRevoke(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	sessionID := chi.URLParam(r, "id")
	if sessionID == "" {
		http.Redirect(w, r, "/settings/sessions", http.StatusFound)
		return
	}
	owned := false
	if sessions, err := h.store.ListSessionsByAccount(r.Context(), account.ID); err == nil {
		for _, session := range sessions {
			if session.ID == sessionID {
				owned = true
				break
			}
		}
	}
	if !owned {
		http.NotFound(w, r)
		return
	}

	// Don't allow revoking the current session (already hidden in the UI, checked here too)
	if cookie, err := r.Cookie("session"); err == nil {
		currentHash := sha256Hex(cookie.Value)
		session, err := h.store.GetSessionByTokenHash(r.Context(), currentHash)
		if err == nil && session.ID == sessionID {
			http.Redirect(w, r, "/settings/sessions", http.StatusFound)
			return
		}
	}

	if err := h.store.DeleteSession(r.Context(), sessionID); err != nil {
		slog.Error("webui: revoke session error", "error", err)
	}

	http.Redirect(w, r, "/settings/sessions", http.StatusFound)
}

// Pruning Settings

// prunePreviewLimit caps how many matching posts the settings page shows. The
// point is to make the rules concrete, not to list every candidate.
const prunePreviewLimit = 5

// prunePreviewPost is one post the current settings would delete on the next
// housekeeping run.
type prunePreviewPost struct {
	Preview   string
	CreatedAt string
	Likes     int
	Boosts    int
}

// effectivePruneSettings resolves the thresholds housekeeping would apply to
// this account: the server values, each overridden by the account's own when
// set. Housekeeper.pruneOwnStatuses holds the same logic and remains the
// authority; this copy exists so the settings page can show what it would do.
func (h *Handler) effectivePruneSettings(account *models.Account) (days, minLikes, minBoosts int) {
	days = h.cfg.Housekeeping.PruneOwnStatusesAfterDays
	minLikes = h.cfg.Housekeeping.PruneOwnStatusesMinLikes
	minBoosts = h.cfg.Housekeeping.PruneOwnStatusesMinBoosts
	if account.PruneOwnStatusesAfterDays != nil {
		days = *account.PruneOwnStatusesAfterDays
	}
	if account.PruneOwnStatusesMinLikes != nil {
		minLikes = *account.PruneOwnStatusesMinLikes
	}
	if account.PruneOwnStatusesMinBoosts != nil {
		minBoosts = *account.PruneOwnStatusesMinBoosts
	}
	return days, minLikes, minBoosts
}

// prunePreview lists a few posts the current settings would delete. The second
// return value is false when pruning is off for this account, which is a
// different message from "on, but nothing matches".
func (h *Handler) prunePreview(ctx context.Context, account *models.Account) ([]prunePreviewPost, bool) {
	days, minLikes, minBoosts := h.effectivePruneSettings(account)
	if days <= 0 {
		return nil, false
	}
	statuses, err := h.store.GetLocalStatusesOlderThanForAccount(ctx, account.ID, days, minLikes, minBoosts, prunePreviewLimit)
	if err != nil {
		slog.Error("webui: prune preview error", "error", err)
		return nil, true
	}
	preview := make([]prunePreviewPost, 0, len(statuses))
	for _, s := range statuses {
		text := truncateText(content.PlainText(s.ContentHTML), 120)
		if text == "" {
			text = truncateText(content.PlainText(s.Summary), 120)
		}
		preview = append(preview, prunePreviewPost{
			Preview:   text,
			CreatedAt: formatTime(s.CreatedAt),
			Likes:     s.FavouritesCount,
			Boosts:    s.ReblogsCount,
		})
	}
	return preview, true
}

// SettingsPruningPage renders the automatic post pruning settings form.
func (h *Handler) SettingsPruningPage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
		return
	}

	// Reload account to get the latest prune settings from DB
	fullAccount, err := h.store.GetAccount(r.Context(), account.ID)
	if err != nil {
		slog.Error("webui: get account for pruning settings", "error", err)
		http.Error(w, "Error loading settings", http.StatusInternalServerError)
		return
	}

	unreadCount, _ := h.store.CountUnreadNotificationsFiltered(r.Context(), account.ID, notificationExcludedTypes(account))

	error := r.URL.Query().Get("error")

	preview, pruningActive := h.prunePreview(r.Context(), fullAccount)

	data := map[string]interface{}{
		"Account":              fullAccount,
		"UnreadCount":          unreadCount,
		"CSRFToken":            getCSRFToken(r.Context()),
		"ServerPruneDays":      h.cfg.Housekeeping.PruneOwnStatusesAfterDays,
		"ServerPruneMinLikes":  h.cfg.Housekeeping.PruneOwnStatusesMinLikes,
		"ServerPruneMinBoosts": h.cfg.Housekeeping.PruneOwnStatusesMinBoosts,
		"PrunePreview":         preview,
		"PruningActive":        pruningActive,
		"Error":                error,
	}

	h.renderPage(w, r, h.pageTitle(r, "page.pruning_settings"), data, "settings_pruning.html")
}

// SettingsPruningUpdate saves the user's pruning preferences.
func (h *Handler) SettingsPruningUpdate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if err := r.ParseForm(); err != nil {
		h.renderError(w, r, "Invalid form data")
		return
	}

	// Reload account to get current DB state
	fullAccount, err := h.store.GetAccount(r.Context(), account.ID)
	if err != nil {
		slog.Error("webui: get account for pruning update", "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	// Parse and validate days field
	daysStr := strings.TrimSpace(r.FormValue("prune_days"))
	if daysStr != "" {
		days, err := strconv.Atoi(daysStr)
		if err != nil || days < 0 {
			http.Redirect(w, r, "/settings/pruning?error=invalid-days", http.StatusFound)
			return
		}
		// Server hard limit: only enforced when the server has actually set a positive value
		if days > 0 && h.cfg.Housekeeping.PruneOwnStatusesAfterDays > 0 && days > h.cfg.Housekeeping.PruneOwnStatusesAfterDays {
			http.Redirect(w, r, "/settings/pruning?error=exceeds-server", http.StatusFound)
			return
		}
		if days == 0 {
			fullAccount.PruneOwnStatusesAfterDays = nil // use server default
		} else {
			fullAccount.PruneOwnStatusesAfterDays = &days
		}
	} else {
		fullAccount.PruneOwnStatusesAfterDays = nil
	}

	// Parse min likes
	likesStr := strings.TrimSpace(r.FormValue("prune_min_likes"))
	if likesStr != "" {
		likes, err := strconv.Atoi(likesStr)
		if err != nil || likes < 0 {
			http.Redirect(w, r, "/settings/pruning?error=invalid-likes", http.StatusFound)
			return
		}
		if likes == 0 {
			fullAccount.PruneOwnStatusesMinLikes = nil
		} else {
			fullAccount.PruneOwnStatusesMinLikes = &likes
		}
	} else {
		fullAccount.PruneOwnStatusesMinLikes = nil
	}

	// Parse min boosts
	boostsStr := strings.TrimSpace(r.FormValue("prune_min_boosts"))
	if boostsStr != "" {
		boosts, err := strconv.Atoi(boostsStr)
		if err != nil || boosts < 0 {
			http.Redirect(w, r, "/settings/pruning?error=invalid-boosts", http.StatusFound)
			return
		}
		if boosts == 0 {
			fullAccount.PruneOwnStatusesMinBoosts = nil
		} else {
			fullAccount.PruneOwnStatusesMinBoosts = &boosts
		}
	} else {
		fullAccount.PruneOwnStatusesMinBoosts = nil
	}

	if err := h.store.UpdateAccount(r.Context(), fullAccount); err != nil {
		slog.Error("webui: update pruning settings", "error", err)
		http.Redirect(w, r, "/settings/pruning?error=save", http.StatusFound)
		return
	}

	slog.Info("webui: pruning settings updated", "username", fullAccount.Username)

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/settings/pruning")
		w.WriteHeader(http.StatusOK)
		return
	}

	http.Redirect(w, r, "/settings/pruning", http.StatusFound)
}

// Privacy Settings

// SettingsPrivacyUpdate saves the user's privacy preferences.
func (h *Handler) SettingsPrivacyUpdate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	// Reload account to get current DB state
	fullAccount, err := h.store.GetAccount(r.Context(), account.ID)
	if err != nil {
		slog.Error("webui: get account for privacy update", "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	followersChanged := fullAccount.HideFollowers != (r.FormValue("hide_followers") == "on")
	fullAccount.HideFollowers = r.FormValue("hide_followers") == "on"
	followingChanged := fullAccount.HideFollowing != (r.FormValue("hide_following") == "on")
	fullAccount.HideFollowing = r.FormValue("hide_following") == "on"
	lockedChanged := fullAccount.ManuallyApprovesFollowers != (r.FormValue("locked") == "on")
	fullAccount.ManuallyApprovesFollowers = r.FormValue("locked") == "on"
	fullAccount.Locked = fullAccount.ManuallyApprovesFollowers
	botChanged := fullAccount.IsBot != (r.FormValue("bot") == "on")
	fullAccount.IsBot = r.FormValue("bot") == "on"
	switch r.FormValue("quote_policy") {
	case "everyone", "followers", "nobody":
		fullAccount.QuotePolicy = r.FormValue("quote_policy")
	}

	if err := h.store.UpdateAccount(r.Context(), fullAccount); err != nil {
		slog.Error("webui: update privacy settings", "error", err)
		http.Error(w, "Error saving settings", http.StatusInternalServerError)
		return
	}

	h.app.InvalidateAccountComponents(fullAccount.ID)
	if lockedChanged || botChanged || followersChanged || followingChanged {
		h.app.FederateProfileUpdate(r.Context(), fullAccount)
	}

	slog.Info("webui: privacy settings updated", "username", fullAccount.Username,
		"hide_followers", fullAccount.HideFollowers,
		"hide_following", fullAccount.HideFollowing,
		"locked", fullAccount.ManuallyApprovesFollowers,
		"quote_policy", fullAccount.QuotePolicy)

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/settings")
		w.WriteHeader(http.StatusOK)
		return
	}

	http.Redirect(w, r, "/settings", http.StatusFound)
}

// SettingsLocaleUpdate saves the user's preferred UI/email/push language and
// mirrors it into the anonymous locale cookie so the choice survives logout.
func (h *Handler) SettingsLocaleUpdate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	locale := r.FormValue("locale")
	if !i18n.IsSupported(locale) {
		http.Error(w, "Unsupported locale", http.StatusBadRequest)
		return
	}

	fullAccount, err := h.store.GetAccount(r.Context(), account.ID)
	if err != nil {
		slog.Error("webui: get account for locale update", "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	fullAccount.Locale = locale

	if err := h.store.UpdateAccount(r.Context(), fullAccount); err != nil {
		slog.Error("webui: update locale", "error", err)
		http.Error(w, "Error saving settings", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     localeCookieName,
		Value:    locale,
		Path:     "/",
		MaxAge:   365 * 24 * 60 * 60,
		HttpOnly: true,
		Secure:   h.secureCookie(r),
		SameSite: http.SameSiteLaxMode,
	})

	slog.Info("webui: locale updated", "username", fullAccount.Username, "locale", locale)

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/settings")
		w.WriteHeader(http.StatusOK)
		return
	}

	http.Redirect(w, r, "/settings", http.StatusFound)
}

// SettingsAppearanceUpdate saves the user's theme and light/dark mode, and
// mirrors both into cookies so the choice survives logout - a visitor who
// picked a look should keep it on the signed-out local timeline too.
func (h *Handler) SettingsAppearanceUpdate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	theme := r.FormValue("theme")
	if !IsSupportedTheme(theme) {
		http.Error(w, "Unsupported theme", http.StatusBadRequest)
		return
	}
	appearance := r.FormValue("appearance")
	if !IsSupportedAppearance(appearance) {
		http.Error(w, "Unsupported appearance", http.StatusBadRequest)
		return
	}

	fullAccount, err := h.store.GetAccount(r.Context(), account.ID)
	if err != nil {
		slog.Error("webui: get account for appearance update", "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	fullAccount.Theme = theme
	fullAccount.Appearance = appearance

	if err := h.store.UpdateAccount(r.Context(), fullAccount); err != nil {
		slog.Error("webui: update appearance", "error", err)
		http.Error(w, "Error saving settings", http.StatusInternalServerError)
		return
	}

	for name, value := range map[string]string{themeCookieName: theme, appearanceCookieName: appearance} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    value,
			Path:     "/",
			MaxAge:   365 * 24 * 60 * 60,
			HttpOnly: true,
			Secure:   h.secureCookie(r),
			SameSite: http.SameSiteLaxMode,
		})
	}

	slog.Info("webui: appearance updated", "username", fullAccount.Username, "theme", theme, "appearance", appearance)

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/settings")
		w.WriteHeader(http.StatusOK)
		return
	}

	http.Redirect(w, r, "/settings", http.StatusFound)
}

// SettingsCustomCSSUpdate saves the account's own stylesheet, from either the
// textarea or an uploaded .css file. Unlike the theme it is not mirrored into a
// cookie: it is far too big for one, so a signed-out visitor sees the instance
// default.
func (h *Handler) SettingsCustomCSSUpdate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, content.MaxCustomCSSBytes+(1<<20))
	if err := r.ParseMultipartForm(content.MaxCustomCSSBytes); err != nil {
		h.redirectCustomCSSError(w, r, "too_large")
		return
	}

	css := r.FormValue("custom_css")
	// An upload beats the textarea: someone who picked a file meant the file,
	// and the textarea still holds whatever was already saved.
	if file, _, err := r.FormFile("custom_css_file"); err == nil {
		defer func() { _ = file.Close() }()
		uploaded, err := io.ReadAll(io.LimitReader(file, content.MaxCustomCSSBytes+1))
		if err != nil {
			h.redirectCustomCSSError(w, r, "read")
			return
		}
		css = string(uploaded)
	}
	if r.FormValue("remove_custom_css") == "1" {
		css = ""
	}

	css, err := content.ValidateCustomCSS(css)
	if err != nil {
		switch {
		case errors.Is(err, content.ErrCustomCSSTooLarge):
			h.redirectCustomCSSError(w, r, "too_large")
		case errors.Is(err, content.ErrCustomCSSEncoding):
			h.redirectCustomCSSError(w, r, "encoding")
		default:
			h.redirectCustomCSSError(w, r, "markup")
		}
		return
	}

	fullAccount, err := h.store.GetAccount(r.Context(), account.ID)
	if err != nil {
		slog.Error("webui: get account for custom css update", "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	fullAccount.CustomCSS = css

	if err := h.store.UpdateAccount(r.Context(), fullAccount); err != nil {
		slog.Error("webui: update custom css", "error", err)
		http.Error(w, "Error saving settings", http.StatusInternalServerError)
		return
	}

	slog.Info("webui: custom css updated", "username", fullAccount.Username, "bytes", len(css))

	h.redirectToSettings(w, r, "/settings#custom-css")
}

// redirectCustomCSSError sends the user back to the custom CSS section with a
// code the settings page turns into a translated message.
func (h *Handler) redirectCustomCSSError(w http.ResponseWriter, r *http.Request, code string) {
	h.redirectToSettings(w, r, "/settings?custom_css_error="+code+"#custom-css")
}

// redirectToSettings returns to the settings page, using htmx's own redirect
// header when htmx made the request so it swaps instead of following the 302.
func (h *Handler) redirectToSettings(w http.ResponseWriter, r *http.Request, target string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// SettingsPostLanguageUpdate saves the user's new-post composer defaults.
func (h *Handler) SettingsPostLanguageUpdate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	postLanguage := r.FormValue("post_language")
	if !isValidContentLanguage(postLanguage) {
		http.Error(w, "Unsupported language", http.StatusBadRequest)
		return
	}
	postVisibility := models.Visibility(r.FormValue("post_visibility"))
	switch postVisibility {
	case models.VisibilityPublic, models.VisibilityUnlisted, models.VisibilityPrivate,
		models.VisibilityDirect, models.VisibilityLocal, models.VisibilityLocalUnlisted:
	default:
		http.Error(w, "Unsupported audience", http.StatusBadRequest)
		return
	}

	fullAccount, err := h.store.GetAccount(r.Context(), account.ID)
	if err != nil {
		slog.Error("webui: get account for post language update", "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	fullAccount.DefaultPostLanguage = postLanguage
	fullAccount.DefaultPostMarkdown = r.FormValue("post_markdown") == "on"
	fullAccount.DefaultPostVisibility = postVisibility
	fullAccount.HideTimelineComposer = r.FormValue("show_timeline_composer") != "on"

	if err := h.store.UpdateAccount(r.Context(), fullAccount); err != nil {
		slog.Error("webui: update post language", "error", err)
		http.Error(w, "Error saving settings", http.StatusInternalServerError)
		return
	}

	slog.Info("webui: posting defaults updated", "username", fullAccount.Username)

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/settings")
		w.WriteHeader(http.StatusOK)
		return
	}

	http.Redirect(w, r, "/settings", http.StatusFound)
}

func (h *Handler) SettingsWellnessUpdate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}
	fullAccount, err := h.store.GetAccount(r.Context(), account.ID)
	if err != nil {
		slog.Error("webui: get account for wellness settings", "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	fullAccount.HideFavouriteCounts = r.FormValue("hide_favourite_counts") == "on"
	fullAccount.HideBoostCounts = r.FormValue("hide_boost_counts") == "on"
	fullAccount.HideFollowerCounts = r.FormValue("hide_follower_counts") == "on"
	if err := h.store.UpdateAccount(r.Context(), fullAccount); err != nil {
		slog.Error("webui: update wellness settings", "error", err)
		http.Error(w, "Error saving settings", http.StatusInternalServerError)
		return
	}
	h.app.InvalidateFilterPages(account.ID)
	h.redirectToSettings(w, r, "/settings#wellness")
}

// SettingsTimelineUpdate saves the user's timeline content preferences.
func (h *Handler) SettingsTimelineUpdate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	fullAccount, err := h.store.GetAccount(r.Context(), account.ID)
	if err != nil {
		slog.Error("webui: get account for timeline settings", "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	fullAccount.HideReplies = r.FormValue("show_replies") != "on"
	fullAccount.HideBoosts = r.FormValue("show_boosts") != "on"
	fullAccount.DataSaver = r.FormValue("data_saver") == "on"
	fullAccount.HidePostsWithoutAlt = r.FormValue("hide_posts_without_alt") == "on"
	if err := h.store.UpdateAccount(r.Context(), fullAccount); err != nil {
		slog.Error("webui: update timeline settings", "error", err)
		http.Error(w, "Error saving settings", http.StatusInternalServerError)
		return
	}

	slog.Info("webui: timeline settings updated", "username", fullAccount.Username,
		"hide_replies", fullAccount.HideReplies, "hide_boosts", fullAccount.HideBoosts,
		"data_saver", fullAccount.DataSaver, "hide_posts_without_alt", fullAccount.HidePostsWithoutAlt)
	h.app.InvalidateFilterPages(account.ID)

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/settings#timeline")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/settings#timeline", http.StatusFound)
}

// SettingsBlogUpdate saves the account's blog reply and permalink preferences.
//
// Changing the template never touches slugs already assigned: a permalink is
// frozen the first time a post is rendered, so only future posts move.
func (h *Handler) SettingsBlogUpdate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}
	if !h.blogAllowed(account) {
		http.Error(w, "Blog settings are unavailable", http.StatusForbidden)
		return
	}

	format := strings.TrimSpace(r.FormValue("blog_slug_format"))
	if h.cfg.Blog.AllowUserSlugFormat && format != "" {
		if err := blogslug.Validate(format); err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
	}

	fullAccount, err := h.store.GetAccount(r.Context(), account.ID)
	if err != nil {
		slog.Error("webui: get account for blog settings", "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	repliesChanged := fullAccount.BlogRepliesEnabled != (r.FormValue("blog_replies_enabled") == "on")
	fullAccount.BlogRepliesEnabled = r.FormValue("blog_replies_enabled") == "on"
	if h.cfg.Blog.DirectoryEnabled {
		fullAccount.Discovery = r.FormValue("blog_directory_listed") == "on"
	}
	if h.cfg.Blog.AllowUserSlugFormat {
		fullAccount.BlogSlugFormat = format
	}
	if err := h.store.UpdateAccount(r.Context(), fullAccount); err != nil {
		slog.Error("webui: update blog settings", "error", err)
		http.Error(w, "Error saving settings", http.StatusInternalServerError)
		return
	}
	if repliesChanged {
		h.enqueueBlogSettingsRebuild(r.Context(), fullAccount.ID)
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/settings#blog")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/settings#blog", http.StatusFound)
}

// SettingsBlogAppearanceUpdate saves the theme and the author stylesheet for
// the generated static blog, from either the textarea or an uploaded .css
// file. Both are baked into the site's style.css at build time, so a change to
// either one queues a full rebuild - and only a change does, because a rebuild
// is the most expensive thing this settings page can start.
func (h *Handler) SettingsBlogAppearanceUpdate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if !h.blogAllowed(account) {
		http.Error(w, "Blog settings are unavailable", http.StatusForbidden)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, content.MaxCustomCSSBytes+(1<<20))
	if err := r.ParseMultipartForm(content.MaxCustomCSSBytes); err != nil {
		h.redirectBlogCSSError(w, r, "too_large")
		return
	}

	theme := r.FormValue("blog_theme")
	if !blogtheme.IsSupported(theme) {
		h.redirectBlogCSSError(w, r, "theme")
		return
	}

	css := r.FormValue("blog_custom_css")
	// An upload beats the textarea: someone who picked a file meant the file,
	// and the textarea still holds whatever was already saved.
	if file, _, err := r.FormFile("blog_custom_css_file"); err == nil {
		defer func() { _ = file.Close() }()
		uploaded, err := io.ReadAll(io.LimitReader(file, content.MaxCustomCSSBytes+1))
		if err != nil {
			h.redirectBlogCSSError(w, r, "read")
			return
		}
		css = string(uploaded)
	}
	if r.FormValue("remove_blog_custom_css") == "1" {
		css = ""
	}

	css, err := content.ValidateCustomCSS(css)
	if err != nil {
		switch {
		case errors.Is(err, content.ErrCustomCSSTooLarge):
			h.redirectBlogCSSError(w, r, "too_large")
		case errors.Is(err, content.ErrCustomCSSEncoding):
			h.redirectBlogCSSError(w, r, "encoding")
		default:
			h.redirectBlogCSSError(w, r, "markup")
		}
		return
	}

	fullAccount, err := h.store.GetAccount(r.Context(), account.ID)
	if err != nil {
		slog.Error("webui: get account for blog appearance", "error", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	changed := fullAccount.BlogTheme != theme || fullAccount.BlogCustomCSS != css
	fullAccount.BlogTheme = theme
	fullAccount.BlogCustomCSS = css
	if err := h.store.UpdateAccount(r.Context(), fullAccount); err != nil {
		slog.Error("webui: update blog appearance", "error", err)
		http.Error(w, "Error saving settings", http.StatusInternalServerError)
		return
	}
	if changed {
		h.enqueueBlogSettingsRebuild(r.Context(), fullAccount.ID)
	}

	slog.Info("webui: blog appearance updated", "username", fullAccount.Username, "theme", theme, "bytes", len(css), "rebuild", changed)

	h.redirectToSettings(w, r, "/settings#blog-appearance")
}

// SettingsBlogRebuild regenerates the whole static site on demand. Publishing
// already queues rebuilds by itself; this is the button for when the files on
// disk and the posts in the database have drifted apart - a restored backup, a
// half-finished build, an upgrade that changed the templates.
func (h *Handler) SettingsBlogRebuild(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if !h.blogAllowed(account) {
		http.Error(w, "Blog settings are unavailable", http.StatusForbidden)
		return
	}
	h.enqueueBlogSettingsRebuild(r.Context(), account.ID)
	slog.Info("webui: blog rebuild requested", "username", account.Username)
	h.redirectToSettings(w, r, "/settings?blog_rebuild=queued#blog-appearance")
}

// redirectBlogCSSError sends the user back to the blog appearance section with
// a code the settings page turns into a translated message.
func (h *Handler) redirectBlogCSSError(w http.ResponseWriter, r *http.Request, code string) {
	h.redirectToSettings(w, r, "/settings?blog_css_error="+code+"#blog-appearance")
}

// SettingsNotificationsUpdate saves notification grouping and visibility preferences.
func (h *Handler) SettingsNotificationsUpdate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	fullAccount, err := h.store.GetAccount(r.Context(), account.ID)
	if err != nil {
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	fullAccount.GroupNotifications = r.FormValue("group_notifications") == "on"
	if !h.cfg.Instance.BlockSensitiveRelationshipNotifications {
		fullAccount.NotifySensitiveRelationships = r.FormValue("notify_sensitive_relationships") == "on"
	}
	excluded := make([]string, 0)
	for _, option := range notificationPreferenceOptions {
		if r.FormValue("show_"+option.Name) != "on" {
			excluded = append(excluded, option.Types...)
		}
	}
	fullAccount.NotificationExcludedTypes = strings.Join(excluded, ",")
	if err := h.store.UpdateAccount(r.Context(), fullAccount); err != nil {
		slog.Error("webui: update notification settings", "error", err)
		http.Error(w, "Error saving settings", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/settings#notifications")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/settings#notifications", http.StatusFound)
}

// SettingsEmailNotificationsUpdate handles updates to email notification preferences.
func (h *Handler) SettingsEmailNotificationsUpdate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	fullAccount, err := h.store.GetAccount(r.Context(), account.ID)
	if err != nil {
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	fullAccount.EmailNotifyMention = r.FormValue("email_notify_mention") == "on"
	fullAccount.EmailNotifyFollow = r.FormValue("email_notify_follow") == "on"
	fullAccount.EmailNotifyReblog = r.FormValue("email_notify_reblog") == "on"
	fullAccount.EmailNotifyFavourite = r.FormValue("email_notify_favourite") == "on"
	fullAccount.EmailNotifyPoll = r.FormValue("email_notify_poll") == "on"
	fullAccount.EmailNotifyFollowRequest = r.FormValue("email_notify_follow_request") == "on"
	fullAccount.EmailNotifyQuote = r.FormValue("email_notify_quote") == "on"
	fullAccount.EmailNotifyDigest = r.FormValue("email_notify_digest") == "on"
	fullAccount.EmailNotifyReport = r.FormValue("email_notify_report") == "on"
	fullAccount.EmailNotifyApproval = r.FormValue("email_notify_approval") == "on"
	fullAccount.EmailNotifyNewAccount = r.FormValue("email_notify_new_account") == "on"
	fullAccount.EmailNotifyInviteSignup = r.FormValue("email_notify_invite_signup") == "on"
	fullAccount.EmailNotifyBackupSuccess = r.FormValue("email_notify_backup_success") == "on"
	fullAccount.EmailNotifyBackupFailure = r.FormValue("email_notify_backup_failure") == "on"
	fullAccount.EmailNotifyInstance = r.FormValue("email_notify_instance") == "on"
	fullAccount.EmailNotifyAccountActivity = r.FormValue("email_notify_account_activity") == "on"
	fullAccount.EmailNotifyAccountDeparture = r.FormValue("email_notify_account_departure") == "on"
	// Same as the push form: the toggle only exists for blog-capable accounts.
	if h.blogAllowed(fullAccount) {
		fullAccount.EmailNotifyBlogRebuild = r.FormValue("email_notify_blog_rebuild") == "on"
	}

	if err := h.store.UpdateAccount(r.Context(), fullAccount); err != nil {
		slog.Error("webui: update email notification settings", "error", err)
		http.Error(w, "Error saving settings", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/settings#email")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/settings#email", http.StatusFound)
}

// SettingsEmailUpdate handles setting or changing the account email address.
func (h *Handler) SettingsEmailUpdate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	if email == "" {
		http.Error(w, "Email is required", http.StatusBadRequest)
		return
	}
	if err := accounts.ValidateEmail(email); err != nil {
		http.Error(w, "Invalid email address", http.StatusBadRequest)
		return
	}
	if email == account.Email {
		http.Redirect(w, r, "/settings#email", http.StatusFound)
		return
	}

	// Check email is not already in use
	existing, err := h.store.GetAccountByEmail(r.Context(), email)
	if err == nil && existing != nil && existing.ID != account.ID {
		http.Error(w, "Email address is already in use by another account", http.StatusBadRequest)
		return
	}

	fullAccount, err := h.store.GetAccount(r.Context(), account.ID)
	if err != nil {
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	if !h.allowLogin(r, fullAccount, r.FormValue("current_password")) {
		http.Error(w, "Invalid password", http.StatusForbidden)
		return
	}

	fullAccount.Email = email
	fullAccount.EmailVerified = false
	fullAccount.EmailVerifiedAt = nil

	if err := h.store.UpdateAccount(r.Context(), fullAccount); err != nil {
		slog.Error("webui: update email", "error", err)
		http.Error(w, "Error saving email", http.StatusInternalServerError)
		return
	}
	if h.mailer != nil {
		if err := mail.EnqueueEmailVerification(r.Context(), h.store, h.domain, fullAccount); err != nil {
			slog.Error("webui: enqueue changed-email verification", "error", err)
		}
	}

	slog.Info("webui: email updated", "username", fullAccount.Username, "email", email)

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/settings#email")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/settings#email", http.StatusFound)
}

// Web Push (PWA) Settings

// pushSubscribeRequest is the JSON body sent by the browser after
// PushManager.subscribe() succeeds.
type pushSubscribeRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256DH string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// SettingsPushSubscribe creates or updates this web session's subscription.
func (h *Handler) SettingsPushSubscribe(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	sessionID := getSessionID(r.Context())
	if account == nil || sessionID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if h.vapidPublicKey == "" {
		http.Error(w, "Push notifications are not configured on this server", http.StatusNotFound)
		return
	}

	var req pushSubscribeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Endpoint == "" || req.Keys.P256DH == "" || req.Keys.Auth == "" {
		http.Error(w, "endpoint, keys.p256dh, and keys.auth are required", http.StatusUnprocessableEntity)
		return
	}
	endpoint, err := pushpkg.NormalizeEndpoint(req.Endpoint)
	if err != nil {
		http.Error(w, "Invalid push subscription endpoint", http.StatusUnprocessableEntity)
		return
	}
	p256dh, auth, err := pushpkg.NormalizeClientKeys(req.Keys.P256DH, req.Keys.Auth)
	if err != nil {
		http.Error(w, "Invalid push subscription keys", http.StatusUnprocessableEntity)
		return
	}

	existing, _ := h.store.GetPushSubscriptionBySessionID(r.Context(), sessionID)
	owner, _ := h.store.GetPushSubscriptionByEndpoint(r.Context(), endpoint)
	if owner != nil && (existing == nil || owner.ID != existing.ID) {
		// A browser subscription is origin-wide. A new login may legitimately
		// present the same endpoint and keys; transfer that web slot, but never
		// take an OAuth app's endpoint or accept mismatched key material.
		if owner.OAuthTokenID != nil || owner.P256DH != p256dh || owner.Auth != auth {
			http.Error(w, "Push endpoint is already registered", http.StatusConflict)
			return
		}
		if err := h.store.DeletePushSubscription(r.Context(), owner.ID); err != nil {
			http.Error(w, "Error transferring subscription", http.StatusInternalServerError)
			return
		}
	}
	if existing != nil {
		existing.Endpoint = endpoint
		existing.P256DH = p256dh
		existing.Auth = auth
		if err := h.store.UpdatePushSubscription(r.Context(), existing); err != nil {
			slog.Error("webui: update push subscription", "error", err)
			http.Error(w, "Error saving subscription", http.StatusInternalServerError)
			return
		}
		writeJSONOK(w)
		return
	}

	sub := &models.PushSubscription{
		AccountID: account.ID,
		SessionID: &sessionID,
		Endpoint:  endpoint,
		P256DH:    p256dh,
		Auth:      auth,
		Label:     friendlyUserAgent(r.UserAgent()),
		Policy:    "all",
		// Match the Mastodon-app defaults (internal/api/push.go): mention/poll/
		// status/quote alerts on, the rest opt-in via /settings/push/preferences.
		NotifyMention:     true,
		NotifyPoll:        true,
		NotifyStatus:      true,
		NotifyQuote:       true,
		NotifyBlogRebuild: true,
	}
	if err := h.store.CreatePushSubscription(r.Context(), sub); err != nil {
		slog.Error("webui: create push subscription", "error", err)
		http.Error(w, "Error saving subscription", http.StatusInternalServerError)
		return
	}
	writeJSONOK(w)
}

// SettingsPushUnsubscribe deletes the web UI's own push subscription slot.
func (h *Handler) SettingsPushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	sessionID := getSessionID(r.Context())
	if account == nil || sessionID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	sub, err := h.store.GetPushSubscriptionBySessionID(r.Context(), sessionID)
	if err != nil {
		writeJSONOK(w)
		return
	}
	if err := h.store.DeletePushSubscription(r.Context(), sub.ID); err != nil {
		slog.Error("webui: delete push subscription", "error", err)
		http.Error(w, "Error removing subscription", http.StatusInternalServerError)
		return
	}
	writeJSONOK(w)
}

// SettingsPushPreferencesUpdate updates the alert-type preferences on the
// web UI's push subscription. A real HTML form (works without JS) - only
// subscribing/unsubscribing itself needs the Push API.
func (h *Handler) SettingsPushPreferencesUpdate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	sessionID := getSessionID(r.Context())
	if account == nil || sessionID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	sub, err := h.store.GetPushSubscriptionBySessionID(r.Context(), sessionID)
	if err != nil {
		http.Error(w, "No push subscription", http.StatusNotFound)
		return
	}

	sub.NotifyMention = r.FormValue("push_notify_mention") == "on"
	sub.NotifyFollow = r.FormValue("push_notify_follow") == "on"
	sub.NotifyReblog = r.FormValue("push_notify_reblog") == "on"
	sub.NotifyFavourite = r.FormValue("push_notify_favourite") == "on"
	sub.NotifyPoll = r.FormValue("push_notify_poll") == "on"
	sub.NotifyFollowRequest = r.FormValue("push_notify_follow_request") == "on"
	sub.NotifyStatus = r.FormValue("push_notify_status") == "on"
	sub.NotifyQuote = r.FormValue("push_notify_quote") == "on"
	// The blog toggle is only rendered for accounts allowed to publish one;
	// otherwise leave the stored value alone instead of reading it as "off".
	if h.blogAllowed(account) {
		sub.NotifyBlogRebuild = r.FormValue("push_notify_blog_rebuild") == "on"
	}
	policy := r.FormValue("push_policy")
	if !pushpkg.ValidPolicy(policy) {
		http.Error(w, "Invalid push policy", http.StatusBadRequest)
		return
	}
	sub.Policy = policy

	if err := h.store.UpdatePushSubscription(r.Context(), sub); err != nil {
		slog.Error("webui: update push preferences", "error", err)
		http.Error(w, "Error saving preferences", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/settings#push")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/settings#push", http.StatusFound)
}

// SettingsPushRevoke deletes any one of the account's push subscriptions by
// ID (a Mastodon app's slot or the web UI's own) - a real form action so it
// works without JS, letting a user clean up a stale/lost device's push
// registration from Settings.
func (h *Handler) SettingsPushRevoke(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	id := chi.URLParam(r, "id")
	sub, err := h.store.GetPushSubscription(r.Context(), id)
	if err != nil || sub.AccountID != account.ID {
		http.NotFound(w, r)
		return
	}
	if err := h.store.DeletePushSubscription(r.Context(), id); err != nil {
		slog.Error("webui: revoke push subscription", "error", err)
		http.Error(w, "Error revoking subscription", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/settings#push")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/settings#push", http.StatusFound)
}

func writeJSONOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// friendlyUserAgent turns a raw User-Agent string into a short label for
// Settings' subscription list. Best-effort pattern match, not a real parser.
func friendlyUserAgent(ua string) string {
	switch {
	case strings.Contains(ua, "Firefox/"):
		return "Web (Firefox)"
	case strings.Contains(ua, "Edg/"):
		return "Web (Edge)"
	case strings.Contains(ua, "OPR/"):
		return "Web (Opera)"
	case strings.Contains(ua, "Chrome/"):
		return "Web (Chrome)"
	case strings.Contains(ua, "Safari/"):
		return "Web (Safari)"
	default:
		return "Web browser"
	}
}

// Account Move Settings

// SettingsMovePage shows the account move settings page.
func (h *Handler) SettingsMovePage(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	ctx := r.Context()
	data := map[string]interface{}{
		"Account":      account,
		"AlreadyMoved": account.MovedToURI != "",
		"MovedToURI":   account.MovedToURI,
		"TargetAcct":   "",
		"CSRFToken":    getCSRFToken(ctx),
		"Domain":       h.localDomain,
	}
	if account.MovedAt != nil {
		data["MovedAt"] = account.MovedAt.Format("2006-01-02 15:04:05")
	}

	h.renderPage(w, r, h.pageTitle(r, "move.heading"), data, "settings_move.html")
}

// SettingsMoveSubmit processes the account move request.
func (h *Handler) SettingsMoveSubmit(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	ctx := r.Context()
	if h.cfg != nil && !h.cfg.Migration.Enabled {
		h.renderPage(w, r, h.pageTitle(r, "move.heading"), map[string]interface{}{
			"Account":   account,
			"CSRFToken": getCSRFToken(ctx),
			"Domain":    h.localDomain,
			"Error":     h.pageTitle(r, "move.error.disabled"),
		}, "settings_move.html")
		return
	}
	if h.federator == nil {
		h.renderPage(w, r, h.pageTitle(r, "move.heading"), map[string]interface{}{
			"Account":   account,
			"CSRFToken": getCSRFToken(ctx),
			"Domain":    h.localDomain,
			"Error":     h.pageTitle(r, "move.error.no_federation"),
		}, "settings_move.html")
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	targetAcct := strings.TrimSpace(r.FormValue("target_account"))
	password := r.FormValue("password")

	if targetAcct == "" || password == "" {
		h.renderPage(w, r, h.pageTitle(r, "move.heading"), map[string]interface{}{
			"Account":    account,
			"CSRFToken":  getCSRFToken(ctx),
			"Domain":     h.localDomain,
			"TargetAcct": targetAcct,
			"Error":      h.pageTitle(r, "move.error.required"),
		}, "settings_move.html")
		return
	}

	// Verify password
	if !auth.CheckPassword(password, account.PasswordHash) {
		slog.Warn("webui: move password verification failed", "username", account.Username)
		h.renderPage(w, r, h.pageTitle(r, "move.heading"), map[string]interface{}{
			"Account":    account,
			"CSRFToken":  getCSRFToken(ctx),
			"Domain":     h.localDomain,
			"TargetAcct": targetAcct,
			"Error":      h.pageTitle(r, "move.error.invalid_password"),
		}, "settings_move.html")
		return
	}

	// Parse target: username@domain
	parts := strings.SplitN(targetAcct, "@", 2)
	if len(parts) != 2 {
		h.renderPage(w, r, h.pageTitle(r, "move.heading"), map[string]interface{}{
			"Account":   account,
			"CSRFToken": getCSRFToken(ctx),
			"Domain":    h.localDomain,
			"Error":     h.pageTitle(r, "move.error.bad_format"),
		}, "settings_move.html")
		return
	}
	targetDomain := parts[1]

	if strings.EqualFold(targetDomain, h.localDomain) {
		h.renderPage(w, r, h.pageTitle(r, "move.heading"), map[string]interface{}{
			"Account":   account,
			"CSRFToken": getCSRFToken(ctx),
			"Domain":    h.localDomain,
			"Error":     h.pageTitle(r, "move.error.same_instance"),
		}, "settings_move.html")
		return
	}

	// Check cooldown
	if account.MovedAt != nil {
		cooldown := 30 * 24 * time.Hour
		if h.cfg != nil && h.cfg.Migration.CooldownAfterMove > 0 {
			cooldown = h.cfg.Migration.CooldownAfterMove
		}
		if time.Since(*account.MovedAt) < cooldown {
			h.renderPage(w, r, h.pageTitle(r, "move.heading"), map[string]interface{}{
				"Account":   account,
				"CSRFToken": getCSRFToken(ctx),
				"Domain":    h.localDomain,
				"Error":     h.pageTitle(r, "move.error.cooldown"),
			}, "settings_move.html")
			return
		}
	}

	// Resolve the target account
	targetAccount, err := h.federator.ResolveAccount(ctx, targetAcct)
	if err != nil {
		slog.Warn("webui: move target resolution failed", "target", targetAcct, "error", err)
		h.renderPage(w, r, h.pageTitle(r, "move.heading"), map[string]interface{}{
			"Account":   account,
			"CSRFToken": getCSRFToken(ctx),
			"Domain":    h.localDomain,
			"Error":     h.pageTitle(r, "move.error.not_found_prefix") + err.Error(),
		}, "settings_move.html")
		return
	}

	newActorURI := targetAccount.ActorURI
	if h.cfg != nil && h.cfg.Migration.RequireTargetAlias {
		refreshedTarget, err := h.federator.RefreshActor(ctx, targetAccount.ActorURI, federation.ActorRefreshOptions{Force: true})
		if err != nil {
			h.renderPage(w, r, h.pageTitle(r, "move.heading"), map[string]interface{}{
				"Account":    account,
				"CSRFToken":  getCSRFToken(ctx),
				"Domain":     h.localDomain,
				"TargetAcct": targetAcct,
				"Error":      h.pageTitle(r, "move.error.refresh_failed_prefix") + err.Error(),
			}, "settings_move.html")
			return
		}
		targetAccount = refreshedTarget
		if err := accountmigration.VerifyMovePair(account, targetAccount, false); err != nil {
			h.renderPage(w, r, h.pageTitle(r, "move.heading"), map[string]interface{}{
				"Account":    account,
				"CSRFToken":  getCSRFToken(ctx),
				"Domain":     h.localDomain,
				"TargetAcct": targetAcct,
				"Error":      h.pageTitle(r, "move.error.alias_required"),
			}, "settings_move.html")
			return
		}
	}

	slog.Info("webui: initiating account move",
		"username", account.Username,
		"from", fmt.Sprintf("https://%s/users/%s", h.domain, account.Username),
		"to", newActorURI,
	)

	// Mark as moved
	if err := h.store.SetAccountMoved(ctx, account.ID, newActorURI); err != nil {
		slog.Error("webui: failed to mark account as moved", "error", err)
		h.renderPage(w, r, h.pageTitle(r, "move.heading"), map[string]interface{}{
			"Account":   account,
			"CSRFToken": getCSRFToken(ctx),
			"Domain":    h.localDomain,
			"Error":     h.pageTitle(r, "move.error.failed"),
		}, "settings_move.html")
		return
	}
	h.app.NotifyAdminsAccountMoved(ctx, account)

	// Build Move activity
	moveID := account.ActorURI + "/move/" + uuid.New().String()
	moveActivity := map[string]interface{}{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       moveID,
		"type":     "Move",
		"actor":    account.ActorURI,
		"object":   account.ActorURI,
		"target":   newActorURI,
	}

	// Deliver to followers asynchronously
	go func() {
		ctx := context.Background()
		if _, err := app.MigrateLocalFollowers(ctx, h.store, h.federator, account, targetAccount); err != nil {
			slog.Warn("webui move: failed to migrate local followers", "error", err)
		}
		inboxes, err := h.store.GetFollowerDestinations(ctx, account.ID)
		if err != nil {
			slog.Error("webui move: failed to get follower destinations", "error", err)
			return
		}
		for _, inbox := range inboxes {
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
				slog.Warn("webui move: enqueue delivery failed", "inbox", inbox, "error", err)
			}
		}

		// Also deliver to the target server
		if targetAccount.InboxURI != "" {
			targetInbox := targetAccount.InboxURI
			if targetAccount.SharedInboxURI != nil && *targetAccount.SharedInboxURI != "" {
				targetInbox = *targetAccount.SharedInboxURI
			}
			if err := h.federator.Deliver(ctx, targetInbox, moveActivity); err != nil {
				slog.Warn("webui move: failed to deliver to target", "target", targetInbox, "error", err)
			}
		}

		slog.Info("webui move: delivered to followers", "username", account.Username, "count", len(inboxes))
	}()

	// Redirect to settings with success message
	http.Redirect(w, r, "/settings?moved=1", http.StatusFound)
}

// SettingsImportFollows triggers follow import from an old account.
// Supports both:
//   - Import from alsoKnownAs (if set and no import_alias provided)
//   - Manual import from an old account handle/URI (from import_alias form field)
func (h *Handler) SettingsImportFollows(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	if account.Domain != nil {
		http.Error(w, "Only local accounts can import follows", http.StatusBadRequest)
		return
	}
	if h.cfg != nil && !h.cfg.Migration.Enabled {
		http.Error(w, "Account migration is disabled on this instance", http.StatusForbidden)
		return
	}

	if h.federator == nil {
		http.Error(w, "Federation not configured", http.StatusInternalServerError)
		return
	}

	// Determine the old account URI
	oldActorURI := strings.TrimSpace(r.FormValue("import_alias"))
	if oldActorURI == "" {
		// Fall back to alsoKnownAs if no manual entry
		oldActorURI = account.AlsoKnownAs
	}
	if oldActorURI == "" {
		// Re-render settings with error
		unreadCount, _ := h.store.CountUnreadNotificationsFiltered(ctx, account.ID, notificationExcludedTypes(account))
		h.renderPage(w, r, h.pageTitle(r, "nav.settings"), &SettingsData{
			Title:       "Settings",
			Account:     account,
			UnreadCount: unreadCount,
			CSRFToken:   getCSRFToken(ctx),
			Domain:      h.localDomain,
			ImportError: "Please provide an old account handle (user@domain) or actor URI.",
		}, "settings.html")
		return
	}

	// If oldActorURI does not look like a full URI, resolve it via WebFinger
	if !strings.HasPrefix(oldActorURI, "http://") && !strings.HasPrefix(oldActorURI, "https://") {
		// Treat as user@domain handle
		resolved, err := h.federator.ResolveAccount(ctx, oldActorURI)
		if err != nil {
			slog.Warn("webui: import_follows resolution failed", "input", oldActorURI, "error", err)
			unreadCount, _ := h.store.CountUnreadNotificationsFiltered(ctx, account.ID, notificationExcludedTypes(account))
			h.renderPage(w, r, h.pageTitle(r, "nav.settings"), &SettingsData{
				Title:       "Settings",
				Account:     account,
				UnreadCount: unreadCount,
				CSRFToken:   getCSRFToken(ctx),
				Domain:      h.localDomain,
				ImportAlias: oldActorURI,
				ImportError: "Could not find that account: " + err.Error(),
			}, "settings.html")
			return
		}
		oldActorURI = resolved.ActorURI
	}

	// Store the alias on the account for future reference
	if account.AlsoKnownAs == "" {
		if err := h.store.SetAccountAlias(ctx, account.ID, oldActorURI); err != nil {
			slog.Warn("webui: failed to set alias", "error", err)
			// Continue anyway - import still works
		}
	}

	slog.Info("webui: starting follow import",
		"username", account.Username,
		"old_actor", oldActorURI,
	)

	// Run import in the background
	options := app.MigrationOptions{}
	if h.cfg != nil {
		options.MaxFollows = h.cfg.Migration.MaxFollowsImport
		options.ImportRate = h.cfg.Migration.ImportRate
	}
	importer := app.NewFollowImporterWithOptions(h.store, h.federator, h.domain, options)
	go func() {
		bgCtx := context.Background()
		imported, err := importer.ImportFollows(bgCtx, account, oldActorURI)
		if err != nil {
			slog.Error("webui: follow import failed",
				"username", account.Username,
				"error", err,
			)
			return
		}
		slog.Info("webui: follow import completed",
			"username", account.Username,
			"imported", imported,
		)
	}()

	// Redirect to settings with success message
	http.Redirect(w, r, "/settings?imported=1", http.StatusFound)
}

// SettingsSetMigrationAlias stores an old account actor as this account's
// alsoKnownAs value. Users moving into littleFedi should do this before
// initiating the Move on the old server.
func (h *Handler) SettingsSetMigrationAlias(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if account.Domain != nil {
		http.Error(w, "Only local accounts can set migration aliases", http.StatusBadRequest)
		return
	}
	if h.cfg != nil && !h.cfg.Migration.Enabled {
		http.Error(w, "Account migration is disabled on this instance", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}
	alias := strings.TrimSpace(r.FormValue("migration_alias"))
	if alias == "" {
		http.Redirect(w, r, "/settings?alias_error=missing", http.StatusFound)
		return
	}
	if !strings.HasPrefix(alias, "http://") && !strings.HasPrefix(alias, "https://") {
		if h.federator == nil {
			http.Error(w, "Federation not configured", http.StatusInternalServerError)
			return
		}
		resolved, err := h.federator.ResolveAccount(r.Context(), alias)
		if err != nil {
			slog.Warn("webui: migration alias resolution failed", "input", alias, "error", err)
			http.Redirect(w, r, "/settings?alias_error=resolve", http.StatusFound)
			return
		}
		alias = resolved.ActorURI
	}
	if err := h.store.SetAccountAlias(r.Context(), account.ID, alias); err != nil {
		slog.Warn("webui: failed to set migration alias", "account", account.ID, "alias", alias, "error", err)
		http.Redirect(w, r, "/settings?alias_error=save", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/settings?alias_set=1", http.StatusFound)
}

// SettingsExportFollows exports the user's following list as a Mastodon-compatible CSV.
// GET /settings/export_follows
func (h *Handler) SettingsExportFollows(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	ctx := r.Context()

	// Fetch all accounts this user follows
	following, err := h.store.GetFollowingToMigrate(ctx, account.ID, 0) // 0 = no limit
	if err != nil {
		slog.Error("webui: export follows failed", "error", err)
		http.Error(w, "Error exporting follows", http.StatusInternalServerError)
		return
	}

	// Build Mastodon-compatible CSV
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

// SettingsImportFollowsCSV imports follows from a Mastodon-compatible CSV file.
// POST /settings/import_follows_csv (multipart form with "csv_file" field)
func (h *Handler) SettingsImportFollowsCSV(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	ctx := r.Context()
	if h.cfg != nil && !h.cfg.Migration.Enabled {
		http.Error(w, "Account migration is disabled on this instance", http.StatusForbidden)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 11<<20)
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		slog.Warn("webui: import CSV parse form failed", "error", err)
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("csv_file")
	if err != nil {
		slog.Warn("webui: import CSV missing file", "error", err)
		unreadCount, _ := h.store.CountUnreadNotificationsFiltered(ctx, account.ID, notificationExcludedTypes(account))
		h.renderPage(w, r, h.pageTitle(r, "nav.settings"), &SettingsData{
			Title:       "Settings",
			Account:     account,
			UnreadCount: unreadCount,
			CSRFToken:   getCSRFToken(ctx),
			Domain:      h.localDomain,
			ImportError: "Please select a CSV file to upload.",
		}, "settings.html")
		return
	}
	defer file.Close()
	defer r.MultipartForm.RemoveAll()

	slog.Info("webui: processing CSV import",
		"username", account.Username,
		"filename", header.Filename,
		"size", header.Size,
	)

	// Parse the CSV
	limit := 0
	if h.cfg != nil {
		limit = h.cfg.Migration.MaxFollowsImport
	}
	handles, err := app.ParseFollowCSV(file, limit)
	if err != nil {
		code := "parse"
		if errors.Is(err, app.ErrFollowImportLimit) {
			code = "limit"
		}
		http.Redirect(w, r, "/settings?csv_error="+code, http.StatusFound)
		return
	}

	if len(handles) == 0 {
		unreadCount, _ := h.store.CountUnreadNotificationsFiltered(ctx, account.ID, notificationExcludedTypes(account))
		h.renderPage(w, r, h.pageTitle(r, "nav.settings"), &SettingsData{
			Title:       "Settings",
			Account:     account,
			UnreadCount: unreadCount,
			CSRFToken:   getCSRFToken(ctx),
			Domain:      h.localDomain,
			ImportError: "No valid account handles found in CSV file.",
		}, "settings.html")
		return
	}

	_, err = app.RequestFollowImport(ctx, h.store, account, handles)
	if err != nil {
		code := "save"
		if errors.Is(err, storage.ErrAccountImportActive) {
			code = "active"
		}
		slog.Warn("webui: request follows import", "account_id", account.ID, "error", err)
		http.Redirect(w, r, "/settings?csv_error="+code, http.StatusFound)
		return
	}
	http.Redirect(w, r, "/settings/import", http.StatusFound)
}

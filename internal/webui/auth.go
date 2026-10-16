package webui

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/accounts"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/auth"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/i18n"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mail"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// Login

const (
	sessionDuration      = 30 * 24 * time.Hour
	sessionRenewalWindow = 7 * 24 * time.Hour
)

func (h *Handler) setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionDuration / time.Second),
		HttpOnly: true,
		Secure:   h.secureCookie(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// LoginPage renders the login form.
func (h *Handler) LoginPage(w http.ResponseWriter, r *http.Request) {
	// If already logged in, redirect to home
	if account, _, _ := h.getSessionAccount(r); account != nil {
		w.Header().Set("Cache-Control", "private, no-store")
		redirectTo := r.URL.Query().Get("redirect")
		if safeLocalPath(redirectTo) {
			http.Redirect(w, r, redirectTo, http.StatusFound)
			return
		}
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// Pre-fill username from the configured single user
	registrationsOpen := h.cfg != nil && h.cfg.Auth.Registrations
	h.renderPage(w, r, h.pageTitle(r, "auth.sign_in_button"), map[string]interface{}{
		"DefaultUsername":   h.singleUser,
		"RegistrationsOpen": registrationsOpen,
		"InviteRequired":    h.cfg != nil && h.cfg.Auth.InviteRequired,
		"Redirect":          r.URL.Query().Get("redirect"),
	}, "login.html")
}

// renderLoginError redraws the login form after a failed attempt.
//
// It carries the redirect target back into the form: without it, one mistyped
// password during an OAuth flow drops the user on the home page and the app
// that sent them here waits forever.
func (h *Handler) renderLoginError(w http.ResponseWriter, r *http.Request, messageKey string) {
	h.renderPage(w, r, h.pageTitle(r, "auth.sign_in_button"), map[string]interface{}{
		"Error":           h.pageTitle(r, messageKey),
		"DefaultUsername": r.FormValue("username"),
		"Redirect":        r.FormValue("redirect"),
	}, "login.html")
}

// Login handles the login form submission (username + password).
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.renderError(w, r, "Invalid form data")
		return
	}

	// Rate limit: 5 login attempts per minute per IP
	ip := clientIP(r)
	if !h.checkAuthRate(&h.authRateLogin, ip, 5, time.Minute) {
		slog.Warn("webui: login rate limit exceeded", "ip", ip)
		h.renderLoginError(w, r, "auth.error.too_many_attempts")
		return
	}

	username := r.FormValue("username")
	submittedPassword := r.FormValue("password")

	if username == "" || submittedPassword == "" {
		h.renderLoginError(w, r, "auth.error.username_password_required")
		return
	}

	// Look up account by username (local accounts only: domain=nil)
	localAccount, err := h.store.GetAccountByUsernameDomain(r.Context(), username, nil)
	if err != nil {
		slog.Warn("webui: no local account found for login", "username", username, "error", err)
		// Generic error to prevent user enumeration
		h.renderLoginError(w, r, "auth.error.invalid_credentials")
		return
	}

	if !h.allowLogin(r, localAccount, submittedPassword) {
		slog.Warn("webui: rejected login", "username", username, "remote_addr", r.RemoteAddr)
		// Generic error to prevent user enumeration
		h.renderLoginError(w, r, "auth.error.invalid_credentials")
		return
	}
	if localAccount.MFAEnabled && !h.verifyMFA(r.Context(), localAccount, r.FormValue("mfa_code")) {
		slog.Warn("webui: rejected second factor", "username", username)
		h.renderLoginError(w, r, "auth.error.invalid_credentials")
		return
	}
	slog.Info("webui: login success", "username", localAccount.Username)

	// Invalidate any existing session for this browser to prevent session fixation
	if existingCookie, err := r.Cookie("session"); err == nil && existingCookie != nil {
		tokenHash := sha256Hex(existingCookie.Value)
		if existingSession, err := h.store.GetSessionByTokenHash(r.Context(), tokenHash); err == nil && existingSession != nil {
			if err := h.store.DeleteSession(r.Context(), existingSession.ID); err != nil {
				slog.Warn("webui: failed to delete existing session", "error", err)
			}
		}
	}

	// Create session
	token, tokenHash := generateSessionToken()
	session := &models.Session{
		AccountID: localAccount.ID,
		Token:     token,
		TokenHash: tokenHash,
		UserAgent: r.UserAgent(),
		IPAddress: r.RemoteAddr,
		ExpiresAt: time.Now().Add(sessionDuration),
	}

	if err := h.store.CreateSession(r.Context(), session); err != nil {
		slog.Error("webui: create session error", "error", err)
		h.renderError(w, r, "Error creating session")
		return
	}

	// Set session cookie - no Domain set so it scopes to the exact host
	// that set it (works with both localhost and 127.0.0.1)
	h.setSessionCookie(w, r, token)

	// Honour the redirect parameter (e.g. from OAuth flow), but only
	// allow same-site paths to prevent open redirect attacks.
	w.Header().Set("Cache-Control", "private, no-store")
	redirectTo := r.FormValue("redirect")
	if safeLocalPath(redirectTo) {
		http.Redirect(w, r, redirectTo, http.StatusFound)
		return
	}

	http.Redirect(w, r, "/", http.StatusFound)
}

func (h *Handler) verifyMFA(ctx context.Context, account *models.Account, code string) bool {
	if account == nil || !account.MFAEnabled || code == "" || h.cfg == nil {
		return false
	}
	secret, err := auth.DecryptTOTPSecret(h.cfg.Auth.MFAEncryptionKey, account.TOTPSecretEncrypted)
	if err == nil && auth.ValidateTOTP(secret, code) {
		return true
	}
	used, err := h.store.ConsumeMFARecoveryCode(ctx, account.ID, auth.RecoveryCodeHash(code))
	return err == nil && used
}

// Logout handles logout.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("session")
	if err == nil {
		tokenHash := sha256Hex(cookie.Value)
		session, err := h.store.GetSessionByTokenHash(r.Context(), tokenHash)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "Could not end the session. Please retry.", http.StatusInternalServerError)
			return
		}
		if err == nil {
			// Only this browser session ends. OAuth tokens are separate grants
			// per application, and signing out of the web UI must not silently
			// disconnect every phone and desktop client. Credential changes
			// (password reset, password change, account deletion) still revoke
			// them all.
			if err := h.store.DeleteSession(r.Context(), session.ID); err != nil {
				slog.Error("webui: logout failed", "error", err)
				http.Error(w, "Could not end the session. Please retry.", http.StatusInternalServerError)
				return
			}
		}
	}

	// Match the Secure/SameSite attributes of the original session cookie so
	// the browser actually deletes it. A deletion cookie without Secure will
	// not overwrite a Secure cookie on HTTPS deployments, leaving the user
	// apparently still logged in.
	secure := h.secureCookie(r)
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})

	// For htmx (boosted) requests, use HX-Redirect so the browser does a
	// full page navigation to the login page. A plain 302 would be followed
	// transparently by the XHR and htmx would swap the login page content
	// into the current URL without updating the address bar - making the
	// sign-out feel "lazy" and leaving stale state in history.
	w.Header().Set("Cache-Control", "private, no-store")
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/auth/sign_in")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
}

// Registration / Sign Up

// SignUpPage renders the registration form.
func (h *Handler) SignUpPage(w http.ResponseWriter, r *http.Request) {
	if account, _, _ := h.getSessionAccount(r); account != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	instanceRules := h.instanceMeta(r.Context()).Rules

	// If registration is closed, show a message instead of the form
	if h.cfg != nil && !h.cfg.Auth.Registrations {
		h.renderPage(w, r, h.pageTitle(r, "page.sign_up"), map[string]interface{}{
			"RegistrationClosed": true,
			"InstanceRules":      instanceRules,
		}, "signup.html")
		return
	}

	h.renderPage(w, r, h.pageTitle(r, "page.sign_up"), map[string]interface{}{
		"InviteRequired":    h.cfg != nil && h.cfg.Auth.InviteRequired,
		"RequireEmail":      h.cfg != nil && h.cfg.Auth.RequireEmail,
		"MinPasswordLength": minPasswordLen(h.cfg),
		"InstanceRules":     instanceRules,
	}, "signup.html")
}

// SignUp handles registration form submission.
func (h *Handler) SignUp(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.renderPage(w, r, h.pageTitle(r, "page.sign_up"), map[string]interface{}{"Error": h.pageTitle(r, "error.invalid_form_data")}, "signup.html")
		return
	}

	// Build registration config from handler config
	regCfg := buildRegConfig(h.cfg)
	instanceRules := h.instanceMeta(r.Context()).Rules

	req := &accounts.RegistrationRequest{
		Username:        r.FormValue("username"),
		Email:           r.FormValue("email"),
		Password:        r.FormValue("password"),
		PasswordConfirm: r.FormValue("password_confirm"),
		InviteCode:      r.FormValue("invite_code"),
	}

	account, err := accounts.Register(r.Context(), h.store, h.domain, regCfg, req)
	if err != nil {
		slog.Warn("webui: registration failed", "error", err)
		errMsg := h.pageTitle(r, "auth.error.registration_failed")
		if errors.Is(err, accounts.ErrRegistrationClosed) {
			errMsg = h.pageTitle(r, "auth.error.registration_closed")
		} else if errors.Is(err, accounts.ErrInviteRequired) || errors.Is(err, accounts.ErrInviteInvalid) || errors.Is(err, accounts.ErrInviteExhausted) {
			errMsg = h.pageTitle(r, "auth.error.invalid_invite")
		} else if errors.Is(err, accounts.ErrPasswordTooShort) {
			errMsg = h.pageTitle(r, "auth.error.password_too_short", minPasswordLen(h.cfg))
		} else if errors.Is(err, accounts.ErrPasswordMismatch) {
			errMsg = h.pageTitle(r, "auth.error.passwords_mismatch_period")
		} else if errors.Is(err, accounts.ErrInvalidUsername) {
			errMsg = h.pageTitle(r, "auth.error.invalid_username")
		} else if errors.Is(err, accounts.ErrEmailRequired) {
			errMsg = h.pageTitle(r, "auth.error.email_required")
		} else if errors.Is(err, accounts.ErrInvalidEmail) {
			errMsg = h.pageTitle(r, "auth.error.invalid_email")
		} else {
			// For ErrUsernameTaken, ErrEmailTaken and other internal errors, use generic message
			errMsg = h.pageTitle(r, "auth.error.registration_failed_generic")
		}
		h.renderPage(w, r, h.pageTitle(r, "page.sign_up"), map[string]interface{}{
			"Error":             errMsg,
			"Username":          req.Username,
			"Email":             req.Email,
			"InviteCode":        req.InviteCode,
			"InviteRequired":    regCfg.GetInviteRequired(),
			"RequireEmail":      regCfg.GetRequireEmail(),
			"MinPasswordLength": minPasswordLen(h.cfg),
			"InstanceRules":     instanceRules,
		}, "signup.html")
		return
	}
	if h.mailer != nil && account.Email != "" {
		if err := mail.EnqueueEmailVerification(r.Context(), h.store, h.domain, account); err != nil {
			slog.Error("webui: enqueue email verification", "error", err)
		}
	}
	if h.mailer != nil {
		mail.NotifyAccountCreated(r.Context(), h.store, h.domain, account, req.InviteCode)
	}

	// If approved, auto-login by creating session
	if account.Approved {
		token, tokenHash := generateSessionToken()
		session := &models.Session{
			AccountID: account.ID,
			Token:     token,
			TokenHash: tokenHash,
			UserAgent: r.UserAgent(),
			IPAddress: r.RemoteAddr,
			ExpiresAt: time.Now().Add(sessionDuration),
		}
		if err := h.store.CreateSession(r.Context(), session); err != nil {
			slog.Error("webui: create session after registration", "error", err)
		} else {
			h.setSessionCookie(w, r, token)
			w.Header().Set("Cache-Control", "private, no-store")
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}

	// Pending approval or auto-login failed
	h.renderPage(w, r, h.pageTitle(r, "page.sign_up"), map[string]interface{}{
		"Info": h.pageTitle(r, "auth.account_created") + " " + (func() string {
			if !account.Approved {
				return h.pageTitle(r, "auth.account_created_pending")
			}
			return h.pageTitle(r, "auth.account_created_go_sign_in")
		})(),
		"InviteRequired":    regCfg.GetInviteRequired(),
		"MinPasswordLength": minPasswordLen(h.cfg),
		"InstanceRules":     instanceRules,
	}, "signup.html")
}

// Password Reset

// PasswordResetRequestPage renders the password reset request form.
func (h *Handler) PasswordResetRequestPage(w http.ResponseWriter, r *http.Request) {
	h.renderPage(w, r, h.pageTitle(r, "page.reset_password"), map[string]interface{}{"MinPasswordLength": minPasswordLen(h.cfg)}, "password_reset_request.html")
}

// PasswordResetRequest handles the password reset request form submission.
func (h *Handler) PasswordResetRequest(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.renderPage(w, r, h.pageTitle(r, "page.reset_password"), map[string]interface{}{"Error": h.pageTitle(r, "error.invalid_form_data")}, "password_reset_request.html")
		return
	}

	// Rate limit: 1 password reset request per minute per IP
	ip := clientIP(r)
	if !h.checkAuthRate(&h.authRateReset, ip, 1, time.Minute) {
		slog.Warn("webui: password reset rate limit exceeded", "ip", ip)
		h.renderPage(w, r, h.pageTitle(r, "page.reset_password"), map[string]interface{}{
			"Info": h.pageTitle(r, "auth.error.too_many_requests"),
		}, "password_reset_request.html")
		return
	}

	username := r.FormValue("username")

	// Look up account by username
	account, err := h.store.GetAccountByUsernameDomain(r.Context(), username, nil)
	if err != nil {
		slog.Info("webui: password reset requested for unknown user", "username", username)
		// Don't reveal whether the account exists
		h.renderPage(w, r, h.pageTitle(r, "page.reset_password"), map[string]interface{}{
			"Info": h.pageTitle(r, "auth.reset_link_sent"),
		}, "password_reset_request.html")
		return
	}

	// Generate reset token
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		h.renderError(w, r, "Error generating token")
		return
	}
	token := hex.EncodeToString(b)
	tokenHash := sha256Hex(token)

	reset := &models.PasswordReset{
		AccountID: account.ID,
		TokenHash: tokenHash,
		ExpiresAt: time.Now().Add(15 * time.Minute),
	}

	if err := h.store.CreatePasswordReset(r.Context(), reset); err != nil {
		slog.Error("webui: create password reset", "error", err)
		h.renderError(w, r, "Error creating password reset")
		return
	}

	slog.Info("webui: password reset token generated", "username", username, "reset_id", reset.ID)

	// Queue the email so a temporary SMTP refusal does not lose the reset link.
	if h.mailer != nil && account.Email != "" && account.EmailVerified {
		subject := i18n.T(account.Locale, "mail.password_reset.subject", h.localDomain)
		body := i18n.T(account.Locale, "mail.password_reset.body", username, h.localDomain, h.domain, token)

		if err := mail.EnqueueEmail(r.Context(), h.store, mail.EmailJobPayload{
			To: account.Email, Subject: subject, Body: body, ExpiresAt: &reset.ExpiresAt,
		}); err != nil {
			slog.Error("webui: failed to queue password reset email", "error", err)
		}
	}

	// Use the same response for known and unknown accounts. Reset credentials
	// are never rendered or logged, even at debug level.
	templateData := map[string]interface{}{
		"MinPasswordLength": minPasswordLen(h.cfg),
		"Info":              h.pageTitle(r, "auth.reset_link_sent"),
	}

	h.renderPage(w, r, h.pageTitle(r, "page.reset_password"), templateData, "password_reset_request.html")
}

// PasswordResetEditPage renders the password reset form with a token.
// For security, the token is validated from the query param on first visit,
// then stored in a short-lived cookie to avoid exposing it in Referer/logs.
func (h *Handler) PasswordResetEditPage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")

	// If token is in the URL, validate it and set a cookie, then redirect to clean URL
	if token != "" {
		tokenHash := sha256Hex(token)
		reset, err := h.store.GetPasswordResetByTokenHash(r.Context(), tokenHash)
		if err != nil || reset.Used || time.Now().After(reset.ExpiresAt) {
			h.renderPage(w, r, h.pageTitle(r, "page.reset_password"), map[string]interface{}{"Error": h.pageTitle(r, "auth.error.invalid_or_expired_token")}, "password_reset_edit.html")
			return
		}

		// Set a short-lived cookie with the token hash, then redirect to clean URL
		secure := h.secureCookie(r)
		http.SetCookie(w, &http.Cookie{
			Name:     "pwreset_token",
			Value:    token,
			Path:     "/auth/password/edit",
			MaxAge:   900, // 15 minutes
			HttpOnly: true,
			Secure:   secure,
			SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, "/auth/password/edit", http.StatusFound)
		return
	}

	// No token in URL - check for the cookie
	tokenCookie, err := r.Cookie("pwreset_token")
	if err != nil || tokenCookie.Value == "" {
		h.renderPage(w, r, h.pageTitle(r, "page.reset_password"), map[string]interface{}{"Error": h.pageTitle(r, "auth.error.missing_token")}, "password_reset_edit.html")
		return
	}

	tokenHash := sha256Hex(tokenCookie.Value)
	reset, err := h.store.GetPasswordResetByTokenHash(r.Context(), tokenHash)
	if err != nil || reset.Used || time.Now().After(reset.ExpiresAt) {
		// Clear the invalid cookie
		http.SetCookie(w, &http.Cookie{
			Name:     "pwreset_token",
			Value:    "",
			Path:     "/auth/password/edit",
			MaxAge:   -1,
			HttpOnly: true,
			Secure:   h.secureCookie(r),
			SameSite: http.SameSiteLaxMode,
		})
		h.renderPage(w, r, h.pageTitle(r, "page.reset_password"), map[string]interface{}{"Error": h.pageTitle(r, "auth.error.invalid_or_expired_token")}, "password_reset_edit.html")
		return
	}

	h.renderPage(w, r, h.pageTitle(r, "page.reset_password"), map[string]interface{}{
		"MinPasswordLength": minPasswordLen(h.cfg),
	}, "password_reset_edit.html")
}

// PasswordResetEdit handles the password reset form submission.
func (h *Handler) PasswordResetEdit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.renderPage(w, r, h.pageTitle(r, "page.reset_password"), map[string]interface{}{"Error": h.pageTitle(r, "error.invalid_form_data")}, "password_reset_edit.html")
		return
	}

	// Read token from the secure cookie (set during the GET page redirect)
	tokenCookie, err := r.Cookie("pwreset_token")
	if err != nil || tokenCookie.Value == "" {
		h.renderPage(w, r, h.pageTitle(r, "page.reset_password"), map[string]interface{}{"Error": h.pageTitle(r, "auth.error.missing_token")}, "password_reset_edit.html")
		return
	}
	token := tokenCookie.Value

	password := r.FormValue("password")
	confirm := r.FormValue("password_confirm")

	if token == "" || password == "" {
		h.renderPage(w, r, h.pageTitle(r, "page.reset_password"), map[string]interface{}{"Error": h.pageTitle(r, "auth.error.missing_required_fields")}, "password_reset_edit.html")
		return
	}

	if password != confirm {
		h.renderPage(w, r, h.pageTitle(r, "page.reset_password"), map[string]interface{}{"Error": h.pageTitle(r, "auth.error.passwords_mismatch")}, "password_reset_edit.html")
		return
	}

	// Validate password length
	minLen := minPasswordLen(h.cfg)
	if err := accounts.ValidatePassword(password, minLen); err != nil {
		h.renderPage(w, r, h.pageTitle(r, "page.reset_password"), map[string]interface{}{"Error": h.pageTitle(r, "auth.error.password_too_short_edit", minLen)}, "password_reset_edit.html")
		return
	}

	// Find the reset token
	tokenHash := sha256Hex(token)
	reset, err := h.store.GetPasswordResetByTokenHash(r.Context(), tokenHash)
	if err != nil {
		h.renderPage(w, r, h.pageTitle(r, "page.reset_password"), map[string]interface{}{"Error": h.pageTitle(r, "auth.error.invalid_or_expired_token")}, "password_reset_edit.html")
		return
	}

	// Check expiry
	if time.Now().After(reset.ExpiresAt) {
		h.renderPage(w, r, h.pageTitle(r, "page.reset_password"), map[string]interface{}{"Error": h.pageTitle(r, "auth.error.token_expired")}, "password_reset_edit.html")
		return
	}

	// Check if already used
	if reset.Used {
		h.renderPage(w, r, h.pageTitle(r, "page.reset_password"), map[string]interface{}{"Error": h.pageTitle(r, "auth.error.token_used")}, "password_reset_edit.html")
		return
	}

	// Claim the token atomically (UPDATE WHERE used=false). If two concurrent
	// requests race, only one will succeed - the other gets a spurious "invalid
	// token" error, which is safe (it means the password was already changed).
	if err := h.store.MarkPasswordResetUsed(r.Context(), reset.ID); err != nil {
		slog.Warn("webui: password reset token already claimed or expired", "error", err)
		h.renderPage(w, r, h.pageTitle(r, "page.reset_password"), map[string]interface{}{"Error": h.pageTitle(r, "auth.error.token_used_or_invalid")}, "password_reset_edit.html")
		return
	}

	// Hash the new password
	hash, err := auth.HashPassword(password)
	if err != nil {
		h.renderError(w, r, "Error hashing password")
		return
	}

	// Update the account password
	account, err := h.store.GetAccount(r.Context(), reset.AccountID)
	if err != nil {
		h.renderError(w, r, "Account not found")
		return
	}

	account.PasswordHash = hash
	if err := h.store.UpdateAccount(r.Context(), account); err != nil {
		slog.Error("webui: update password after reset", "error", err)
		h.renderError(w, r, "Error updating password")
		return
	}

	// Clear the reset token cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "pwreset_token",
		Value:    "",
		Path:     "/auth/password/edit",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.secureCookie(r),
		SameSite: http.SameSiteLaxMode,
	})

	slog.Info("webui: password reset completed", "username", account.Username)

	// Redirect to login with success message
	h.renderPage(w, r, h.pageTitle(r, "page.password_reset_done"), map[string]interface{}{
		"Info": h.pageTitle(r, "auth.password_changed_success"),
	}, "login.html")
}

// VerifyEmail handles email verification via token link.
func (h *Handler) VerifyEmail(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		h.renderError(w, r, h.pageTitle(r, "auth.error.missing_verification_token"))
		return
	}

	tokenHash := sha256Hex(token)
	now := time.Now()
	account, err := h.store.VerifyEmail(r.Context(), tokenHash, now)
	if err != nil {
		h.renderError(w, r, h.pageTitle(r, "auth.error.invalid_verification_link"))
		return
	}

	slog.Info("webui: email verified", "username", account.Username)
	h.renderPage(w, r, h.pageTitle(r, "page.email_verified"), map[string]interface{}{
		"Info": h.pageTitle(r, "auth.email_verified_thanks"),
	}, "login.html")
}

func (h *Handler) ResendVerificationPage(w http.ResponseWriter, r *http.Request) {
	h.renderPage(w, r, h.pageTitle(r, "auth.resend_verification.title"), nil, "email_verification_resend.html")
}

func (h *Handler) ResendVerification(w http.ResponseWriter, r *http.Request) {
	info := h.pageTitle(r, "auth.resend_verification.sent")
	if err := r.ParseForm(); err == nil && h.checkAuthRate(&h.authRateVerify, clientIP(r), 5, time.Hour) {
		if account, err := h.store.GetAccountByEmail(r.Context(), r.FormValue("email")); err == nil && !account.EmailVerified && account.DeletionState == "" {
			if err := mail.EnqueueEmailVerification(r.Context(), h.store, h.domain, account); err != nil {
				slog.Error("webui: resend email verification", "error", err)
			}
		}
	}
	h.renderPage(w, r, h.pageTitle(r, "auth.resend_verification.title"), map[string]interface{}{"Info": info}, "email_verification_resend.html")
}

package oauth

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// tokenRequest holds the parameters of a token or revocation request,
// whichever body encoding the client used.
type tokenRequest struct {
	GrantType    string `json:"grant_type"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Code         string `json:"code"`
	RedirectURI  string `json:"redirect_uri"`
	CodeVerifier string `json:"code_verifier"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	Token        string `json:"token"`
}

// parseTokenRequest reads JSON, form-encoded and multipart bodies, then lets
// HTTP Basic credentials override the body ones. Basic is the
// client_secret_basic method from RFC 6749 §2.3.1, which generic OAuth
// libraries use by default.
func parseTokenRequest(r *http.Request) tokenRequest {
	var req tokenRequest
	if jsonRequest(r) {
		_ = json.NewDecoder(r.Body).Decode(&req)
	} else if err := parseOAuthForm(r); err == nil {
		req = tokenRequest{
			GrantType:    r.FormValue("grant_type"),
			ClientID:     r.FormValue("client_id"),
			ClientSecret: r.FormValue("client_secret"),
			Code:         r.FormValue("code"),
			RedirectURI:  r.FormValue("redirect_uri"),
			CodeVerifier: r.FormValue("code_verifier"),
			RefreshToken: r.FormValue("refresh_token"),
			Scope:        r.FormValue("scope"),
			Token:        r.FormValue("token"),
		}
	}
	if id, secret, ok := r.BasicAuth(); ok && id != "" {
		// RFC 6749 §2.3.1 form-encodes the two halves before Base64.
		if unescaped, err := url.QueryUnescape(id); err == nil {
			id = unescaped
		}
		if unescaped, err := url.QueryUnescape(secret); err == nil {
			secret = unescaped
		}
		req.ClientID, req.ClientSecret = id, secret
	}
	return req
}

// Token handles POST /oauth/token - exchange code for access token.
func (h *Handler) Token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	req := parseTokenRequest(r)

	switch req.GrantType {
	case "":
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "grant_type is required")
	case "authorization_code":
		h.tokenAuthCode(w, r, req)
	case "client_credentials":
		h.tokenClientCredentials(w, r, req)
	case "refresh_token":
		h.tokenRefresh(w, r, req)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "unsupported grant_type")
	}
}

// authenticateClient verifies the presented client credentials against an
// application. A secret is mandatory unless the transaction was protected by
// PKCE, which is how RFC 8252 public clients (no secret to keep) authenticate.
// A secret that is presented is always checked, whether required or not.
func authenticateClient(app *models.OAuthApplication, req tokenRequest, secretRequired bool) bool {
	if app == nil || app.ClientID != req.ClientID {
		return false
	}
	if req.ClientSecret == "" {
		return !secretRequired
	}
	return secretMatches(app, req.ClientSecret)
}

func (h *Handler) tokenClientCredentials(w http.ResponseWriter, r *http.Request, req tokenRequest) {
	app, err := h.store.GetOAuthAppByClientID(r.Context(), req.ClientID)
	if err != nil || !authenticateClient(app, req, true) {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "invalid client")
		return
	}
	scope := req.Scope
	if scope == "" {
		scope = "read"
	}
	if !scopesAllowed(app.Scopes, strings.Fields(scope)) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_scope", "requested scope was not registered for this application")
		return
	}

	// An app token carries no account, so it authenticates no user: the API
	// middleware fails its account lookup. It is stored anyway so
	// /api/v1/apps/verify_credentials can resolve it back to the application.
	accessToken := generateSecret(48)
	if err := h.store.CreateOAuthToken(r.Context(), &models.OAuthToken{
		AccessToken:   accessToken,
		RefreshToken:  generateSecret(32),
		AccountID:     "",
		ApplicationID: app.ID,
		Scopes:        scope,
	}); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "error creating token")
		return
	}

	writeAPJSON(w, http.StatusOK, map[string]interface{}{
		"access_token": accessToken,
		"token_type":   "Bearer",
		"scope":        scope,
		"created_at":   time.Now().Unix(),
	})
}

func (h *Handler) tokenAuthCode(w http.ResponseWriter, r *http.Request, req tokenRequest) {
	if req.Code == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "code is required")
		return
	}

	existingToken, err := h.store.GetOAuthTokenByAccess(r.Context(), req.Code)
	if err != nil {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_grant", "invalid authorization code")
		return
	}
	if !existingToken.AuthorizationCode || existingToken.ExpiresAt == nil || time.Now().After(*existingToken.ExpiresAt) {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_grant", "invalid or expired authorization code")
		return
	}

	app, err := h.store.GetOAuthApp(r.Context(), existingToken.ApplicationID)
	if err != nil || !authenticateClient(app, req, existingToken.CodeChallenge == "") {
		// Say which half is missing. A client that loses track of its own
		// registration sends neither, and "invalid client" alone gives whoever
		// is reading the log nothing to go on.
		description := "invalid client"
		switch {
		case req.ClientID == "":
			description = "client_id is required"
		case req.ClientSecret == "" && existingToken.CodeChallenge == "":
			description = "client_secret is required when the authorization did not use PKCE"
		}
		slog.Warn("oauth: client authentication failed in token exchange",
			"reason", description, "client_id_present", req.ClientID != "", "secret_present", req.ClientSecret != "")
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", description)
		return
	}

	// Mastodon lets a client omit redirect_uri here and reuses the one from the
	// authorization request; when it is sent it must be the same callback.
	if req.RedirectURI != "" && req.RedirectURI != existingToken.RedirectURI {
		slog.Warn("oauth: redirect_uri mismatch in token exchange", "app", app.Name)
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the authorization request")
		return
	}

	if existingToken.CodeChallenge != "" {
		computed := pkceChallenge(req.CodeVerifier)
		if !validPKCEVerifier(req.CodeVerifier) ||
			subtle.ConstantTimeCompare([]byte(computed), []byte(existingToken.CodeChallenge)) != 1 {
			writeOAuthError(w, http.StatusUnauthorized, "invalid_grant", "invalid code_verifier")
			return
		}
	} else if req.CodeVerifier != "" {
		// A verifier on a transaction that never carried a challenge indicates a
		// downgrade or client-state mix-up; never silently accept it.
		writeOAuthError(w, http.StatusUnauthorized, "invalid_grant", "unexpected code_verifier")
		return
	}

	account, err := h.store.GetAccount(r.Context(), existingToken.AccountID)
	if err != nil || !account.CanAuthenticate(h.requireEmail, h.requireAdminMFA) {
		writeOAuthError(w, http.StatusForbidden, "invalid_grant", "account is not permitted to authenticate")
		return
	}

	// Burn the code before issuing, so a concurrent replay loses the race.
	consumed, err := h.store.ConsumeOAuthAuthorizationCode(r.Context(), req.Code)
	if err != nil || !consumed {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_grant", "authorization code has already been used")
		return
	}

	accessToken, refreshToken := generateSecret(48), generateSecret(48)
	newToken := &models.OAuthToken{
		AccessToken:   accessToken,
		RefreshToken:  refreshToken,
		AccountID:     existingToken.AccountID,
		ApplicationID: existingToken.ApplicationID,
		Scopes:        existingToken.Scopes,
	}
	if err := h.store.CreateOAuthToken(r.Context(), newToken); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "error creating token")
		return
	}

	writeAPJSON(w, http.StatusOK, map[string]interface{}{
		"access_token":  accessToken,
		"token_type":    "Bearer",
		"scope":         newToken.Scopes,
		"created_at":    time.Now().Unix(),
		"refresh_token": refreshToken,
		"account_id":    account.ID,
		"account_uri":   account.ActorURI,
	})
}

// tokenRefresh handles grant_type=refresh_token: it rotates both the access
// and refresh secrets on the existing token row so a leaked, since-rotated
// refresh_token can't be replayed to mint another pair.
func (h *Handler) tokenRefresh(w http.ResponseWriter, r *http.Request, req tokenRequest) {
	if req.RefreshToken == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}

	existingToken, err := h.store.GetOAuthTokenByRefresh(r.Context(), req.RefreshToken)
	if err != nil || existingToken.AuthorizationCode || existingToken.AccountID == "" {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_grant", "invalid refresh_token")
		return
	}

	app, err := h.store.GetOAuthApp(r.Context(), existingToken.ApplicationID)
	if err != nil || !authenticateClient(app, req, true) {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "invalid client")
		return
	}

	account, err := h.store.GetAccount(r.Context(), existingToken.AccountID)
	if err != nil || !account.CanAuthenticate(h.requireEmail, h.requireAdminMFA) {
		writeOAuthError(w, http.StatusForbidden, "invalid_grant", "account is not permitted to authenticate")
		return
	}

	newAccessToken, newRefreshToken := generateSecret(48), generateSecret(48)
	if err := h.store.RotateOAuthToken(r.Context(), existingToken.ID, req.RefreshToken, newAccessToken, newRefreshToken); errors.Is(err, sql.ErrNoRows) {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_grant", "refresh token is no longer valid")
		return
	} else if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "error rotating token")
		return
	}

	writeAPJSON(w, http.StatusOK, map[string]interface{}{
		"access_token":  newAccessToken,
		"token_type":    "Bearer",
		"scope":         existingToken.Scopes,
		"created_at":    time.Now().Unix(),
		"refresh_token": newRefreshToken,
		"account_id":    account.ID,
		"account_uri":   account.ActorURI,
	})
}

// Revoke handles POST /oauth/revoke for Mastodon-compatible clients.
func (h *Handler) Revoke(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	req := parseTokenRequest(r)
	if req.ClientID == "" || req.ClientSecret == "" || req.Token == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "client_id, client_secret and token are required")
		return
	}

	app, err := h.store.GetOAuthAppByClientID(r.Context(), req.ClientID)
	if err != nil || !secretMatches(app, req.ClientSecret) {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "invalid client")
		return
	}

	existing, err := h.store.GetOAuthTokenByAccess(r.Context(), req.Token)
	if err == nil && existing.ApplicationID == app.ID {
		if err := h.store.RevokeOAuthToken(r.Context(), req.Token); err != nil {
			writeOAuthError(w, http.StatusInternalServerError, "server_error", "error revoking token")
			return
		}
	}
	// OAuth revocation is idempotent: a missing/already-revoked token succeeds.
	writeAPJSON(w, http.StatusOK, map[string]interface{}{})
}

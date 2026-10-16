// Package oauth provides OAuth2 endpoints for Mastodon API compatibility.
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Handler handles OAuth2 endpoints for Mastodon client registration and auth.
type Handler struct {
	store           storage.Storage
	domain          string
	vapidPublic     string // VAPID public key for Web Push (optional)
	requireEmail    bool
	requireAdminMFA bool
	consentRenderer ConsentRenderer
}

// ConsentPageData contains the validated values shown on the OAuth consent page.
type ConsentPageData struct {
	AppName, AppWebsite, Username                     string
	ClientID, RedirectURI, ResponseType, Scope, State string
	CodeChallenge, CodeChallengeMethod, CSRFToken     string
	Scopes                                            []string

	// AllowURL and DenyURL are the client's callback, already carrying the
	// authorization code or the access_denied error. When they are set the
	// consent controls must be plain links to them rather than a form: see
	// the comment on issueAuthorizationCode. They are empty for the
	// out-of-band callback, which has nowhere to send the user, and the form
	// is used instead.
	AllowURL, DenyURL string
}

// ConsentRenderer lets the web UI render OAuth consent inside its normal theme.
type ConsentRenderer func(http.ResponseWriter, *http.Request, ConsentPageData)

func (h *Handler) SetConsentRenderer(renderer ConsentRenderer) {
	h.consentRenderer = renderer
}

func (h *Handler) SetAuthenticationPolicy(requireEmail, requireAdminMFA bool) {
	h.requireEmail, h.requireAdminMFA = requireEmail, requireAdminMFA
}

// NewHandler creates a new OAuth handler.
func NewHandler(store storage.Storage, domain string) *Handler {
	return &Handler{store: store, domain: domain}
}

// NewHandlerWithVAPID creates a new OAuth handler with a VAPID public key.
func NewHandlerWithVAPID(store storage.Storage, domain, vapidPublic string) *Handler {
	return &Handler{store: store, domain: domain, vapidPublic: vapidPublic}
}

// RegisterRoutes adds OAuth routes to the router.
func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Post("/api/v1/apps", h.CreateApp)
	r.Get("/api/v1/apps/verify_credentials", h.AppVerifyCredentials)
	r.Get("/.well-known/oauth-authorization-server", h.Metadata)
	r.Get("/oauth/authorize", h.Authorize)
	r.Post("/oauth/authorize", h.AuthorizeDecision)
	r.Post("/oauth/token", h.Token)
	r.Post("/oauth/revoke", h.Revoke)
}

// Metadata advertises the security properties clients need to start an OAuth
// flow without probing or silently downgrading PKCE.
func (h *Handler) Metadata(w http.ResponseWriter, _ *http.Request) {
	base := "https://" + h.domain
	writeAPJSON(w, http.StatusOK, map[string]interface{}{
		"issuer":                           base,
		"authorization_endpoint":           base + "/oauth/authorize",
		"token_endpoint":                   base + "/oauth/token",
		"revocation_endpoint":              base + "/oauth/revoke",
		"registration_endpoint":            base + "/api/v1/apps",
		"response_types_supported":         []string{"code"},
		"grant_types_supported":            []string{"authorization_code", "refresh_token", "client_credentials"},
		"code_challenge_methods_supported": []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{
			"client_secret_post", "client_secret_basic", "none",
		},
		"scopes_supported": supportedScopes(),
	})
}

// getSessionAccount extracts the account from the web session cookie.
func (h *Handler) getSessionAccount(r *http.Request) *models.Account {
	cookie, err := r.Cookie("session")
	if err != nil {
		return nil
	}

	tokenHash := sha256Hex(cookie.Value)
	session, err := h.store.GetSessionByTokenHash(r.Context(), tokenHash)
	if err != nil {
		return nil
	}

	account, err := h.store.GetAccount(r.Context(), session.AccountID)
	if err != nil || !account.CanAuthenticate(h.requireEmail, h.requireAdminMFA) {
		return nil
	}

	return account
}

// getSessionToken extracts the raw session token from the cookie.
func (h *Handler) getSessionToken(r *http.Request) string {
	cookie, err := r.Cookie("session")
	if err != nil {
		return ""
	}
	return cookie.Value
}

// csrfToken derives a CSRF token from the session token.
func csrfToken(sessionToken string) string {
	return sha256Hex("csrf:" + sessionToken)
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func parseOAuthForm(r *http.Request) error {
	if strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		return r.ParseMultipartForm(64 << 10)
	}
	return r.ParseForm()
}

func jsonRequest(r *http.Request) bool {
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	return strings.Contains(ct, "/json") || strings.Contains(ct, "+json")
}

// Helpers

func writeAPJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// writeAPIError reports a Mastodon-style failure, where the human sentence goes
// in "error". Used by the application registration endpoints.
func writeAPIError(w http.ResponseWriter, status int, description string) {
	writeAPJSON(w, status, map[string]interface{}{
		"error":             description,
		"error_description": description,
	})
}

// writeOAuthError reports an RFC 6749 §5.2 failure: a machine-readable code in
// "error" and the sentence in "error_description". Clients branch on the code,
// so putting prose there leaves them with an unrecognized error and no retry.
func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Cache-Control", "no-store")
	writeAPJSON(w, status, map[string]interface{}{
		"error":             code,
		"error_description": description,
	})
}

func generateSecret(length int) string {
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		return uuid.New().String()
	}
	return hex.EncodeToString(b)
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func validPKCEParams(challenge, method string) bool {
	if challenge == "" && method == "" {
		// Mastodon-compatible clients historically omit PKCE. When present it is
		// fully enforced; metadata advertises S256 so modern clients can use it.
		return true
	}
	if method != "S256" || len(challenge) != 43 {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(challenge)
	return err == nil && len(decoded) == sha256.Size
}

func validPKCEVerifier(verifier string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	for _, c := range []byte(verifier) {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~') {
			return false
		}
	}
	return true
}

// secretMatches compares a presented client secret against the stored digest.
func secretMatches(app *models.OAuthApplication, secret string) bool {
	if app == nil || secret == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(app.ClientSecret), []byte(sha256Hex(secret))) == 1
}

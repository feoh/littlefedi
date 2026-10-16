package oauth

import (
	"crypto/subtle"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// authorizationCodeTTL bounds how long a code may sit unexchanged.
const authorizationCodeTTL = 10 * time.Minute

// Authorize handles GET /oauth/authorize - show authorization consent page.
func (h *Handler) Authorize(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	clientID := query.Get("client_id")
	responseType := query.Get("response_type")
	scope := query.Get("scope")
	state := query.Get("state")
	codeChallenge := query.Get("code_challenge")
	codeChallengeMethod := query.Get("code_challenge_method")

	if clientID == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "client_id is required")
		return
	}
	app, err := h.store.GetOAuthAppByClientID(r.Context(), clientID)
	if err != nil {
		writeOAuthError(w, http.StatusNotFound, "invalid_client", "application not found")
		return
	}
	redirectURI, ok := resolveRedirectURI(app.RedirectURIs, query.Get("redirect_uri"))
	if !ok {
		// The callback is unusable, so the failure cannot be delivered through
		// it without becoming an open redirect. This is the one case RFC 6749
		// keeps on the server.
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "redirect_uri not registered for this application")
		return
	}

	// Past this point the client's callback is trusted, so every failure is
	// reported there: an app's web authentication session only closes when it
	// sees its own scheme, and an error page it cannot read leaves it hanging.
	if responseType != "code" {
		redirectToClientError(w, r, redirectURI, state, "unsupported_response_type", "only response_type=code is supported")
		return
	}
	if !validPKCEParams(codeChallenge, codeChallengeMethod) {
		redirectToClientError(w, r, redirectURI, state, "invalid_request", "invalid PKCE parameters; only S256 is supported")
		return
	}
	AllowCallbackInCSP(w, redirectURI)

	// Authenticate via session cookie (web UI flow)
	account := h.getSessionAccount(r)
	if account == nil {
		// Not logged in - redirect to login, then back here
		authURL := fmt.Sprintf("/auth/sign_in?redirect=%s", url.QueryEscape(r.URL.RequestURI()))
		http.Redirect(w, r, authURL, http.StatusFound)
		return
	}

	scopes := strings.Fields(scope)
	if len(scopes) == 0 {
		scopes = []string{"read"}
		scope = "read"
	}
	if !scopesAllowed(app.Scopes, scopes) {
		redirectToClientError(w, r, redirectURI, state, "invalid_scope", "requested scope was not registered for this application")
		return
	}

	data := ConsentPageData{
		AppName: app.Name, AppWebsite: app.Website, Username: account.Username,
		ClientID: clientID, RedirectURI: redirectURI, ResponseType: responseType,
		Scope: scope, State: state, CodeChallenge: codeChallenge,
		CodeChallengeMethod: codeChallengeMethod, CSRFToken: csrfToken(h.getSessionToken(r)),
		Scopes: scopes,
	}
	if redirectURI != oobRedirectURI {
		code, err := h.issueAuthorizationCode(r, account, app, redirectURI, scope, codeChallenge, codeChallengeMethod)
		if err != nil {
			redirectToClientError(w, r, redirectURI, state, "server_error", "error creating authorization")
			return
		}
		data.AllowURL = buildRedirectURL(redirectURI, map[string]string{"code": code, "state": state})
		data.DenyURL = buildRedirectURL(redirectURI, map[string]string{
			"error": "access_denied", "error_description": "the user denied authorization", "state": state,
		})
	}

	if h.consentRenderer != nil {
		h.consentRenderer(w, r, data)
		return
	}
	h.renderFallbackConsent(w, data)
}

// issueAuthorizationCode stores a fresh authorization code for this account and
// application.
//
// The code is minted while the consent page is being drawn, so that Allow and
// Deny can be plain links straight to the client's callback. Submitting a form
// and answering it with a redirect looks equivalent, but it does not work for
// the clients that run the login inside ASWebAuthenticationSession: WebKit will
// not leave for an application's own URL scheme as the outcome of a form
// submission, so the code is issued, the redirect is sent, and the web view
// stays where it is. Following a
// link the user has just tapped is the one navigation that is always allowed.
//
// A code minted here and never tapped is never handed to anyone, and expires
// on its own.
func (h *Handler) issueAuthorizationCode(r *http.Request, account *models.Account, app *models.OAuthApplication,
	redirectURI, scope, codeChallenge, codeChallengeMethod string) (string, error) {

	code := generateSecret(32)
	expiresAt := time.Now().Add(authorizationCodeTTL)
	token := &models.OAuthToken{
		AccessToken:         code,
		RefreshToken:        generateSecret(32),
		AccountID:           account.ID,
		ApplicationID:       app.ID,
		Scopes:              scope,
		AuthorizationCode:   true,
		RedirectURI:         redirectURI,
		CodeChallenge:       codeChallenge,
		CodeChallengeMethod: codeChallengeMethod,
		ExpiresAt:           &expiresAt,
	}
	if err := h.store.CreateOAuthToken(r.Context(), token); err != nil {
		return "", err
	}
	return code, nil
}

// AuthorizeDecision handles POST /oauth/authorize - process consent decision.
func (h *Handler) AuthorizeDecision(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := parseOAuthForm(r); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid form data")
		return
	}

	clientID := r.FormValue("client_id")
	scope := r.FormValue("scope")
	state := r.FormValue("state")
	decision := r.FormValue("decision")
	responseType := r.FormValue("response_type")
	codeChallenge := r.FormValue("code_challenge")
	codeChallengeMethod := r.FormValue("code_challenge_method")

	// Authenticate via session cookie
	account := h.getSessionAccount(r)
	if account == nil {
		writeOAuthError(w, http.StatusUnauthorized, "access_denied", "not logged in")
		return
	}

	// CSRF protection - verify the csrf_token hidden field
	sessionToken := h.getSessionToken(r)
	submittedCSRF := r.FormValue("csrf_token")
	if sessionToken == "" || submittedCSRF == "" || subtle.ConstantTimeCompare([]byte(submittedCSRF), []byte(csrfToken(sessionToken))) != 1 {
		slog.Warn("oauth: CSRF token mismatch on consent decision", "remote_addr", r.RemoteAddr)
		writeOAuthError(w, http.StatusForbidden, "access_denied", "invalid CSRF token")
		return
	}

	app, err := h.store.GetOAuthAppByClientID(r.Context(), clientID)
	if err != nil {
		writeOAuthError(w, http.StatusNotFound, "invalid_client", "application not found")
		return
	}
	redirectURI, ok := resolveRedirectURI(app.RedirectURIs, r.FormValue("redirect_uri"))
	if !ok {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "redirect_uri not registered for this application")
		return
	}
	AllowCallbackInCSP(w, redirectURI)
	w.Header().Set("Cache-Control", "no-store")

	if responseType != "code" {
		redirectToClientError(w, r, redirectURI, state, "unsupported_response_type", "only response_type=code is supported")
		return
	}
	if !validPKCEParams(codeChallenge, codeChallengeMethod) {
		redirectToClientError(w, r, redirectURI, state, "invalid_request", "invalid PKCE parameters; only S256 is supported")
		return
	}
	requestedScopes := strings.Fields(scope)
	if !scopesAllowed(app.Scopes, requestedScopes) {
		redirectToClientError(w, r, redirectURI, state, "invalid_scope", "requested scope was not registered for this application")
		return
	}

	if decision != "allow" {
		redirectToClientError(w, r, redirectURI, state, "access_denied", "the user denied authorization")
		return
	}

	code, err := h.issueAuthorizationCode(r, account, app, redirectURI, scope, codeChallenge, codeChallengeMethod)
	if err != nil {
		redirectToClientError(w, r, redirectURI, state, "server_error", "error creating authorization")
		return
	}

	if redirectURI != oobRedirectURI {
		redirectToCallback(w, r, buildRedirectURL(redirectURI, map[string]string{
			"code":  code,
			"state": state,
		}))
		return
	}

	// Out-of-band: the user copies the code across by hand.
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		renderOOBCode(w, app.Name, code)
		return
	}
	writeAPJSON(w, http.StatusOK, map[string]interface{}{"code": code})
}

func renderOOBCode(w http.ResponseWriter, appName, code string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head><meta charset="UTF-8"><meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Authorization code - littleFedi</title></head>
<body>
<main>
<h1>Authorization code</h1>
<p>Copy this code into %s to finish signing in.</p>
<p><code>%s</code></p>
</main>
</body>
</html>`, template.HTMLEscapeString(appName), template.HTMLEscapeString(code))
}

// renderFallbackConsent draws the consent screen when no themed renderer is
// wired up (tests, and any embedding that skips the web UI).
func (h *Handler) renderFallbackConsent(w http.ResponseWriter, data ConsentPageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	var badges strings.Builder
	for _, s := range data.Scopes {
		fmt.Fprintf(&badges, `<span class="scope-badge">%s</span>`, template.HTMLEscapeString(s))
	}

	// Links straight to the callback, for the reason given on
	// issueAuthorizationCode. Only the out-of-band callback, which has nowhere
	// to send the user, still posts the decision back here.
	controls := fmt.Sprintf(
		`<div class="actions"><a href="%s" rel="nofollow" class="btn btn-allow">Allow</a>`+
			`<a href="%s" rel="nofollow" class="btn btn-deny">Deny</a></div>`,
		template.HTMLEscapeString(data.AllowURL), template.HTMLEscapeString(data.DenyURL))
	if data.AllowURL == "" {
		controls = fmt.Sprintf(`<form method="post" action="/oauth/authorize" class="actions">
<input type="hidden" name="client_id" value="%s">
<input type="hidden" name="redirect_uri" value="%s">
<input type="hidden" name="response_type" value="%s">
<input type="hidden" name="scope" value="%s">
<input type="hidden" name="state" value="%s">
<input type="hidden" name="code_challenge" value="%s">
<input type="hidden" name="code_challenge_method" value="%s">
<input type="hidden" name="csrf_token" value="%s">
<button type="submit" name="decision" value="allow" class="btn btn-allow">Allow</button>
<button type="submit" name="decision" value="deny" class="btn btn-deny">Deny</button>
</form>`,
			template.HTMLEscapeString(data.ClientID),
			template.HTMLEscapeString(data.RedirectURI),
			template.HTMLEscapeString(data.ResponseType),
			template.HTMLEscapeString(data.Scope),
			template.HTMLEscapeString(data.State),
			template.HTMLEscapeString(data.CodeChallenge),
			template.HTMLEscapeString(data.CodeChallengeMethod),
			template.HTMLEscapeString(data.CSRFToken))
	}

	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Authorize - littleFedi</title>
<style>
:root{--bg:#f5f7fa;--surface:#fff;--text:#171a21;--muted:#6b7385;--accent:#0e9488;--radius:14px;--shadow:0 4px 16px rgba(15,23,42,.06)}
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;background:var(--bg);color:var(--text);display:flex;align-items:center;justify-content:center;min-height:100vh}
.card{background:var(--surface);border-radius:var(--radius);padding:2rem;max-width:440px;width:90%%;box-shadow:var(--shadow)}
h1{font-size:1.4rem;margin-bottom:.5rem}
.app-name{font-weight:700;color:var(--accent)}
.scopes{display:flex;flex-wrap:wrap;gap:.4rem;margin:1rem 0}
.scope-badge{background:rgba(14,148,136,.1);color:var(--accent);padding:.3rem .7rem;border-radius:999px;font-size:.8rem;font-weight:600}
.actions{display:flex;gap:.6rem;margin-top:1.2rem}
.btn{display:inline-flex;align-items:center;justify-content:center;padding:.6rem 1.2rem;border-radius:999px;font-size:.9rem;font-weight:650;border:none;cursor:pointer;font-family:inherit;text-decoration:none}
.btn-allow{background:var(--accent);color:#fff;flex:1}
.btn-deny{background:#e5e7eb;color:#374151;flex:1}
.user-info{font-size:.8rem;color:var(--muted);margin-top:1rem}
</style>
</head>
<body>
<div class="card">
<h1>Authorize <span class="app-name">%s</span></h1>
<p style="color:var(--muted);margin-top:.3rem">This application wants permission to:</p>
<div class="scopes">%s</div>
%s
<p class="user-info">Signed in as <strong>@%s</strong></p>
</div>
</body>
</html>`,
		template.HTMLEscapeString(data.AppName),
		badges.String(),
		controls,
		template.HTMLEscapeString(data.Username),
	)
}

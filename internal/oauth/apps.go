package oauth

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// defaultAppScopes is what an application gets when it registers without asking
// for anything. "push" is included because /api/v1/push/subscription requires
// it and a client cannot add a scope after registration.
const defaultAppScopes = "read write follow push"

// appRegistration is a client's registration request. redirect_uris and scopes
// are kept raw because clients send them as either a string or an array:
// Mastodon 4.3 accepts both, and RFC 7591 clients always send an array.
type appRegistration struct {
	ClientName   string          `json:"client_name"`
	RedirectURIs json.RawMessage `json:"redirect_uris"`
	Scopes       json.RawMessage `json:"scopes"`
	Website      string          `json:"website"`
}

// jsonStringOrList accepts a JSON string or array of strings, joining a list
// with sep.
func jsonStringOrList(raw json.RawMessage, sep string) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", true
	}
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return single, true
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return strings.Join(list, sep), true
	}
	return "", false
}

// formRedirectURIs collects every callback a form-encoded registration carries.
// Clients send repeated "redirect_uris" fields, or the bracketed
// "redirect_uris[]" form; reading only the first value silently discarded them.
func formRedirectURIs(r *http.Request) string {
	values := append([]string{}, r.Form["redirect_uris"]...)
	values = append(values, r.Form["redirect_uris[]"]...)
	return strings.Join(values, "\n")
}

// CreateApp handles POST /api/v1/apps - register a new client application.
func (h *Handler) CreateApp(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var clientName, redirectURIs, scopes, website string
	if jsonRequest(r) {
		var body appRegistration
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid or oversized application registration")
			return
		}
		var uriOK, scopeOK bool
		redirectURIs, uriOK = jsonStringOrList(body.RedirectURIs, "\n")
		scopes, scopeOK = jsonStringOrList(body.Scopes, " ")
		if !uriOK || !scopeOK {
			writeAPIError(w, http.StatusUnprocessableEntity, "redirect_uris and scopes must be a string or an array of strings")
			return
		}
		clientName, website = body.ClientName, body.Website
	} else {
		if err := parseOAuthForm(r); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid or oversized application registration")
			return
		}
		clientName = r.FormValue("client_name")
		redirectURIs = formRedirectURIs(r)
		scopes = r.FormValue("scopes")
		website = r.FormValue("website")
	}
	if len(clientName) > 200 || len(website) > 2048 || len(redirectURIs) > 8192 || len(scopes) > 1024 {
		writeAPIError(w, http.StatusUnprocessableEntity, "application registration fields are too long")
		return
	}

	if clientName == "" {
		writeAPIError(w, http.StatusUnprocessableEntity, "client_name is required")
		return
	}
	if strings.TrimSpace(redirectURIs) == "" {
		redirectURIs = oobRedirectURI
	}
	redirectURIs = strings.Join(registeredRedirectURIs(redirectURIs), "\n")
	if !validRegisteredRedirectURIs(redirectURIs) {
		writeAPIError(w, http.StatusUnprocessableEntity, "redirect_uris contains an unsafe or invalid callback")
		return
	}
	if strings.TrimSpace(scopes) == "" {
		scopes = defaultAppScopes
	}

	app := &models.OAuthApplication{
		Name:         clientName,
		Website:      website,
		RedirectURIs: redirectURIs,
		ClientID:     generateSecret(32),
		ClientSecret: generateSecret(64),
		Scopes:       scopes,
	}

	if err := h.store.CreateOAuthApp(r.Context(), app); err != nil {
		slog.Error("failed to create oauth app", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error creating application")
		return
	}

	slog.Info("registered oauth app", "name", clientName, "id", app.ID)

	uris := registeredRedirectURIs(app.RedirectURIs)
	writeAPJSON(w, http.StatusOK, map[string]interface{}{
		"id":            app.ID,
		"name":          app.Name,
		"website":       app.Website,
		"scopes":        strings.Fields(app.Scopes),
		"redirect_uri":  app.RedirectURIs, // Mastodon's newline-separated string
		"redirect_uris": uris,             // Mastodon 4.3+ array form
		"client_id":     app.ClientID,
		"client_secret": app.ClientSecret,
		"vapid_key":     h.vapidPublic, // Web Push VAPID public key
	})
}

// AppVerifyCredentials handles GET /api/v1/apps/verify_credentials, which
// clients call right after registering to confirm the server understood them.
// Both user tokens and client-credentials app tokens are accepted; neither
// discloses the client secret.
func (h *Handler) AppVerifyCredentials(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	if token == "" {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_token", "missing bearer token")
		return
	}
	stored, err := h.store.GetOAuthTokenByAccess(r.Context(), token)
	if err != nil || stored.AuthorizationCode {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_token", "the access token is invalid")
		return
	}
	app, err := h.store.GetOAuthApp(r.Context(), stored.ApplicationID)
	if err != nil {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_token", "the access token is invalid")
		return
	}
	writeAPJSON(w, http.StatusOK, map[string]interface{}{
		"id":            app.ID,
		"name":          app.Name,
		"website":       app.Website,
		"scopes":        strings.Fields(app.Scopes),
		"redirect_uri":  app.RedirectURIs,
		"redirect_uris": registeredRedirectURIs(app.RedirectURIs),
		"vapid_key":     h.vapidPublic,
	})
}

// bearerToken extracts the credential from an Authorization header, accepting
// the "token" prefix some client libraries still send.
func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	for _, prefix := range []string{"Bearer ", "bearer ", "token ", "Token "} {
		if strings.HasPrefix(header, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(header, prefix))
		}
	}
	return ""
}

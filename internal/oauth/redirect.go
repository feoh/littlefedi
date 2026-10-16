package oauth

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// oobRedirectURI is the out-of-band callback: the code is shown to the user
// instead of being handed to a running application.
const oobRedirectURI = "urn:ietf:wg:oauth:2.0:oob"

// registeredRedirectURIs splits a stored registration into its entries.
func registeredRedirectURIs(registered string) []string {
	var uris []string
	for _, raw := range strings.Split(registered, "\n") {
		if raw = strings.TrimSpace(raw); raw != "" {
			uris = append(uris, raw)
		}
	}
	return uris
}

// resolveRedirectURI picks the callback for an authorization request. An
// omitted redirect_uri falls back to the sole registered callback, which is
// what Mastodon does and what several clients rely on; anything else must match
// a registered entry byte for byte.
func resolveRedirectURI(registered, requested string) (string, bool) {
	uris := registeredRedirectURIs(registered)
	if requested == "" {
		if len(uris) == 1 {
			return uris[0], true
		}
		return "", false
	}
	if !validOAuthRedirectURI(requested) {
		return "", false
	}
	for _, uri := range uris {
		if uri == requested {
			return requested, true
		}
	}
	return "", false
}

func validRegisteredRedirectURIs(registeredURIs string) bool {
	uris := registeredRedirectURIs(registeredURIs)
	if len(uris) == 0 || len(uris) > 20 {
		return false
	}
	for _, uri := range uris {
		if !validOAuthRedirectURI(uri) {
			return false
		}
	}
	return true
}

// validOAuthRedirectURI keeps authorization codes away from executable and
// clear-text web callbacks. Native application schemes remain supported; HTTP
// is accepted only for the loopback redirect pattern from RFC 8252.
func validOAuthRedirectURI(raw string) bool {
	if raw == oobRedirectURI {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "https":
		return u.Hostname() != ""
	case "http":
		host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
		if host == "localhost" {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	case "javascript", "data", "vbscript", "file", "about", "blob", "urn":
		return false
	default:
		// A private-use scheme (RFC 8252 §7.1) is enough on its own: Ice Cubes
		// registers the bare "icecubesapp://", with no authority and no path.
		return true
	}
}

// buildRedirectURL appends query parameters to a callback URI.
//
// The URI is spliced on its first "?" rather than parsed: url.Parse lower-cases
// the scheme and re-normalizes authority-less custom-scheme URLs, which would
// turn a registered callback such as com.tapbots.Ivory.25200:/request_token/x
// into something the client no longer recognizes.
func buildRedirectURL(baseURI string, params map[string]string) string {
	prefix, rawQuery := baseURI, ""
	if i := strings.IndexByte(baseURI, '?'); i >= 0 {
		prefix, rawQuery = baseURI[:i], baseURI[i+1:]
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return ""
	}
	for k, v := range params {
		if v == "" {
			continue
		}
		query.Set(k, v)
	}
	return prefix + "?" + query.Encode()
}

// redirectToClientError reports an authorization failure the way RFC 6749
// §4.1.2.1 requires: back at the client's callback, so the app's web
// authentication session closes instead of hanging on an error page it cannot
// read. Only reachable once the callback has been matched against the
// registration, so this is never an open redirect.
func redirectToClientError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, description string) {
	if redirectURI == "" || redirectURI == oobRedirectURI {
		writeOAuthError(w, http.StatusBadRequest, code, description)
		return
	}
	target := buildRedirectURL(redirectURI, map[string]string{
		"error":             code,
		"error_description": description,
		"state":             state,
	})
	if target == "" {
		writeOAuthError(w, http.StatusBadRequest, code, description)
		return
	}
	redirectToCallback(w, r, target)
}

// redirectToCallback hands control back to the client application.
//
// A form submitted through htmx boosting travels as an XHR, and
// XMLHttpRequest cannot follow a redirect into a client's private-use scheme:
// the browser reports a network error and the app's authentication session
// waits forever. The consent form opts out of boosting
// (templates/oauth_authorize.html), and answering HX-Request with HX-Redirect
// keeps the handoff working even if some other path arrives boosted.
func redirectToCallback(w http.ResponseWriter, r *http.Request, target string) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

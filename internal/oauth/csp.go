package oauth

import (
	"net/http"
	"strings"
)

// AllowCallbackInCSP drops the form-action directive from an already-set
// Content-Security-Policy so the consent form can hand control back to the
// client's callback.
//
// WebKit enforces form-action across the whole redirect chain of a form
// submission, and the response here is a 302 into the client's own scheme.
// Naming that scheme as an extra source is not enough in practice: clients
// that run the flow inside ASWebAuthenticationSession (Tusker, Mona) still
// never see their callback, while the same authorization completes as soon as
// the page is opened in Safari. Mastodon turns the directive off outright on
// this controller (p.form_action(false) in OAuth::AuthorizationsController),
// and every one of these clients is built and tested against that behaviour.
//
// Nothing is given up here: the page carries a single form, and it posts to
// this same endpoint.
//
// Exported so the HTTP layer that owns the policy can assert the two agree.
func AllowCallbackInCSP(w http.ResponseWriter, redirectURI string) {
	if redirectURI == "" || redirectURI == oobRedirectURI {
		return
	}
	policy := w.Header().Get("Content-Security-Policy")
	if policy == "" {
		return
	}
	if relaxed, ok := removeFormAction(policy); ok {
		w.Header().Set("Content-Security-Policy", relaxed)
	}
}

// removeFormAction strips the form-action directive from a policy, leaving the
// surrounding directives and separators alone. It reports false when the policy
// carries no form-action directive, so a policy change elsewhere shows up as a
// failing test rather than as a silent no-op.
func removeFormAction(policy string) (string, bool) {
	directives := strings.Split(policy, ";")
	kept := make([]string, 0, len(directives))
	found := false
	for _, directive := range directives {
		fields := strings.Fields(directive)
		if len(fields) > 0 && strings.EqualFold(fields[0], "form-action") {
			found = true
			continue
		}
		kept = append(kept, directive)
	}
	if !found {
		return policy, false
	}
	return strings.Join(kept, ";"), true
}

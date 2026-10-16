package webui

import (
	"context"
	"html/template"
	"net/http"
	"strings"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/content"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/instancemeta"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/version"
)

// instanceMeta resolves the instance profile: the admin-saved values where
// they exist, the configured ones otherwise.
func (h *Handler) instanceMeta(ctx context.Context) instancemeta.Meta {
	defaults := instancemeta.Meta{}
	if h.cfg != nil {
		defaults = instancemeta.Meta{
			ShortDescription:    h.cfg.Instance.ShortDescription,
			Description:         h.cfg.Instance.Description,
			ExtendedDescription: h.cfg.Instance.ExtendedDescription,
			TermsOfService:      h.cfg.Instance.TermsOfService,
			ContactEmail:        h.cfg.Instance.ContactEmail,
			Rules:               h.cfg.Instance.Rules,
		}
	}
	return instancemeta.Load(ctx, h.store, defaults)
}

// contactAccount returns the account to show as the moderation contact: the
// configured contact_username, else the first administrator. Nil when the
// instance has neither.
func (h *Handler) contactAccount(ctx context.Context) *models.Account {
	if h.cfg != nil && strings.TrimSpace(h.cfg.ContactUsername) != "" {
		if account, err := h.store.GetAccountByUsernameDomain(ctx, strings.TrimSpace(h.cfg.ContactUsername), nil); err == nil {
			return account
		}
	}
	if admins, err := h.store.GetAdmins(ctx); err == nil && len(admins) > 0 {
		return admins[0]
	}
	return nil
}

// renderInstanceText renders admin-authored Markdown to sanitized HTML. It is
// sanitized despite the author being an administrator so a single compromised
// admin session cannot plant a script into a page every visitor loads.
func renderInstanceText(src string) template.HTML {
	if strings.TrimSpace(src) == "" {
		return ""
	}
	return content.SanitizeHTML(content.MarkdownToHTML(src))
}

// AboutPage renders the public "about this server" page: what the instance is,
// the rules it enforces, who to contact, and how big it is. It is deliberately
// reachable without an account - it is the page someone reads while deciding
// whether to make one.
func (h *Handler) AboutPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta := h.instanceMeta(ctx)

	userCount, _ := h.store.CountLocalAccounts(ctx)
	// Two separate numbers: what this server's own people wrote, and how much
	// it has cached from the rest of the network. Only the first describes the
	// instance someone is deciding whether to join.
	statusCount, _ := h.store.CountLocalStatuses(ctx)
	knownStatusCount, _ := h.store.CountStatuses(ctx)
	domainCount, _ := h.store.CountDomains(ctx)

	data := map[string]interface{}{
		"InstanceName":        h.instanceName(),
		"Domain":              h.domain,
		"ShortDescription":    meta.ShortDescription,
		"Description":         meta.Description,
		"ExtendedDescription": renderInstanceText(meta.ExtendedDescription),
		"Rules":               meta.Rules,
		"ContactEmail":        meta.ContactEmail,
		"ContactAccount":      h.contactAccount(ctx),
		"HasTerms":            strings.TrimSpace(meta.TermsOfService) != "",
		"UserCount":           userCount,
		"StatusCount":         statusCount,
		"KnownStatusCount":    knownStatusCount,
		"DomainCount":         domainCount,
		"Version":             version.Version,
	}
	if h.cfg != nil {
		data["Registrations"] = h.cfg.Auth.Registrations
		data["ApprovalRequired"] = h.cfg.Auth.RequireApproval
		data["InviteRequired"] = h.cfg.Auth.InviteRequired
	}

	h.renderPage(w, r, h.pageTitle(r, "about.title"), data, "about.html")
}

// TermsPage renders the instance terms of service, and 404s when the operator
// has published none rather than showing an empty legal page.
func (h *Handler) TermsPage(w http.ResponseWriter, r *http.Request) {
	meta := h.instanceMeta(r.Context())
	if strings.TrimSpace(meta.TermsOfService) == "" {
		http.NotFound(w, r)
		return
	}
	h.renderPage(w, r, h.pageTitle(r, "about.terms_title"), map[string]interface{}{
		"InstanceName": h.instanceName(),
		"Terms":        renderInstanceText(meta.TermsOfService),
		"UpdatedAt":    meta.UpdatedAt,
	}, "terms.html")
}

// Package instancemeta resolves the instance profile (the description text,
// rules, and contact address a server publishes about itself) from the two
// places it can come from: the [instance] config section, and the admin-edited
// singleton row that overrides it.
//
// It lives in its own package because the Mastodon API, the web UI, and
// NodeInfo all answer with the same values and must not disagree.
package instancemeta

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// Defaults used when neither the database nor the config says anything, so the
// instance APIs never advertise an empty description.
const (
	DefaultShortDescription = "A minimalist ActivityPub server"
	DefaultDescription      = "littleFedi is a single-binary ActivityPub server that federates with the wider Fediverse."
)

// Meta is one resolved instance profile.
type Meta struct {
	ShortDescription string
	Description      string
	// ExtendedDescription and TermsOfService are Markdown authored by an
	// administrator; render them through content.MarkdownToHTML.
	ExtendedDescription string
	TermsOfService      string
	ContactEmail        string
	Rules               []string
	// UpdatedAt is when an administrator last saved the profile; zero while it
	// still comes entirely from the config file.
	UpdatedAt time.Time
}

// Store is the slice of storage.Storage this package needs.
type Store interface {
	GetInstanceMeta(ctx context.Context) (*models.InstanceMeta, error)
}

// Load resolves the instance profile, with each stored field overriding the
// config fallback in defaults while it is non-empty.
//
// Rules are the exception: once the row exists its rules replace the
// configured ones outright, empty included, because "no rules" is a choice an
// administrator has to be able to make from the console.
//
// A database error degrades to the config values rather than failing the
// request - a description is not worth a 500.
func Load(ctx context.Context, store Store, defaults Meta) Meta {
	meta := defaults
	if meta.ShortDescription == "" {
		meta.ShortDescription = DefaultShortDescription
	}
	if meta.Description == "" {
		meta.Description = DefaultDescription
	}
	if store == nil {
		return meta
	}
	stored, err := store.GetInstanceMeta(ctx)
	if err != nil {
		slog.Error("instancemeta: load failed, using configured values", "error", err)
		return meta
	}
	if stored == nil {
		return meta
	}
	if v := strings.TrimSpace(stored.ShortDescription); v != "" {
		meta.ShortDescription = v
	}
	if v := strings.TrimSpace(stored.Description); v != "" {
		meta.Description = v
	}
	if v := strings.TrimSpace(stored.ExtendedDescription); v != "" {
		meta.ExtendedDescription = v
	}
	if v := strings.TrimSpace(stored.TermsOfService); v != "" {
		meta.TermsOfService = v
	}
	if v := strings.TrimSpace(stored.ContactEmail); v != "" {
		meta.ContactEmail = v
	}
	meta.Rules = SplitRules(stored.Rules)
	meta.UpdatedAt = stored.UpdatedAt
	return meta
}

// SplitRules parses the stored one-rule-per-line form, dropping blank lines.
func SplitRules(stored string) []string {
	lines := strings.Split(stored, "\n")
	rules := make([]string, 0, len(lines))
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			rules = append(rules, line)
		}
	}
	return rules
}

// JoinRules renders rules back into the stored form.
func JoinRules(rules []string) string {
	cleaned := make([]string, 0, len(rules))
	for _, rule := range rules {
		if rule = strings.TrimSpace(rule); rule != "" {
			cleaned = append(cleaned, rule)
		}
	}
	return strings.Join(cleaned, "\n")
}

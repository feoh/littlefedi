// Package blogtheme is the catalogue of skins for the generated static blog.
//
// It carries no build tag on purpose. The generator lives behind "-tags blog",
// but the settings picker has to compile in every build, so the list of themes
// and their stylesheets live here where both sides can reach them. The cost is
// a few kilobytes of embedded CSS in a binary built without the blog.
//
// A theme is CSS only: it layers over the generator's own style.css and may
// never assume markup the generator does not already emit. That is the same
// rule the web UI themes follow, and it is what keeps a theme from breaking a
// page it was never opened on.
package blogtheme

import (
	"embed"
	"fmt"
)

//go:embed css/*.css
var themeFS embed.FS

// Theme identifiers. Persisted on accounts, so they are part of the on-disk
// format: rename one and those accounts fall back to the default look.
const (
	// Default is the stock blog stylesheet, served by style.css alone.
	Default = ""
	// Marginalia is a long-form reading page: serif text at a narrow measure,
	// with each post's date and tags in a margin column.
	Marginalia = "marginalia"
	// Ledger is for technical notes: monospace metadata on a strict grid,
	// code blocks that break out of the reading measure.
	Ledger = "ledger"
	// Kiosk is for short posts and link blogs: a full-width colour band,
	// heavy titles, tags as pills.
	Kiosk = "kiosk"
	// Dusk is a dark-first personal journal: no rules anywhere, wide leading,
	// one amber mark per post.
	Dusk = "dusk"
	// Press is the reading half of the web UI theme of the same name: two flat
	// inks, heavy titles, a colophon line under each one.
	Press = "press"
)

// Theme describes one selectable blog skin.
type Theme struct {
	// ID is the stable identifier stored on the account.
	ID string
	// Stylesheet is a file under css/, appended after the generator's own
	// style.css. Empty for the default theme, which that stylesheet already is.
	Stylesheet string
}

// NameKey and DescriptionKey are the translation keys for the theme picker.
// The default theme has no ID, so it gets a name of its own.
func (t Theme) NameKey() string {
	if t.ID == Default {
		return "blogtheme.default.name"
	}
	return "blogtheme." + t.ID + ".name"
}

func (t Theme) DescriptionKey() string {
	if t.ID == Default {
		return "blogtheme.default.description"
	}
	return "blogtheme." + t.ID + ".description"
}

// themes lists every wired-in blog theme, in picker display order.
var themes = []Theme{
	{ID: Default, Stylesheet: ""},
	{ID: Marginalia, Stylesheet: "css/marginalia.css"},
	{ID: Ledger, Stylesheet: "css/ledger.css"},
	{ID: Kiosk, Stylesheet: "css/kiosk.css"},
	{ID: Dusk, Stylesheet: "css/dusk.css"},
	{ID: Press, Stylesheet: "css/press.css"},
}

// Themes returns every wired-in blog theme, in display order.
func Themes() []Theme {
	out := make([]Theme, len(themes))
	copy(out, themes)
	return out
}

// IsSupported reports whether id names a wired-in blog theme. The empty id
// (the default look) counts.
func IsSupported(id string) bool {
	for _, t := range themes {
		if t.ID == id {
			return true
		}
	}
	return false
}

// CSS returns the stylesheet layer for the named theme. An unknown id, like
// one left behind by a removed theme, yields no layer rather than an error:
// the site still builds and looks like the default.
func CSS(id string) ([]byte, error) {
	for _, t := range themes {
		if t.ID != id || t.Stylesheet == "" {
			continue
		}
		data, err := themeFS.ReadFile(t.Stylesheet)
		if err != nil {
			return nil, fmt.Errorf("blogtheme: read %s: %w", t.Stylesheet, err)
		}
		return data, nil
	}
	return nil, nil
}

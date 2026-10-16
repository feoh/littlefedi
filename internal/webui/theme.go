package webui

// Themes are CSS-only skins: a theme may change how the UI looks, never what
// markup is rendered. Rendered status cards are cached in memory keyed on
// viewer + locale + status + presentation hash (see statusCardKey), and adding
// a theme axis to that key would multiply cache pressure on the low-power
// hardware this project targets. So where two themes need different glyphs the
// markup renders both and CSS hides one, rather than branching in a template.
//
// Themes come in two sizes, and picking the smaller one is usually right:
//
//   - A palette theme redefines tokens, plus at most a handful of selectors
//     that follow from the palette (link underline, avatar radius, focus ring).
//     It inherits littleFedi's layout wholesale, so it cannot break a page it
//     was never opened on: 60-120 lines, and a smoke render is enough.
//   - A layout theme reflows the shell. It is 200-450 lines and puts every
//     template in scope for manual verification. Roost, Herd and the four
//     desktop themes are layout themes; the desktop ones are the largest
//     because they turn the header into a side panel and light up the rail.
//
// Adding a theme is one entry below, one static/theme-*.css file and two
// translation keys per locale. Nothing else: the settings picker ranges over
// Themes(), and the service worker derives its precache list the same way.

// Theme identifiers. Persisted on accounts and in the theme cookie, so they
// are part of the on-disk format: rename one and existing accounts fall back
// to the instance default.
const (
	// ThemeDefault is littleFedi's own look, served by app.css alone.
	ThemeDefault = "default"
	// ThemeRoost is a familiar timeline for people arriving from classic
	// microblogging apps: left navigation rail, ruled feed, blue accent.
	ThemeRoost = "roost"
	// ThemeHerd is the dense panelled column people arriving from elsewhere
	// on the fediverse expect: one surface on a contrasting ground.
	ThemeHerd = "herd"
	// ThemeConsole is monospace and flat, for self-hosters.
	ThemeConsole = "console"
	// ThemeInk is a serif reading page, for instances that publish long-form.
	ThemeInk = "ink"
	// ThemePress dresses the interface as a small two-colour press: flat inks
	// on stock paper, every panel printed a few pixels out of register.
	ThemePress = "press"
	// The four desktop themes dress littleFedi as an application window on the
	// desktop the reader already uses. They are named after the look rather
	// than after the desktop, the way Roost and Herd are.
	//
	// ThemeCupertino is the Mac idiom: vibrancy, capsule buttons, system blue.
	ThemeCupertino = "cupertino"
	// ThemeZephyr is the Plasma idiom: crisp, dense, flat, blue frames.
	ThemeZephyr = "zephyr"
	// ThemeBurrow is the GTK idiom: boxed lists, pill buttons, roomy corners.
	ThemeBurrow = "burrow"
	// ThemeWhisker is the Xfce idiom: a dark panel, raised controls, notebook
	// tabs.
	ThemeWhisker = "whisker"
	// ThemeContrast maximises legibility. It is an accessibility option
	// rather than a taste, so it sits last in the picker.
	ThemeContrast = "contrast"
)

// Appearance identifiers. "auto" follows prefers-color-scheme; "black" is a
// true-black variant of dark for OLED screens.
const (
	AppearanceAuto  = "auto"
	AppearanceLight = "light"
	AppearanceDark  = "dark"
	AppearanceBlack = "black"
)

// Theme describes one selectable skin.
type Theme struct {
	// ID is the stable identifier stored on the account.
	ID string
	// Stylesheet is a file under static/, loaded after app.css. Empty for
	// the default theme, which app.css already is.
	Stylesheet string
	// ThemeColor* fill the <meta name="theme-color"> pair, which colours
	// browser chrome and the PWA splash screen.
	ThemeColorLight string
	ThemeColorDark  string
}

// NameKey and DescriptionKey are the translation keys for the theme picker.
func (t Theme) NameKey() string        { return "theme." + t.ID + ".name" }
func (t Theme) DescriptionKey() string { return "theme." + t.ID + ".description" }

// themes lists every wired-in theme, in picker display order.
var themes = []Theme{
	{ID: ThemeDefault, Stylesheet: "", ThemeColorLight: "#0e9488", ThemeColorDark: "#151a22"},
	{ID: ThemeRoost, Stylesheet: "theme-roost.css", ThemeColorLight: "#ffffff", ThemeColorDark: "#15202b"},
	{ID: ThemeHerd, Stylesheet: "theme-herd.css", ThemeColorLight: "#d9e1e8", ThemeColorDark: "#191b22"},
	{ID: ThemeConsole, Stylesheet: "theme-console.css", ThemeColorLight: "#f7f5f0", ThemeColorDark: "#0f1115"},
	{ID: ThemeInk, Stylesheet: "theme-ink.css", ThemeColorLight: "#f4f4f2", ThemeColorDark: "#17181a"},
	{ID: ThemePress, Stylesheet: "theme-press.css", ThemeColorLight: "#e8eaee", ThemeColorDark: "#0e0f14"},
	{ID: ThemeCupertino, Stylesheet: "theme-cupertino.css", ThemeColorLight: "#ececec", ThemeColorDark: "#1e1e1e"},
	{ID: ThemeZephyr, Stylesheet: "theme-zephyr.css", ThemeColorLight: "#eff0f1", ThemeColorDark: "#232629"},
	{ID: ThemeBurrow, Stylesheet: "theme-burrow.css", ThemeColorLight: "#fafafb", ThemeColorDark: "#242424"},
	// Whisker's panel is dark in every mode, and the panel is what browser
	// chrome sits next to.
	{ID: ThemeWhisker, Stylesheet: "theme-whisker.css", ThemeColorLight: "#4b5054", ThemeColorDark: "#2b2f31"},
	{ID: ThemeContrast, Stylesheet: "theme-contrast.css", ThemeColorLight: "#ffffff", ThemeColorDark: "#000000"},
}

var appearances = []string{AppearanceAuto, AppearanceLight, AppearanceDark, AppearanceBlack}

// Themes returns every wired-in theme, in display order.
func Themes() []Theme {
	out := make([]Theme, len(themes))
	copy(out, themes)
	return out
}

// Appearances returns every appearance mode, in display order.
func Appearances() []string {
	out := make([]string, len(appearances))
	copy(out, appearances)
	return out
}

// ThemeByID returns the named theme, falling back to the default one. The
// boolean reports whether the id was recognised.
func ThemeByID(id string) (Theme, bool) {
	for _, t := range themes {
		if t.ID == id {
			return t, true
		}
	}
	return themes[0], false
}

// IsSupportedTheme reports whether id names a wired-in theme.
func IsSupportedTheme(id string) bool {
	_, ok := ThemeByID(id)
	return ok
}

// IsSupportedAppearance reports whether a names a wired-in appearance mode.
func IsSupportedAppearance(a string) bool {
	for _, known := range appearances {
		if known == a {
			return true
		}
	}
	return false
}

// Package i18n provides a small embedded message catalog for translating the
// web UI, emails, and push notifications. Flat-key JSON per locale, stdlib
// only (encoding/json + embed) - no translation library, no CLDR plural
// machinery, because every shipped locale (en/it/de/fr/es/pt) only needs a
// plain singular/other split.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed locales/*.json
var localeFS embed.FS

// DefaultLocale is the source-of-truth language: every key must exist here,
// and it's the fallback when a locale's catalog is missing a key entirely.
const DefaultLocale = "en"

// supportedLocales lists every locale wired into the catalog, in display
// order (used to build language-picker UI). Adding a fully-translated
// locale later is a data-file change only: add "xx.json" here.
var supportedLocales = []string{"en", "it", "de", "fr", "es", "pt"}

// LocaleNames gives each locale's name in its own language, for a language
// picker - these are proper nouns and are not looked up via T.
var LocaleNames = map[string]string{
	"en": "English",
	"it": "Italiano",
	"de": "Deutsch",
	"fr": "Français",
	"es": "Español",
	"pt": "Português",
}

var catalogs map[string]map[string]string

func init() {
	catalogs = make(map[string]map[string]string, len(supportedLocales))
	for _, loc := range supportedLocales {
		data, err := localeFS.ReadFile("locales/" + loc + ".json")
		if err != nil {
			panic(fmt.Sprintf("i18n: missing catalog for locale %q: %v", loc, err))
		}
		var m map[string]string
		if err := json.Unmarshal(data, &m); err != nil {
			panic(fmt.Sprintf("i18n: invalid catalog for locale %q: %v", loc, err))
		}
		catalogs[loc] = m
	}
}

// SupportedLocales returns every wired-in locale code, in display order.
func SupportedLocales() []string {
	out := make([]string, len(supportedLocales))
	copy(out, supportedLocales)
	return out
}

// IsSupported reports whether locale has a catalog (even a stub/partial one).
func IsSupported(locale string) bool {
	_, ok := catalogs[locale]
	return ok
}

// T resolves key in locale, falling back to DefaultLocale, then to the raw
// key itself (a visible, greppable missing-key marker rather than a panic
// or a blank string). args are applied via fmt.Sprintf when present.
func T(locale, key string, args ...any) string {
	if f, ok := lookup(locale, key); ok {
		return format(f, args)
	}
	if f, ok := lookup(DefaultLocale, key); ok {
		return format(f, args)
	}
	return key
}

// Raw returns key's unformatted catalog string (args placeholders like %s
// left intact), for callers that need to substitute a value client-side
// instead of via fmt.Sprintf. Falls back like T.
func Raw(locale, key string) string {
	if f, ok := lookup(locale, key); ok {
		return f
	}
	if f, ok := lookup(DefaultLocale, key); ok {
		return f
	}
	return key
}

// Tn resolves key+".one" when n == 1, otherwise key+".other". Sufficient for
// every shipped locale (all plain singular/plural, no CLDR few/many).
func Tn(locale string, n int, key string, args ...any) string {
	suffix := ".other"
	if n == 1 {
		suffix = ".one"
	}
	return T(locale, key+suffix, args...)
}

func lookup(locale, key string) (string, bool) {
	m, ok := catalogs[locale]
	if !ok {
		return "", false
	}
	f, ok := m[key]
	return f, ok
}

func format(f string, args []any) string {
	if len(args) == 0 {
		return f
	}
	return fmt.Sprintf(f, args...)
}

// MatchAcceptLanguage does a basic best-effort match against Accept-Language:
// split on commas, drop q-values, take the primary subtag before '-'/'_',
// and return the first supported locale found. Not a full RFC 4647
// implementation (no q-value ordering beyond header order) - sufficient for
// picking a reasonable default among a handful of supported locales.
func MatchAcceptLanguage(header string) string {
	for _, part := range strings.Split(header, ",") {
		tag := strings.TrimSpace(part)
		if i := strings.IndexByte(tag, ';'); i >= 0 {
			tag = tag[:i]
		}
		tag = strings.ToLower(strings.TrimSpace(tag))
		if i := strings.IndexAny(tag, "-_"); i >= 0 {
			tag = tag[:i]
		}
		if IsSupported(tag) {
			return tag
		}
	}
	return ""
}

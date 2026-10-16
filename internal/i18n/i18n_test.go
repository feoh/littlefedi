package i18n

import (
	"regexp"
	"sort"
	"testing"
)

func TestTranslateAndFallback(t *testing.T) {
	if got := T("en", "composer.post"); got != "Post" {
		t.Errorf("T(en, composer.post) = %q", got)
	}
	if got := T("it", "composer.post"); got != "Pubblica" {
		t.Errorf("T(it, composer.post) = %q", got)
	}
	if got := T("xx", "composer.post"); got != "Post" {
		t.Errorf("unknown locale should fall back to English, got %q", got)
	}
	if got := T("en", "no.such.key"); got != "no.such.key" {
		t.Errorf("missing key should return the key, got %q", got)
	}
}

func TestSupportedLocales(t *testing.T) {
	locales := SupportedLocales()
	if len(locales) == 0 || locales[0] != DefaultLocale {
		t.Fatalf("SupportedLocales = %v", locales)
	}
	for _, loc := range locales {
		if !IsSupported(loc) {
			t.Errorf("IsSupported(%q) = false", loc)
		}
		if LocaleNames[loc] == "" {
			t.Errorf("no display name for %q", loc)
		}
	}
	locales[0] = "mutated"
	if SupportedLocales()[0] != DefaultLocale {
		t.Error("SupportedLocales exposes its backing slice")
	}
}

func TestMatchAcceptLanguage(t *testing.T) {
	tests := map[string]string{
		"de-DE,de;q=0.9,en;q=0.8": "de",
		"ja, fr-CA;q=0.5":         "fr",
		"pt_BR":                   "pt",
		"ja, zh":                  "",
		"":                        "",
	}
	for header, want := range tests {
		if got := MatchAcceptLanguage(header); got != want {
			t.Errorf("MatchAcceptLanguage(%q) = %q, want %q", header, got, want)
		}
	}
}

var verbPattern = regexp.MustCompile(`%[-+# 0]*[0-9]*(?:\.[0-9]+)?[a-zA-Z%]`)

// Every catalog carries every English key with the same format verbs, so a
// translation can never drop an argument or crash fmt with a missing one.
func TestCatalogsMatchEnglish(t *testing.T) {
	english := catalogs[DefaultLocale]
	for _, loc := range SupportedLocales() {
		if loc == DefaultLocale {
			continue
		}
		catalog := catalogs[loc]
		for key, en := range english {
			translated, ok := catalog[key]
			if !ok {
				t.Errorf("%s: missing key %q", loc, key)
				continue
			}
			if a, b := verbs(en), verbs(translated); !equal(a, b) {
				t.Errorf("%s: %q has verbs %v, English has %v", loc, key, b, a)
			}
		}
		for key := range catalog {
			if _, ok := english[key]; !ok {
				t.Errorf("%s: key %q is not in the English catalog", loc, key)
			}
		}
	}
}

func verbs(s string) []string {
	found := verbPattern.FindAllString(s, -1)
	sort.Strings(found)
	return found
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTn(t *testing.T) {
	for key := range catalogs[DefaultLocale] {
		if len(key) > 4 && key[len(key)-4:] == ".one" {
			base := key[:len(key)-4]
			if _, ok := catalogs[DefaultLocale][base+".other"]; !ok {
				continue
			}
			if Tn("en", 1, base, 1) == Tn("en", 2, base, 2) {
				t.Errorf("Tn(%q) gives the same text for 1 and 2", base)
			}
			return
		}
	}
	t.Skip("no pluralized keys in the catalog")
}

package blogtheme

import (
	"testing"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/i18n"
)

func TestEveryThemeHasCSSAndTranslations(t *testing.T) {
	for _, theme := range Themes() {
		if !IsSupported(theme.ID) {
			t.Errorf("IsSupported(%q) = false", theme.ID)
		}
		css, err := CSS(theme.ID)
		if err != nil {
			t.Errorf("CSS(%q): %v", theme.ID, err)
		}
		if theme.ID == Default && css != nil {
			t.Error("the default theme should have no extra stylesheet")
		}
		if theme.ID != Default && len(css) == 0 {
			t.Errorf("theme %q has an empty stylesheet", theme.ID)
		}
		for _, key := range []string{theme.NameKey(), theme.DescriptionKey()} {
			if i18n.T("en", key) == key {
				t.Errorf("theme %q: no English text for %q", theme.ID, key)
			}
		}
	}
}

func TestUnknownTheme(t *testing.T) {
	if IsSupported("neon") {
		t.Error("IsSupported(neon) = true")
	}
	if css, err := CSS("neon"); css != nil || err != nil {
		t.Errorf("CSS(neon) = %d bytes, %v", len(css), err)
	}
}

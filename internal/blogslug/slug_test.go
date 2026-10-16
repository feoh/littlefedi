package blogslug

import (
	"testing"
	"time"
)

func TestSlugify(t *testing.T) {
	tests := map[string]string{
		"Hello, World!":       "hello-world",
		"  Café au lait  ":    "cafe-au-lait",
		"Ünïcödé   Tëxt":      "unicode-text",
		"multiple---dashes__": "multiple-dashes",
		"":                    "",
	}
	for in, want := range tests {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildDefaultFormat(t *testing.T) {
	created := time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)
	got := Build(DefaultFormat, created, "My First Post", "01JABCDEFGHIJ")
	if want := "2026/03/07/my-first-post"; got != want {
		t.Errorf("Build = %q, want %q", got, want)
	}
}

func TestBuildFallsBackToShortIDWithoutTitle(t *testing.T) {
	got := Build("{title}", time.Now(), "", "ABCDEFGHIJKL")
	if got != "abcdefgh" {
		t.Errorf("Build with no title = %q, want the short id", got)
	}
}

func TestValidate(t *testing.T) {
	for _, ok := range []string{DefaultFormat, "{shortid}", "posts/{year}/{title}", "{id}.html"} {
		if err := Validate(ok); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "/{title}", "{year}/{month}", "{nope}/{title}", "../{title}", "tags/{title}"} {
		if Validate(bad) == nil {
			t.Errorf("Validate(%q) = nil, want an error", bad)
		}
	}
}

func TestResolveFormat(t *testing.T) {
	if got := ResolveFormat("{year}/{title}", "{shortid}", true); got != "{shortid}" {
		t.Errorf("user format should win when allowed, got %q", got)
	}
	if got := ResolveFormat("{year}/{title}", "{shortid}", false); got != "{year}/{title}" {
		t.Errorf("instance format should win when users are not allowed, got %q", got)
	}
	if got := ResolveFormat("bogus", "", false); got != DefaultFormat {
		t.Errorf("invalid formats should fall back to the default, got %q", got)
	}
}

func TestCleanAndUnique(t *testing.T) {
	if got := Clean("/Tags//Hello World/../x"); got != "tags-post/hello-world/x" {
		t.Errorf("Clean = %q", got)
	}
	taken := map[string]string{"hello": "a", "hello-2": "b"}
	if got := Unique("hello", "c", taken); got != "hello-3" {
		t.Errorf("Unique = %q, want hello-3", got)
	}
	if got := Unique("hello", "a", taken); got != "hello" {
		t.Errorf("Unique for the owning id = %q, want hello", got)
	}
}

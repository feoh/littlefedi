// Package blogslug builds the URL path of a static blog post from a
// configurable template. It is free of build tags and of any
// dependency on the blog generator, so the config loader, the web UI and the
// generator can all share one definition of what a valid slug looks like.
package blogslug

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// DefaultFormat is the instance default: dated paths are self-disambiguating
// and stay readable for a blog that runs for years.
const DefaultFormat = "{year}/{month}/{day}/{title}"

// maxTitleRunes bounds the {title} segment. Long enough to stay descriptive,
// short enough that the URL survives being pasted into a chat window.
const maxTitleRunes = 60

// maxSlugLen bounds a whole user-supplied slug, template-rendered or custom.
const maxSlugLen = 200

// shortIDLen is how much of the status UUID {shortid} keeps. 8 hex chars are
// unique enough within one account's posts, and collisions are resolved by
// the generator's suffix pass anyway.
const shortIDLen = 8

// knownPlaceholders are the substitutions Build understands. Anything else
// left in braces is a typo, and Validate rejects it rather than rendering it
// literally into every permalink.
var knownPlaceholders = []string{"{year}", "{month}", "{day}", "{title}", "{id}", "{shortid}"}

// reservedSegments are the path segments the generator itself owns at the
// site root. A post slug starting with one of these would shadow the tag
// index, the archive or the feed, so Build renames it and Validate refuses a
// template that hardcodes one.
var reservedSegments = map[string]bool{
	"tags":       true,
	"archive":    true,
	"media":      true,
	"feed.xml":   true,
	"style.css":  true,
	"index.html": true,
}

// Validate reports whether format is usable as a slug template. It is called
// on the instance default at config load and on a user-supplied format before
// it is stored, so a bad template fails once at the boundary instead of
// silently producing broken permalinks for every future post.
func Validate(format string) error {
	f := strings.TrimSpace(format)
	if f == "" {
		return fmt.Errorf("slug format is empty")
	}
	if strings.HasPrefix(f, "/") || strings.HasSuffix(f, "/") {
		return fmt.Errorf("slug format must not start or end with %q", "/")
	}
	if strings.Contains(f, "//") {
		return fmt.Errorf("slug format must not contain an empty path segment")
	}
	if strings.Contains(f, "..") {
		return fmt.Errorf("slug format must not contain %q", "..")
	}

	literals := f
	anchored := false
	for _, p := range knownPlaceholders {
		if strings.Contains(literals, p) {
			if p == "{title}" || p == "{id}" || p == "{shortid}" {
				anchored = true
			}
			literals = strings.ReplaceAll(literals, p, "")
		}
	}
	if strings.ContainsAny(literals, "{}") {
		return fmt.Errorf("slug format contains an unknown placeholder (known: %s)", strings.Join(knownPlaceholders, " "))
	}
	if !anchored {
		return fmt.Errorf("slug format must contain at least one of {title}, {id} or {shortid}, otherwise every post shares the same URL")
	}
	for _, r := range literals {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			continue
		}
		if r == '-' || r == '_' || r == '.' || r == '/' {
			continue
		}
		return fmt.Errorf("slug format contains an unsupported character %q", r)
	}
	if first, _, _ := strings.Cut(f, "/"); reservedSegments[strings.ToLower(first)] {
		return fmt.Errorf("slug format must not start with the reserved path segment %q", first)
	}
	return nil
}

// ResolveFormat picks the format to use: the account's own, when the instance
// allows per-user formats and the account set a valid one, otherwise the
// instance default (itself falling back to DefaultFormat). An account format
// that no longer validates - because it was stored before a rule tightened -
// is ignored rather than allowed to break the rebuild.
func ResolveFormat(instanceFormat, accountFormat string, allowUser bool) string {
	if allowUser {
		if f := strings.TrimSpace(accountFormat); f != "" && Validate(f) == nil {
			return f
		}
	}
	if f := strings.TrimSpace(instanceFormat); f != "" && Validate(f) == nil {
		return f
	}
	return DefaultFormat
}

// Build renders format for one post. title is the post's derived title and id
// its status UUID; createdAt supplies the date placeholders in whatever zone
// the caller passes (the generator passes the same value it renders on the
// page, so the URL date and the printed date always agree).
//
// The result is a clean relative path with no leading or trailing slash. It
// is not guaranteed unique (the caller resolves collisions), but it is
// always non-empty and always safe to use as a filesystem path.
func Build(format string, createdAt time.Time, title, id string) string {
	short := id
	if len(short) > shortIDLen {
		short = short[:shortIDLen]
	}
	titleSlug := truncateSlug(Slugify(title), maxTitleRunes)
	if titleSlug == "" {
		titleSlug = Slugify(short)
	}
	if titleSlug == "" {
		titleSlug = "post"
	}

	out := strings.NewReplacer(
		"{year}", createdAt.Format("2006"),
		"{month}", createdAt.Format("01"),
		"{day}", createdAt.Format("02"),
		"{title}", titleSlug,
		"{id}", Slugify(id),
		"{shortid}", Slugify(short),
	).Replace(format)

	return Clean(out)
}

// Clean turns an arbitrary user-supplied path into a slug that is safe to use
// as both a URL path and a directory path: each segment is slugified, empty
// and dot segments disappear, and a leading reserved segment is renamed so a
// post can never shadow the generated tag/archive/feed paths.
func Clean(path string) string {
	parts := strings.Split(path, "/")
	segments := make([]string, 0, len(parts))
	for _, part := range parts {
		s := Slugify(part)
		if s == "" || s == "." || s == ".." {
			continue
		}
		segments = append(segments, s)
	}
	if len(segments) == 0 {
		return ""
	}
	if reservedSegments[segments[0]] {
		segments[0] += "-post"
	}
	out := strings.Join(segments, "/")
	if len(out) > maxSlugLen {
		out = strings.Trim(truncateSlug(out[:maxSlugLen], maxSlugLen), "-/")
	}
	return out
}

// Unique returns base, or base with a numeric suffix, so that it is not
// already taken by a different status. taken maps slug to the id of the
// status owning it; a slug already owned by id is returned unchanged, which
// makes the function idempotent across rebuilds.
func Unique(base, id string, taken map[string]string) string {
	if base == "" {
		base = Clean(id)
	}
	if base == "" {
		base = "post"
	}
	candidate := base
	for n := 2; ; n++ {
		owner, exists := taken[candidate]
		if !exists || owner == id {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", base, n)
	}
}

// Slugify reduces one path segment to lowercase letters and digits joined by
// single hyphens. Latin accents are folded to their base letter; scripts that
// have no ASCII equivalent (CJK, Cyrillic, Greek, ...) are kept as-is rather
// than deleted, since a percent-encoded but readable path beats a path made
// of nothing but an opaque id.
func Slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if folded, err := foldDiacritics(s); err == nil {
		s = folded
	}
	var b strings.Builder
	pendingDash := false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if pendingDash && b.Len() > 0 {
				b.WriteRune('-')
			}
			pendingDash = false
			b.WriteRune(r)
			continue
		}
		pendingDash = true
	}
	return b.String()
}

// foldDiacritics decomposes, drops combining marks, and recomposes, so
// "Perché" becomes "perche" instead of "perch". The transformer chain is
// stateful, so it is built per call rather than shared.
func foldDiacritics(s string) (string, error) {
	out, _, err := transform.String(transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC), s)
	if err != nil {
		return s, err
	}
	return out, nil
}

// truncateSlug cuts an already-slugified string to at most n runes, backing
// up to the previous hyphen so the last word is not left as a fragment.
func truncateSlug(s string, n int) string {
	runeCount := 0
	for i := range s {
		runeCount++
		if runeCount > n {
			cut := s[:i]
			if idx := strings.LastIndexAny(cut, "-/"); idx > 0 {
				cut = cut[:idx]
			}
			return strings.Trim(cut, "-/")
		}
	}
	return strings.Trim(s, "-/")
}

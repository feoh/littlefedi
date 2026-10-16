// Package content handles user-visible HTML boundaries.
package content

import (
	"fmt"
	"html"
	"html/template"
	"net/url"
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
)

// Status source formats. These mirror the mediaType advertised in an
// ActivityPub object's `source` (Pleroma/GoToSocial convention).
const (
	ContentTypePlain    = "text/plain"
	ContentTypeMarkdown = "text/markdown"
	ContentTypeHTML     = "text/html"
)

// NormalizeContentType maps arbitrary input to a supported source format,
// defaulting to plain text for anything unrecognized.
func NormalizeContentType(ct string) string {
	switch ct {
	case ContentTypeMarkdown:
		return ContentTypeMarkdown
	default:
		return ContentTypePlain
	}
}

var (
	urlPattern        = regexp.MustCompile(`https?://[^\s<]+`)
	allowedClassRegex = regexp.MustCompile(`^(mention|hashtag|link|h-card|u-url|invisible)([[:space:]]+(mention|hashtag|link|h-card|u-url|invisible))*$`)
	quoteInlineClass  = regexp.MustCompile(`^quote-inline$`)

	// emojiShortcodePattern matches custom-emoji shortcodes like `:blobcat:`.
	// It is permissive - only candidates that resolve to a known
	// emoji (passed in by the caller) are ever rendered, so loose matches are
	// harmless.
	emojiShortcodePattern = regexp.MustCompile(`:([a-zA-Z0-9_]+):`)

	// anchorHrefPattern captures the href attribute of an anchor so hashtag
	// links can be rewritten for local display.
	anchorHrefPattern = regexp.MustCompile(`(?i)(<a\b[^>]*?\bhref=")([^"]*)(")`)
	// tagPathPattern matches a hashtag page path, e.g. /tags/runbsd or /tag/runbsd.
	tagPathPattern = regexp.MustCompile(`(?i)^/tags?/([^/]+)/?$`)
)

// sanitizePolicy is a bluemonday policy for ActivityPub/Mastodon HTML.
// It allows a small subset of safe HTML tags and attributes.
var sanitizePolicy *bluemonday.Policy

func init() {
	p := bluemonday.NewPolicy()

	// Allow safe inline tags
	p.AllowElements("p", "br", "span", "strong", "b", "em", "i", "code", "pre", "del", "s")

	// Allow block tags
	p.AllowElements("blockquote", "ul", "ol", "li")

	// Allow the additional block structure produced by Markdown rendering.
	// Headings, horizontal rules, and GFM tables all federate fine (Mastodon
	// and GoToSocial emit and accept these).
	p.AllowElements("h1", "h2", "h3", "h4", "h5", "h6", "hr")
	p.AllowElements("table", "thead", "tbody", "tr", "th", "td")
	// Column alignment is the only table attribute Markdown emits.
	p.AllowAttrs("align").OnElements("th", "td")

	// Allow links with href, class, rel, and target - but only whitelisted classes
	p.AllowAttrs("href", "class", "rel", "target").Matching(allowedClassRegex).OnElements("a")
	p.AllowAttrs("href", "rel", "target").OnElements("a") // allow href without class too

	// Allow class on spans (for mention/hashtag styling) - only whitelisted classes
	p.AllowAttrs("class").Matching(allowedClassRegex).OnElements("span")
	// Mastodon marks the backward-compatible link inside quote posts so native
	// clients can hide it when they render the quote card.
	p.AllowAttrs("class").Matching(quoteInlineClass).OnElements("p")

	// Require rel="nofollow noopener noreferrer" on all links
	p.RequireNoFollowOnLinks(true)
	p.RequireNoReferrerOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)

	// Only allow http, https and relative URLs in href
	p.AllowRelativeURLs(true)
	p.AllowURLSchemes("http", "https")

	// Allow title and alt on any element
	p.AllowAttrs("title", "alt").Globally()

	sanitizePolicy = p
}

// PlainTextToHTML escapes plain text and adds simple links, mentions, hashtags, and line breaks.
func PlainTextToHTML(text string) string {
	escaped := html.EscapeString(strings.TrimSpace(text))
	if escaped == "" {
		return ""
	}

	escaped = urlPattern.ReplaceAllStringFunc(escaped, func(raw string) string {
		display := raw
		trailing := ""
		for len(raw) > 0 && strings.ContainsRune(".,!?:;)", rune(raw[len(raw)-1])) {
			trailing = raw[len(raw)-1:] + trailing
			raw = raw[:len(raw)-1]
		}
		if !safeURL(raw) {
			return display
		}
		return fmt.Sprintf(`<a href="%s" class="link" target="_blank" rel="noopener noreferrer">%s</a>%s`, raw, display[:len(raw)], trailing)
	})

	// Process line by line so newlines survive as <br>. Splitting the whole
	// string with strings.Fields would collapse every run of whitespace -
	// including newlines - into single spaces before we could preserve them.
	lines := strings.Split(escaped, "\n")
	for li, line := range lines {
		words := strings.Fields(line)
		for i, word := range words {
			prefix := ""
			suffix := ""
			core := word
			for strings.HasPrefix(core, "(") || strings.HasPrefix(core, "[") {
				prefix += core[:1]
				core = core[1:]
			}
			for len(core) > 0 && strings.ContainsRune(".,!?:;)]", rune(core[len(core)-1])) {
				suffix = core[len(core)-1:] + suffix
				core = core[:len(core)-1]
			}
			switch {
			case strings.HasPrefix(core, "@") && len(core) > 1:
				acct := strings.TrimPrefix(core, "@")
				words[i] = fmt.Sprintf(`%s<a href="/@%s" class="mention">@%s</a>%s`, prefix, url.PathEscape(acct), acct, suffix)
			case strings.HasPrefix(core, "#") && len(core) > 1 && core[1] != '#':
				tag := strings.TrimPrefix(core, "#")
				words[i] = fmt.Sprintf(`%s<a href="/tags/%s" class="mention hashtag" rel="tag">#<span>%s</span></a>%s`, prefix, url.PathEscape(strings.ToLower(tag)), tag, suffix)
			}
		}
		lines[li] = strings.Join(words, " ")
	}

	// A blank line is a paragraph boundary, while a single newline is a hard
	// line break. Besides being better HTML, this distinction is required by
	// Mastodon's hashtag-bar parser: it only extracts a trailing hashtag-only
	// block when that line is the final element (normally its own <p>), not when
	// it follows <br><br> inside the preceding paragraph.
	paragraphs := make([]string, 0, len(lines))
	paragraphLines := make([]string, 0, len(lines))
	flushParagraph := func() {
		if len(paragraphLines) == 0 {
			return
		}
		paragraphs = append(paragraphs, "<p>"+strings.Join(paragraphLines, "<br>")+"</p>")
		paragraphLines = paragraphLines[:0]
	}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			flushParagraph()
			continue
		}
		paragraphLines = append(paragraphLines, line)
	}
	flushParagraph()
	return strings.Join(paragraphs, "")
}

// ExtractHashtags returns the unique, lowercased hashtag names present in plain
// text, using the same token detection as PlainTextToHTML so the stored tags
// match the rendered #links.
func ExtractHashtags(text string) []string {
	seen := map[string]bool{}
	var names []string
	for _, word := range strings.Fields(text) {
		core := word
		for strings.HasPrefix(core, "(") || strings.HasPrefix(core, "[") {
			core = core[1:]
		}
		for len(core) > 0 && strings.ContainsRune(".,!?:;)]", rune(core[len(core)-1])) {
			core = core[:len(core)-1]
		}
		if !strings.HasPrefix(core, "#") || len(core) <= 1 || core[1] == '#' {
			continue
		}
		name := strings.ToLower(strings.TrimPrefix(core, "#"))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// SanitizeHTML cleans HTML content using bluemonday, a well-tested Go HTML
// sanitizer. It keeps the small ActivityPub/Mastodon HTML subset littleFedi renders.
func SanitizeHTML(input string) template.HTML {
	if strings.TrimSpace(input) == "" {
		return ""
	}

	clean := sanitizePolicy.Sanitize(input)
	return template.HTML(clean)
}

// SanitizeHTMLStr returns sanitized HTML as a plain string, intended for API
// output paths where we want to ship sanitized content to clients but don't
// want to return template.HTML (which implies trust).
func SanitizeHTMLStr(input string) string {
	if strings.TrimSpace(input) == "" {
		return ""
	}
	return sanitizePolicy.Sanitize(input)
}

// ResolveSocialLinks replaces the provisional local hrefs emitted by the text
// renderers with canonical, absolute URLs. ActivityPub content is HTML, and
// remote implementations cannot resolve a relative /@... or /tags/... URL
// against the author's instance reliably (the object ID may have any path).
func ResolveSocialLinks(input, origin string, mentions map[string]string) string {
	for acct, actorURL := range mentions {
		if acct == "" || !safeURL(actorURL) {
			continue
		}
		old := `href="/@` + url.PathEscape(acct) + `" class="mention"`
		new := `href="` + html.EscapeString(actorURL) + `" class="mention u-url"`
		input = strings.ReplaceAll(input, old, new)
	}
	origin = strings.TrimRight(origin, "/")
	if safeURL(origin) {
		input = strings.ReplaceAll(input, `href="/@`, `href="`+html.EscapeString(origin)+`/@`)
		input = strings.ReplaceAll(input, `href="/tags/`, `href="`+html.EscapeString(origin)+`/tags/`)
	}
	return input
}

// plainTextPolicy strips every tag, leaving only text nodes. Used to derive a
// short plain-text excerpt from stored content HTML.
var plainTextPolicy = bluemonday.StrictPolicy()

// blockBreakPattern matches closing block/line-break tags so they collapse to a
// single space instead of gluing adjacent words together once tags are removed.
var blockBreakPattern = regexp.MustCompile(`(?i)</(p|div|h[1-6]|li|blockquote|pre)>|<br\s*/?>`)

var whitespaceRun = regexp.MustCompile(`\s+`)

// PlainText converts stored content HTML to a single-line plain-text string:
// block boundaries become spaces, all tags are stripped, HTML entities are
// decoded, and runs of whitespace are collapsed. Intended for excerpts and
// previews, not for display of rich content.
func PlainText(input string) string {
	if strings.TrimSpace(input) == "" {
		return ""
	}
	spaced := blockBreakPattern.ReplaceAllString(input, " ")
	stripped := plainTextPolicy.Sanitize(spaced)
	stripped = html.UnescapeString(stripped)
	return strings.TrimSpace(whitespaceRun.ReplaceAllString(stripped, " "))
}

// LocalizeHashtagLinks rewrites hashtag anchor hrefs that point at a remote
// instance's tag page (e.g. https://remote.example/tags/RunBSD) to this
// instance's local /tags/<name> page, lowercasing the name. Non-hashtag links
// are left untouched. It is intended for server-side rendered display only;
// API output keeps the original remote URLs. The transform is idempotent for
// already-local links.
func LocalizeHashtagLinks(input string) string {
	if input == "" {
		return input
	}
	return anchorHrefPattern.ReplaceAllStringFunc(input, func(match string) string {
		sub := anchorHrefPattern.FindStringSubmatch(match)
		if sub == nil {
			return match
		}
		name, ok := hashtagFromHref(sub[2])
		if !ok {
			return match
		}
		return sub[1] + "/tags/" + name + sub[3]
	})
}

// LocalizeInstanceLinks keeps browser navigation on the origin that served the
// HTML, while stored/federated content retains canonical absolute URLs.
func LocalizeInstanceLinks(input, domain string) string {
	if input == "" || domain == "" {
		return input
	}
	return anchorHrefPattern.ReplaceAllStringFunc(input, func(match string) string {
		sub := anchorHrefPattern.FindStringSubmatch(match)
		if sub == nil {
			return match
		}
		u, err := url.Parse(html.UnescapeString(sub[2]))
		if err != nil || u.User != nil || !strings.EqualFold(u.Host, domain) ||
			(u.Scheme != "http" && u.Scheme != "https") {
			return match
		}
		return sub[1] + html.EscapeString(u.RequestURI()) + sub[3]
	})
}

// hashtagFromHref returns the URL-escaped, lowercased tag name when href points
// at a hashtag page path, and whether it matched.
func hashtagFromHref(href string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(html.UnescapeString(href)))
	if err != nil {
		return "", false
	}
	m := tagPathPattern.FindStringSubmatch(u.EscapedPath())
	if m == nil {
		return "", false
	}
	name := m[1]
	if unescaped, err := url.PathUnescape(name); err == nil {
		name = unescaped
	}
	name = strings.ToLower(strings.TrimPrefix(name, "#"))
	if name == "" {
		return "", false
	}
	return url.PathEscape(name), true
}

// ExtractEmojiShortcodes returns the unique custom-emoji shortcodes (without the
// surrounding colons) referenced in the given text or HTML. Callers use it to
// decide which emoji rows to look up before rendering.
func ExtractEmojiShortcodes(s string) []string {
	if !strings.Contains(s, ":") {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	for _, m := range emojiShortcodePattern.FindAllStringSubmatch(s, -1) {
		code := m[1]
		if _, ok := seen[code]; ok {
			continue
		}
		seen[code] = struct{}{}
		out = append(out, code)
	}
	return out
}

// EmojifyHTML replaces `:shortcode:` occurrences in already-sanitized HTML with
// inline <img> emoji tags, using the shortcode->imageURL map. It must run after
// sanitization: the sanitizer strips <img> from federated content
// (a privacy measure), so emoji markup we control is injected afterwards. Only
// shortcodes present in the map are replaced; the image URLs are expected to be
// proxy/cache-aware (the caller rewrites them).
func EmojifyHTML(htmlContent string, urls map[string]string) string {
	if len(urls) == 0 || !strings.Contains(htmlContent, ":") {
		return htmlContent
	}
	return emojiShortcodePattern.ReplaceAllStringFunc(htmlContent, func(match string) string {
		code := strings.Trim(match, ":")
		src, ok := urls[code]
		if !ok {
			return match
		}
		return emojiImgTag(code, src)
	})
}

// EmojifyText is like EmojifyHTML but for plain text (e.g. display names): it
// HTML-escapes the input first, then injects emoji <img> tags. The result is
// trusted HTML.
func EmojifyText(text string, urls map[string]string) string {
	escaped := html.EscapeString(text)
	if len(urls) == 0 {
		return escaped
	}
	return EmojifyHTML(escaped, urls)
}

// emojiImgTag builds the inline <img> markup for a custom emoji. The shortcode
// and src are escaped so this is safe to emit as trusted HTML.
func emojiImgTag(shortcode, src string) string {
	code := html.EscapeString(shortcode)
	return fmt.Sprintf(`<img class="emoji" src="%s" alt=":%s:" title=":%s:" draggable="false">`,
		html.EscapeString(src), code, code)
}

func safeURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return true
	default:
		return strings.HasPrefix(raw, "/")
	}
}

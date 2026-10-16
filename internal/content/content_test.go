package content

import (
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeContentType(t *testing.T) {
	if NormalizeContentType("text/markdown") != ContentTypeMarkdown {
		t.Error("markdown not kept")
	}
	for _, other := range []string{"", "text/html", "application/json"} {
		if NormalizeContentType(other) != ContentTypePlain {
			t.Errorf("NormalizeContentType(%q) should fall back to plain text", other)
		}
	}
}

func TestPlainTextToHTML(t *testing.T) {
	got := PlainTextToHTML("Hello #golang @bob@example.com\nline two https://example.com/x <b>")
	for _, want := range []string{
		`<a href="/tags/golang" class="mention hashtag" rel="tag">#<span>golang</span></a>`,
		`<a href="/@bob@example.com" class="mention">@bob@example.com</a>`,
		`<br>`,
		`<a href="https://example.com/x" class="link" target="_blank" rel="noopener noreferrer">`,
		`&lt;b&gt;`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("PlainTextToHTML output lacks %s\ngot: %s", want, got)
		}
	}
	if !strings.HasPrefix(got, "<p>") || !strings.HasSuffix(got, "</p>") {
		t.Errorf("output is not a paragraph: %s", got)
	}
}

func TestExtractHashtags(t *testing.T) {
	got := ExtractHashtags("I like #Go and #golang, #Go again #über")
	if want := []string{"go", "golang", "über"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractHashtags = %q, want %q", got, want)
	}
}

func TestSanitizeHTML(t *testing.T) {
	got := SanitizeHTMLStr(`<p onclick="x()">hi <script>alert(1)</script><a href="javascript:alert(1)">l</a><a href="https://e.com">ok</a></p>`)
	for _, banned := range []string{"<script", "onclick", "javascript:", "alert"} {
		if strings.Contains(got, banned) {
			t.Errorf("sanitized HTML still contains %q: %s", banned, got)
		}
	}
	if !strings.Contains(got, `href="https://e.com"`) || !strings.Contains(got, "noopener") {
		t.Errorf("safe link lost or missing rel: %s", got)
	}
	if string(SanitizeHTML("<b>x</b>")) != SanitizeHTMLStr("<b>x</b>") {
		t.Error("SanitizeHTML and SanitizeHTMLStr disagree")
	}
}

func TestPlainText(t *testing.T) {
	if got := PlainText("<p>Hello &amp; <b>world</b></p><p>two</p>"); got != "Hello & world two" {
		t.Errorf("PlainText = %q", got)
	}
}

func TestCustomEmoji(t *testing.T) {
	if got := ExtractEmojiShortcodes("hi :blobcat: and :party_parrot: :not valid:"); !reflect.DeepEqual(got, []string{"blobcat", "party_parrot"}) {
		t.Errorf("ExtractEmojiShortcodes = %q", got)
	}
	urls := map[string]string{"blobcat": "https://e.com/b.png"}
	for name, got := range map[string]string{
		"html": EmojifyHTML("<p>hi :blobcat: :unknown:</p>", urls),
		"text": EmojifyText("hi :blobcat: :unknown:", urls),
	} {
		if !strings.Contains(got, `<img class="emoji" src="https://e.com/b.png" alt=":blobcat:"`) {
			t.Errorf("%s: emoji not replaced: %s", name, got)
		}
		if !strings.Contains(got, ":unknown:") {
			t.Errorf("%s: unknown shortcode should stay as text: %s", name, got)
		}
	}
}

func TestMarkdownRoundTrip(t *testing.T) {
	html := MarkdownToHTML("# Title\n\n**bold** and *it* and `code`\n\n- a\n- b\n\n<script>x</script>")
	for _, want := range []string{"<h1>Title</h1>", "<strong>bold</strong>", "<em>it</em>", "<code>code</code>", "<li>a</li>"} {
		if !strings.Contains(html, want) {
			t.Errorf("MarkdownToHTML lacks %s: %s", want, html)
		}
	}
	if strings.Contains(html, "<script") {
		t.Errorf("raw HTML passed through Markdown: %s", html)
	}
	if got := HTMLToMarkdown("<p><strong>bold</strong> and <em>it</em></p><ul><li>a</li></ul>"); got != "**bold** and *it*\n\n- a" {
		t.Errorf("HTMLToMarkdown = %q", got)
	}
}

func TestValidateCustomCSS(t *testing.T) {
	css, err := ValidateCustomCSS("body { color: red; }")
	if err != nil || css != "body { color: red; }" {
		t.Errorf("ValidateCustomCSS(valid) = %q, %v", css, err)
	}
	if _, err := ValidateCustomCSS("body{}</style><script>alert(1)</script>"); err == nil {
		t.Error("CSS that breaks out of <style> was accepted")
	}
	if _, err := ValidateCustomCSS(strings.Repeat("a", MaxCustomCSSBytes+1)); err == nil {
		t.Error("oversized CSS was accepted")
	}
}

func TestLinkRewriting(t *testing.T) {
	if got := RemoveQuoteInlineFallback(`<p>text</p><p class="quote-inline">RE: <a href="https://e.com/1">https://e.com/1</a></p>`); got != "<p>text</p>" {
		t.Errorf("RemoveQuoteInlineFallback = %q", got)
	}
	if got := LocalizeHashtagLinks(`<a href="https://remote.example/tags/go" class="mention hashtag" rel="tag">#<span>go</span></a>`); !strings.Contains(got, `href="/tags/go"`) {
		t.Errorf("LocalizeHashtagLinks = %q", got)
	}
	if got := LocalizeInstanceLinks(`<a href="https://test.example/@bob">x</a>`, "test.example"); got != `<a href="/@bob">x</a>` {
		t.Errorf("LocalizeInstanceLinks = %q", got)
	}
}

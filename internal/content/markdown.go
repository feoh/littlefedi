package content

import (
	"bytes"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	ghtml "github.com/yuin/goldmark/renderer/html"
	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var markdownBlankLines = regexp.MustCompile(`\n{3,}`)

// markdownRenderer converts user-authored Markdown to HTML. Raw HTML embedded in
// the source is not rendered (html.WithUnsafe is omitted), and the
// output is sanitized again with bluemonday afterwards - defense in depth. Hard
// wraps are enabled so single newlines become <br>, matching the line-break
// behaviour users expect from social posts. GFM strikethrough, tables, and bare
// URL autolinking are enabled; everything else stays CommonMark.
var markdownRenderer = goldmark.New(
	goldmark.WithExtensions(
		extension.Strikethrough,
		extension.Table,
		extension.NewLinkify(
			extension.WithLinkifyURLRegexp(linkifyURLPattern),
			extension.WithLinkifyWWWRegexp(linkifyWWWPattern),
		),
	),
	goldmark.WithRendererOptions(
		ghtml.WithHardWraps(),
	),
)

// goldmark's default Linkify patterns only accept ASCII, so a link like
// https://example.de/Überblick.pdf stopped right before the Ü, and IDN hosts
// were not linked at all. These are the same patterns with \p{L} and \p{N}
// added to the host and path classes. goldmark still trims trailing
// punctuation and unbalanced parentheses after the match.
var (
	linkifyURLPattern = regexp.MustCompile(`^(?:http|https|ftp)://[-a-zA-Z0-9\p{L}\p{N}@:%._\+~#=]{1,256}\.[a-z]+(?::\d+)?(?:[/#?][-a-zA-Z0-9\p{L}\p{N}@:%_+.~#$!?&/=\(\);,'">\^{}\[\]` + "`" + `]*)?`)
	linkifyWWWPattern = regexp.MustCompile(`^www\.[-a-zA-Z0-9\p{L}\p{N}@:%._\+~#=]{1,256}\.[a-z]+(?:[/#?][-a-zA-Z0-9\p{L}\p{N}@:%_\+.~#!?&/=\(\);,'">\^{}\[\]` + "`" + `]*)?`)
)

// inlineTokenPattern matches @mention and #hashtag candidates inside a text run.
// A leading word/URL character disqualifies a match (handled by the caller) so
// "foo@bar" emails and "/path#frag" fragments are not turned into mentions/tags.
var inlineTokenPattern = regexp.MustCompile(`[@#][A-Za-z0-9_]+(?:@[A-Za-z0-9.\-]+)?`)

// MarkdownToHTML renders Markdown source to sanitized, linkified HTML suitable
// for storing in Status.ContentHTML. Empty/whitespace input yields "".
func MarkdownToHTML(src string) string {
	if strings.TrimSpace(src) == "" {
		return ""
	}
	var buf bytes.Buffer
	if err := markdownRenderer.Convert([]byte(src), &buf); err != nil {
		// Rendering Markdown effectively never fails, but if it does, fall back
		// to the plain-text path rather than dropping the post.
		return string(SanitizeHTML(PlainTextToHTML(src)))
	}
	clean := sanitizePolicy.Sanitize(buf.String())
	// Linkify after sanitization: we inject only anchors the policy already
	// permits (class="mention"/"hashtag", relative href), and this avoids a
	// second sanitize pass - mirroring how EmojifyHTML runs post-sanitize.
	return linkifyMentionsAndTags(clean)
}

// HTMLToMarkdown converts the small HTML subset produced by the visual
// composer back to Markdown. Unsupported elements are unwrapped, while
// executable and embedded content is discarded. MarkdownToHTML remains the
// final sanitizer before anything is published.
func HTMLToMarkdown(src string) string {
	if strings.TrimSpace(src) == "" {
		return ""
	}
	context := &xhtml.Node{Type: xhtml.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := xhtml.ParseFragment(strings.NewReader(src), context)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, node := range nodes {
		b.WriteString(htmlNodeMarkdown(node, false))
	}
	return strings.TrimSpace(markdownBlankLines.ReplaceAllString(strings.ReplaceAll(b.String(), "\r", ""), "\n\n"))
}

func htmlNodeMarkdown(node *xhtml.Node, pre bool) string {
	if node.Type == xhtml.TextNode {
		text := strings.ReplaceAll(node.Data, "\u00a0", " ")
		if pre {
			return text
		}
		if strings.TrimSpace(text) == "" && strings.Contains(text, "\n") {
			return ""
		}
		return escapeMarkdownText(text)
	}
	if node.Type != xhtml.ElementNode && node.Type != xhtml.DocumentNode {
		return ""
	}
	name := strings.ToLower(node.Data)
	switch name {
	case "script", "style", "template", "iframe", "object", "embed", "img", "video", "audio", "source":
		return ""
	case "br":
		return "\n"
	case "hr":
		return "\n\n---\n\n"
	case "ul":
		return "\n\n" + htmlListMarkdown(node, false, 0) + "\n\n"
	case "ol":
		return "\n\n" + htmlListMarkdown(node, true, 0) + "\n\n"
	case "table":
		return "\n\n" + htmlTableMarkdown(node) + "\n\n"
	}
	children := htmlChildrenMarkdown(node, pre || name == "pre")
	switch name {
	case "p", "div", "section", "article":
		return "\n\n" + strings.TrimSpace(children) + "\n\n"
	case "h1", "h2", "h3", "h4", "h5", "h6":
		level, _ := strconv.Atoi(name[1:])
		return "\n\n" + strings.Repeat("#", level) + " " + strings.TrimSpace(children) + "\n\n"
	case "strong", "b":
		return markdownDelimited(children, "**")
	case "em", "i":
		return markdownDelimited(children, "*")
	case "del", "s", "strike":
		return markdownDelimited(children, "~~")
	case "span":
		style := strings.ToLower(htmlAttr(node, "style"))
		if strings.Contains(style, "font-weight") && strings.Contains(style, "bold") {
			children = markdownDelimited(children, "**")
		}
		if strings.Contains(style, "font-style") && strings.Contains(style, "italic") {
			children = markdownDelimited(children, "*")
		}
		if strings.Contains(style, "text-decoration") && strings.Contains(style, "line-through") {
			children = markdownDelimited(children, "~~")
		}
		return children
	case "code":
		if node.Parent != nil && strings.EqualFold(node.Parent.Data, "pre") {
			return htmlChildrenMarkdown(node, true)
		}
		text := htmlPlainText(node)
		if text == "" {
			return ""
		}
		return markdownCodeSpan(text)
	case "pre":
		text := strings.TrimRight(htmlPlainText(node), "\n")
		fence := strings.Repeat("`", maxBacktickRun(text)+1)
		if len(fence) < 3 {
			fence = "```"
		}
		return "\n\n" + fence + "\n" + text + "\n" + fence + "\n\n"
	case "blockquote":
		text := strings.TrimSpace(children)
		if text == "" {
			return ""
		}
		return "\n\n> " + strings.ReplaceAll(text, "\n", "\n> ") + "\n\n"
	case "a":
		label := strings.TrimSpace(htmlPlainText(node))
		href, class := htmlAttr(node, "href"), htmlAttr(node, "class")
		if label == "" {
			return ""
		}
		if strings.Contains(class, "mention") || strings.Contains(class, "hashtag") || href == "" || !safeURL(href) {
			return escapeMarkdownText(label)
		}
		if label == href {
			return href
		}
		return "[" + escapeMarkdownText(label) + "](" + strings.ReplaceAll(href, ")", `\)`) + ")"
	}
	return children
}

func htmlChildrenMarkdown(node *xhtml.Node, pre bool) string {
	var b strings.Builder
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		b.WriteString(htmlNodeMarkdown(child, pre))
	}
	return b.String()
}

func htmlPlainText(node *xhtml.Node) string {
	var b strings.Builder
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.TextNode {
			b.WriteString(strings.ReplaceAll(n.Data, "\u00a0", " "))
			return
		}
		if n.Type == xhtml.ElementNode && strings.EqualFold(n.Data, "br") {
			b.WriteByte('\n')
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return b.String()
}

func htmlListMarkdown(list *xhtml.Node, ordered bool, depth int) string {
	var lines []string
	for item := list.FirstChild; item != nil; item = item.NextSibling {
		if item.Type != xhtml.ElementNode {
			continue
		}
		// Some browsers indent a list item by appending the nested list as a
		// sibling of the items instead of a child of one; treat both as nested.
		if strings.EqualFold(item.Data, "ul") || strings.EqualFold(item.Data, "ol") {
			lines = append(lines, htmlListMarkdown(item, strings.EqualFold(item.Data, "ol"), depth+1))
			continue
		}
		if !strings.EqualFold(item.Data, "li") {
			continue
		}
		var body strings.Builder
		var nested []string
		for child := item.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == xhtml.ElementNode && (strings.EqualFold(child.Data, "ul") || strings.EqualFold(child.Data, "ol")) {
				nested = append(nested, htmlListMarkdown(child, strings.EqualFold(child.Data, "ol"), depth+1))
				continue
			}
			body.WriteString(htmlNodeMarkdown(child, false))
		}
		indent := strings.Repeat("  ", depth)
		prefix := "- "
		if ordered {
			prefix = "1. "
		}
		text := strings.TrimSpace(body.String())
		continuation := "\n" + indent + strings.Repeat(" ", len(prefix))
		lines = append(lines, indent+prefix+strings.ReplaceAll(text, "\n", continuation))
		lines = append(lines, nested...)
	}
	return strings.Join(lines, "\n")
}

func htmlTableMarkdown(table *xhtml.Node) string {
	var rows [][]string
	var walk func(*xhtml.Node)
	walk = func(node *xhtml.Node) {
		if node.Type == xhtml.ElementNode && strings.EqualFold(node.Data, "tr") {
			var cells []string
			for cell := node.FirstChild; cell != nil; cell = cell.NextSibling {
				if cell.Type == xhtml.ElementNode && (strings.EqualFold(cell.Data, "th") || strings.EqualFold(cell.Data, "td")) {
					cells = append(cells, strings.ReplaceAll(strings.TrimSpace(htmlChildrenMarkdown(cell, false)), "|", `\|`))
				}
			}
			if len(cells) > 0 {
				rows = append(rows, cells)
			}
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(table)
	if len(rows) == 0 {
		return ""
	}
	columns := len(rows[0])
	line := func(cells []string) string {
		padded := make([]string, columns)
		copy(padded, cells)
		return "| " + strings.Join(padded, " | ") + " |"
	}
	out := []string{line(rows[0]), line(strings.Fields(strings.Repeat("--- ", columns)))}
	for _, row := range rows[1:] {
		out = append(out, line(row))
	}
	return strings.Join(out, "\n")
}

func htmlAttr(node *xhtml.Node, name string) string {
	for _, attr := range node.Attr {
		if strings.EqualFold(attr.Key, name) {
			return attr.Val
		}
	}
	return ""
}

func markdownDelimited(text, delimiter string) string {
	if strings.TrimSpace(text) == "" {
		return text
	}
	leading := text[:len(text)-len(strings.TrimLeft(text, " \t\n"))]
	trailing := text[len(strings.TrimRight(text, " \t\n")):]
	return leading + delimiter + strings.TrimSpace(text) + delimiter + trailing
}

func markdownCodeSpan(text string) string {
	fence := strings.Repeat("`", maxBacktickRun(text)+1)
	if strings.HasPrefix(text, "`") || strings.HasSuffix(text, "`") || strings.HasPrefix(text, " ") || strings.HasSuffix(text, " ") {
		return fence + " " + text + " " + fence
	}
	return fence + text + fence
}

func maxBacktickRun(text string) int {
	max, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			if run > max {
				max = run
			}
		} else {
			run = 0
		}
	}
	return max
}

func escapeMarkdownText(text string) string {
	r := strings.NewReplacer(`\`, `\\`, "`", `\`+"`", "*", `\*`, "_", `\_`, "[", `\[`, "]", `\]`)
	return r.Replace(text)
}

// linkifyMentionsAndTags rewrites bare @mentions and #hashtags in the text nodes
// of an HTML fragment into local links, leaving the text inside <a>, <code>, and
// <pre> untouched so existing links and code samples are preserved verbatim.
func linkifyMentionsAndTags(htmlStr string) string {
	if !strings.ContainsAny(htmlStr, "@#") {
		return htmlStr
	}
	z := xhtml.NewTokenizer(strings.NewReader(htmlStr))
	var b strings.Builder
	skip := 0
	for {
		tt := z.Next()
		if tt == xhtml.ErrorToken {
			// Normal end of input (io.EOF); on any other error we have already
			// written everything successfully tokenized.
			break
		}
		switch tt {
		case xhtml.TextToken:
			if skip > 0 {
				b.Write(z.Raw())
			} else {
				b.WriteString(linkifyText(string(z.Text())))
			}
		case xhtml.StartTagToken:
			name, _ := z.TagName()
			if isLinkifySkipTag(string(name)) {
				skip++
			}
			b.Write(z.Raw())
		case xhtml.EndTagToken:
			name, _ := z.TagName()
			if isLinkifySkipTag(string(name)) && skip > 0 {
				skip--
			}
			b.Write(z.Raw())
		default:
			b.Write(z.Raw())
		}
	}
	return b.String()
}

func isLinkifySkipTag(name string) bool {
	switch name {
	case "a", "code", "pre":
		return true
	}
	return false
}

// linkifyText escapes a plain-text run and replaces standalone @mentions and
// #hashtags with anchors, matching the markup PlainTextToHTML produces.
func linkifyText(text string) string {
	locs := inlineTokenPattern.FindAllStringIndex(text, -1)
	if locs == nil {
		return html.EscapeString(text)
	}
	var b strings.Builder
	last := 0
	for _, loc := range locs {
		start, end := loc[0], loc[1]
		// Require a boundary before the token so emails (foo@bar) and URL
		// fragments (/path#x) are not mistaken for mentions/hashtags. Tokens
		// that fail the check are left for the trailing escape to emit as text.
		if start > 0 && !isBoundaryByte(text[start-1]) {
			continue
		}
		b.WriteString(html.EscapeString(text[last:start]))
		b.WriteString(renderInlineToken(text[start:end]))
		last = end
	}
	b.WriteString(html.EscapeString(text[last:]))
	return b.String()
}

func renderInlineToken(tok string) string {
	body := tok[1:]
	switch tok[0] {
	case '#':
		return fmt.Sprintf(`<a href="/tags/%s" class="mention hashtag" rel="tag">#<span>%s</span></a>`,
			url.PathEscape(strings.ToLower(body)), html.EscapeString(body))
	case '@':
		return fmt.Sprintf(`<a href="/@%s" class="mention">@%s</a>`,
			url.PathEscape(body), html.EscapeString(body))
	default:
		return html.EscapeString(tok)
	}
}

// isBoundaryByte reports whether b can legitimately precede a mention/hashtag.
// Word characters and the connector bytes that appear inside URLs/emails are
// rejected so we don't link substrings of larger tokens.
func isBoundaryByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return false
	case b == '_' || b == '/' || b == '@' || b == '#' || b == '.' || b == '-':
		return false
	}
	return true
}

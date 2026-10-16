package content

import (
	"bytes"
	"strings"

	xhtml "golang.org/x/net/html"
)

// RemoveQuoteInlineFallback removes Mastodon's backward-compatible quote link
// from a status that is already being rendered with a native quote card.
// Mastodon marks this redundant paragraph with class="quote-inline".
func RemoveQuoteInlineFallback(input string) string {
	if input == "" {
		return input
	}

	const wrapperID = "littlefedi-quote-fragment"
	doc, err := xhtml.Parse(strings.NewReader(`<div id="` + wrapperID + `">` + input + `</div>`))
	if err != nil {
		return input
	}

	var wrapper *xhtml.Node
	var findWrapper func(*xhtml.Node)
	findWrapper = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode {
			for _, attr := range n.Attr {
				if attr.Key == "id" && attr.Val == wrapperID {
					wrapper = n
					return
				}
			}
		}
		for child := n.FirstChild; child != nil && wrapper == nil; child = child.NextSibling {
			findWrapper(child)
		}
	}
	findWrapper(doc)
	if wrapper == nil {
		return input
	}

	removeQuoteInlineNodes(wrapper)
	removeLegacyQuoteInlineNode(wrapper)
	var out bytes.Buffer
	for child := wrapper.FirstChild; child != nil; child = child.NextSibling {
		if err := xhtml.Render(&out, child); err != nil {
			return input
		}
	}
	return out.String()
}

// removeLegacyQuoteInlineNode handles statuses stored before littleFedi kept
// class="quote-inline" during sanitization. The exact Mastodon fallback shape
// is a leading paragraph containing only "RE:" and one link.
func removeLegacyQuoteInlineNode(wrapper *xhtml.Node) {
	for child := wrapper.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == xhtml.TextNode && strings.TrimSpace(child.Data) == "" {
			continue
		}
		if isLegacyQuoteInlineNode(child) {
			wrapper.RemoveChild(child)
		}
		return
	}
}

func isLegacyQuoteInlineNode(n *xhtml.Node) bool {
	if n.Type != xhtml.ElementNode || n.Data != "p" {
		return false
	}
	seenPrefix := false
	seenLink := false
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		switch child.Type {
		case xhtml.TextNode:
			text := strings.TrimSpace(child.Data)
			if text == "" {
				continue
			}
			if !seenPrefix && text == "RE:" {
				seenPrefix = true
				continue
			}
			return false
		case xhtml.ElementNode:
			if child.Data != "a" || !seenPrefix || seenLink {
				return false
			}
			seenLink = true
		case xhtml.CommentNode:
			continue
		default:
			return false
		}
	}
	return seenPrefix && seenLink
}

func removeQuoteInlineNodes(parent *xhtml.Node) {
	for child := parent.FirstChild; child != nil; {
		next := child.NextSibling
		if hasHTMLClass(child, "quote-inline") {
			parent.RemoveChild(child)
		} else {
			removeQuoteInlineNodes(child)
		}
		child = next
	}
}

func hasHTMLClass(n *xhtml.Node, class string) bool {
	if n.Type != xhtml.ElementNode {
		return false
	}
	for _, attr := range n.Attr {
		if attr.Key == "class" {
			for _, candidate := range strings.Fields(attr.Val) {
				if candidate == class {
					return true
				}
			}
		}
	}
	return false
}

package opengraph

import (
	"net/url"
	"strings"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// FEP-8967 lets the publisher ship the link preview with the post instead of
// leaving every receiving server to scrape the page itself: a Link in the
// object's attachment array, carrying a preview object shaped after Open Graph.
//
//	{"type": "Link", "href": "https://example.org/essay",
//	 "preview": {"type": "Article", "name": "...", "summary": "...",
//	             "image": {"type": "Image", "url": "..."},
//	             "attributedTo": {"name": "Example"}}}
//
// The values come from a server we do not control, so they are treated exactly
// like scraped metadata: same length limits, same URL rules, and the image is
// left as a remote reference for the ordinary caching path to fetch through its
// own guard rather than being trusted here.

// PreviewFromLinkAttachment builds a stored card from one attachment entry.
// It reports false for anything that is not an FEP-8967 link with usable
// preview metadata, including a bare Link with no preview at all: a card with
// nothing but a URL in it is worse than no card.
func PreviewFromLinkAttachment(attachment map[string]interface{}) (*models.StatusPreview, bool) {
	if attachment == nil {
		return nil, false
	}
	if typ, _ := attachment["type"].(string); typ != "" && !strings.EqualFold(typ, "Link") {
		return nil, false
	}
	href := safeAbsoluteURL(stringValue(attachment["href"]))
	if href == "" {
		return nil, false
	}

	preview, _ := attachment["preview"].(map[string]interface{})
	if preview == nil {
		return nil, false
	}

	card := &models.StatusPreview{
		SourceURL:   href,
		URL:         href,
		Title:       clean(stringValue(preview["name"]), 300),
		Description: clean(stringValue(preview["summary"]), 500),
		ImageRemote: safeAbsoluteURL(imageURL(preview["image"])),
		SiteName:    clean(attributionName(preview["attributedTo"]), 100),
		FetchedAt:   time.Now(),
	}
	if card.Title == "" && card.Description == "" && card.ImageRemote == "" {
		return nil, false
	}
	if card.SiteName == "" {
		if u, err := url.Parse(href); err == nil {
			card.SiteName = u.Hostname()
		}
	}
	return card, true
}

// LinkAttachment renders a stored card as the FEP-8967 attachment entry that
// goes out with the post. It returns nil when there is nothing worth sending.
//
// The image is published as the original remote URL, never as the local cached
// copy: the cached path is an implementation detail of this instance, and the
// point of shipping the preview is to save the receiver a scrape, not to make
// it depend on our media store.
func LinkAttachment(preview *models.StatusPreview) map[string]interface{} {
	if preview == nil {
		return nil
	}
	href := safeAbsoluteURL(firstNonEmpty(preview.URL, preview.SourceURL))
	if href == "" {
		return nil
	}

	body := map[string]interface{}{"type": "Note"}
	if preview.Title != "" {
		body["name"] = preview.Title
		body["type"] = "Article"
	}
	if preview.Description != "" {
		body["summary"] = preview.Description
	}
	if image := safeAbsoluteURL(firstNonEmpty(preview.ImageRemote, preview.Image)); image != "" {
		body["image"] = map[string]interface{}{"type": "Image", "url": image}
	}
	if preview.SiteName != "" {
		body["attributedTo"] = map[string]interface{}{"type": "Organization", "name": preview.SiteName}
	}
	if len(body) == 1 {
		return nil
	}

	return map[string]interface{}{
		"type":    "Link",
		"href":    href,
		"preview": body,
	}
}

// stringValue accepts both a plain string and the single-element form some
// implementations produce for a property that is nominally a set.
func stringValue(value interface{}) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case []interface{}:
		for _, item := range v {
			if s := stringValue(item); s != "" {
				return s
			}
		}
	}
	return ""
}

// imageURL reads the image reference, which may be an IRI, an Image object, or
// a list of either.
func imageURL(value interface{}) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]interface{}:
		if u := stringValue(v["url"]); u != "" {
			return u
		}
		if u, ok := v["url"].(map[string]interface{}); ok {
			return stringValue(u["href"])
		}
		return stringValue(v["href"])
	case []interface{}:
		for _, item := range v {
			if u := imageURL(item); u != "" {
				return u
			}
		}
	}
	return ""
}

func attributionName(value interface{}) string {
	switch v := value.(type) {
	case map[string]interface{}:
		return stringValue(v["name"])
	case []interface{}:
		for _, item := range v {
			if name := attributionName(item); name != "" {
				return name
			}
		}
	}
	return ""
}

// safeAbsoluteURL keeps only absolute http(s) URLs. Everything else, including
// the javascript: and data: forms that would end up in an href or an img src,
// is dropped.
func safeAbsoluteURL(raw string) string {
	if raw == "" || len(raw) > 2048 {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || validateURL(parsed) != nil {
		return ""
	}
	return parsed.String()
}

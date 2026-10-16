package federation

import (
	"strings"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/opengraph"
)

// hasParseableAttachment reports whether an object carries at least one
// attachment that ParseMediaAttachment accepts. It walks the same shape as the
// attachment loop in parseRemoteStatusInternal, so callers keeping a status for
// its media get the same answer that loop will give. Acceptance does not depend
// on the owning account, hence the empty ID.
func hasParseableAttachment(object map[string]interface{}) bool {
	for _, att := range objectAttachmentEntries(object) {
		if _, ok := opengraph.PreviewFromLinkAttachment(att); ok {
			return true
		}
		if _, ok := ParseMediaAttachment(att, ""); ok {
			return true
		}
	}
	return false
}

func objectAttachmentEntries(object map[string]interface{}) []map[string]interface{} {
	switch value := object["attachment"].(type) {
	case []interface{}:
		out := make([]map[string]interface{}, 0, len(value))
		for _, item := range value {
			if attachment, ok := item.(map[string]interface{}); ok {
				out = append(out, attachment)
			}
		}
		return out
	case map[string]interface{}:
		return []map[string]interface{}{value}
	default:
		return nil
	}
}

// ParseMediaAttachment converts an ActivityPub attachment object into a local
// media record. ActivityPub producers commonly encode url as a string, a Link
// object, or an array of Link objects.
func ParseMediaAttachment(att map[string]interface{}, accountID string) (*models.MediaAttachment, bool) {
	attType, _ := att["type"].(string)
	if attType != "Document" && attType != "Image" && attType != "Video" && attType != "Audio" {
		return nil, false
	}

	mimeType, _ := att["mediaType"].(string)
	url, urlMime := attachmentURL(att["url"])
	if url == "" {
		url, urlMime = attachmentURL(att["href"])
	}
	if mimeType == "" {
		mimeType = urlMime
	}
	if url == "" {
		return nil, false
	}
	kind := mediaKind(attType, mimeType)

	previewURL, _ := attachmentURL(att["preview"])
	if previewURL == "" {
		previewURL, _ = attachmentURL(att["icon"])
	}
	if previewURL == "" && kind != "video" {
		previewURL = url
	}

	name, _ := att["name"].(string)
	if name == "" {
		name, _ = att["summary"].(string)
	}
	// Remote servers do not all cap alt text. Cut it at our own limit, as
	// Mastodon does, rather than store and render whatever arrives.
	name = strings.TrimSpace(name)
	if runes := []rune(name); len(runes) > models.MaxMediaDescriptionLength {
		name = strings.TrimSpace(string(runes[:models.MaxMediaDescriptionLength]))
	}

	return &models.MediaAttachment{
		Type:        kind,
		URL:         url,
		PreviewURL:  previewURL,
		RemoteURL:   url,
		Description: name,
		AccountID:   accountID,
		MIMEType:    mimeType,
	}, true
}

func attachmentURL(v interface{}) (string, string) {
	switch val := v.(type) {
	case string:
		return val, ""
	case map[string]interface{}:
		return linkURL(val)
	case []interface{}:
		for _, item := range val {
			url, mimeType := attachmentURL(item)
			if url != "" {
				return url, mimeType
			}
		}
	}
	return "", ""
}

func linkURL(link map[string]interface{}) (string, string) {
	mimeType, _ := link["mediaType"].(string)
	if href, _ := link["href"].(string); href != "" {
		return href, mimeType
	}
	if url, _ := link["url"].(string); url != "" {
		return url, mimeType
	}
	return "", mimeType
}

func mediaKind(attType, mimeType string) string {
	switch attType {
	case "Image":
		return "image"
	case "Video":
		return "video"
	case "Audio":
		return "audio"
	}
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return "image"
	case strings.HasPrefix(mimeType, "video/"):
		return "video"
	case strings.HasPrefix(mimeType, "audio/"):
		return "audio"
	default:
		return "unknown"
	}
}

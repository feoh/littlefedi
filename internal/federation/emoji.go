package federation

import (
	"context"
	"log/slog"
	"strings"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// emojiUpserter is the slice of storage needed to persist custom emoji rows.
type emojiUpserter interface {
	UpsertEmoji(ctx context.Context, emoji *models.CustomEmoji) error
}

// ParseEmojiTag converts an ActivityPub `Emoji` tag object into a CustomEmoji
// row for the given domain (nil = local). The url is kept as the remote origin;
// the privacy proxy and the on-disk cache rewrite it for display later. It
// returns false for non-emoji tags or malformed entries.
func ParseEmojiTag(tag map[string]interface{}, domain *string) (*models.CustomEmoji, bool) {
	if t, _ := tag["type"].(string); t != "Emoji" {
		return nil, false
	}
	name, _ := tag["name"].(string)
	shortcode := strings.Trim(strings.TrimSpace(name), ":")
	if shortcode == "" {
		return nil, false
	}

	iconURL := ""
	if icon, ok := tag["icon"].(map[string]interface{}); ok {
		iconURL, _ = attachmentURL(icon["url"])
		if iconURL == "" {
			iconURL, _ = linkURL(icon)
		}
	}
	if iconURL == "" {
		return nil, false
	}

	return &models.CustomEmoji{
		Shortcode:       shortcode,
		Domain:          domain,
		URL:             iconURL,
		StaticURL:       iconURL,
		RemoteURL:       iconURL,
		VisibleInPicker: true,
	}, true
}

// upsertEmojiTags extracts every `Emoji` entry from an ActivityPub `tag` array
// and stores it for the given domain. Failures are logged but not fatal - a
// missing emoji only means it renders as plain `:shortcode:` text.
func upsertEmojiTags(ctx context.Context, store emojiUpserter, tagValue interface{}, domain *string) {
	tagList, ok := tagValue.([]interface{})
	if !ok {
		return
	}
	for _, t := range tagList {
		tag, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		emoji, ok := ParseEmojiTag(tag, domain)
		if !ok {
			continue
		}
		if err := store.UpsertEmoji(ctx, emoji); err != nil {
			slog.Warn("federation: failed to upsert custom emoji",
				"shortcode", emoji.Shortcode, "error", err)
		}
	}
}

package models

import "strings"

// The import ID keeps separate runs distinct under the existing notification key.
func ParseFollowImportNotificationType(value string) (kind, importID string) {
	kind, importID, _ = strings.Cut(value, ":")
	if importID == "" || kind != "follow_import.complete" && kind != "follow_import.failed" {
		return "", ""
	}
	return kind, importID
}

func (a *Account) AllowsRelationshipNotification(notificationType string, blocked bool) bool {
	switch notificationType {
	case "unfollow", "block", "unblock", "follow_reject":
		return !blocked && a.NotifySensitiveRelationships
	default:
		return true
	}
}

// mastodonNotificationTypes are the notification types Mastodon defines.
// Mastodon's apps render only these; anything else shows up as an error
// row ("Unexpected Notification Type"), so the client API keeps littleFedi's
// own types away from clients that did not ask for them.
var mastodonNotificationTypes = map[string]bool{
	"mention": true, "status": true, "reblog": true, "follow": true, "follow_request": true,
	"favourite": true, "poll": true, "update": true, "admin.sign_up": true, "admin.report": true,
	"quote": true, "quoted_update": true, "severed_relationships": true, "moderation_warning": true,
	"annual_report": true,
}

// ExtensionNotificationTypes are the types littleFedi stores that Mastodon
// has no equivalent for, keyed by the name the client API uses for them.
// The follow_import ones are stored with a ":<import id>" suffix.
var ExtensionNotificationTypes = map[string]string{
	"follow_accept":          "follow_accept",
	"follow_reject":          "follow_reject",
	"unfollow":               "unfollow",
	"block":                  "block",
	"unblock":                "unblock",
	"move":                   "move",
	"pleroma:emoji_reaction": "emoji_reaction",
	"blog.rebuilt":           "blog.rebuilt",
	"follow_import.complete": "follow_import.complete",
	"follow_import.failed":   "follow_import.failed",
	"admin.account_deleted":  "admin.account_deleted",
	"admin.account_moved":    "admin.account_moved",
}

// IsMastodonNotificationType reports whether a stored notification type is
// one Mastodon clients understand.
func IsMastodonNotificationType(storedType string) bool {
	return mastodonNotificationTypes[storedType]
}

// MastodonNotificationTypes returns the Mastodon-defined type names.
func MastodonNotificationTypes() []string {
	out := make([]string, 0, len(mastodonNotificationTypes))
	for t := range mastodonNotificationTypes {
		out = append(out, t)
	}
	return out
}

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

package app

import (
	"strings"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// NotificationExcludedTypes parses the account's stored notification
// preferences (a comma-separated exclude-list) into the excludeTypes slice
// ListNotifications and CountUnreadNotificationsFiltered expect. Shared by the
// web UI and the cache warmer so both compute the same cache-key variant.
func NotificationExcludedTypes(account *models.Account) []string {
	if account == nil || strings.TrimSpace(account.NotificationExcludedTypes) == "" {
		return nil
	}
	parts := strings.Split(account.NotificationExcludedTypes, ",")
	types := make([]string, 0, len(parts))
	favouriteExcluded := false
	for _, notificationType := range parts {
		if notificationType = strings.TrimSpace(notificationType); notificationType != "" {
			types = append(types, notificationType)
			favouriteExcluded = favouriteExcluded || notificationType == "favourite"
		}
	}
	if favouriteExcluded {
		types = append(types, "emoji_reaction")
	}
	return types
}

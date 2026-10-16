package app

import "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"

// FollowListHidden applies to readers, not internal delivery recipient queries.
func FollowListHidden(account, viewer *models.Account, followers bool) bool {
	if viewer != nil && viewer.ID == account.ID {
		return false
	}
	if followers {
		return account.HideFollowers
	}
	return account.HideFollowing
}

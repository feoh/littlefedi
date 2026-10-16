package app

import "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"

// shallowReadCopy shares nested data with the cached component. Consumers must
// copy a nested value before changing it; interaction counts use WithInteractionCounts.
func shallowReadCopy(in *models.Status) *models.Status {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

// WithInteractionCounts copies only the two nodes whose counters change.
func WithInteractionCounts(status *models.Status, favourites, reblogs int) *models.Status {
	out := *status
	out.FavouritesCount, out.ReblogsCount = favourites, reblogs
	if status.BoostOf != nil {
		boost := *status.BoostOf
		boost.FavouritesCount, boost.ReblogsCount = favourites, reblogs
		out.BoostOf = &boost
	}
	return &out
}

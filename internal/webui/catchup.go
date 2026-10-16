package webui

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// Catch-up view.
//
// A bounded, explained answer to "what did I miss" that does not
// touch the chronological timelines. Every signal it ranks on is already in
// this instance's database - who among the accounts you follow boosted a post,
// how many reactions reached us, which hashtags you follow, which authors and
// threads you engaged with recently - so nothing is fetched from, or reported
// to, anywhere else. There is no learning loop: the weights below are fixed and
// shown on the page, every listed post carries the reason it was listed, the
// list ends after catchupResults entries with no pagination, and the only thing
// the feature ever writes is one timeline marker recording where the reader
// stopped.
const (
	// catchupMarker is the markers-table timeline name holding the reader's
	// "caught up" position. Reusing markers means no new table, no migration,
	// and account deletion already removes it.
	catchupMarker = "catchup"
	// catchupCandidates bounds the home_feed walk. One indexed range scan of
	// this many rows is the whole read cost of the page.
	catchupCandidates = 400
	catchupResults    = 20
	// catchupPerAuthor stops one prolific account from taking the page.
	catchupPerAuthor = 2
	// The window is "since you last caught up", clamped so that a reader who
	// refreshes twice still sees something and a reader who was away for
	// months does not get an unbounded scan.
	catchupWindowMin   = 6 * time.Hour
	catchupWindowMax   = 7 * 24 * time.Hour
	catchupWindowFirst = 24 * time.Hour
	// A quiet window widens once to this, rather than showing an empty page.
	catchupWindowWide = 48 * time.Hour
	catchupThin       = 5
	// Affinity looks at the reader's own last month of favourites, boosts and
	// replies, capped per signal.
	catchupAffinityWindow = 30 * 24 * time.Hour
	catchupAffinityLimit  = 200
	// catchupReactionsReason is the number of locally visible reactions
	// (favourites, boosts, and emoji reactions) worth naming as a reason.
	catchupReactionsReason = 3
)

// Ranking weights. Constants rather than configuration on purpose: the ranking
// has to be one formula that every reader can see in full on the page itself,
// not a per-instance dial that makes "why am I seeing this" unanswerable.
const (
	catchupWeightBooster    = 2.5 // per distinct account you follow who boosted it
	catchupWeightFavourites = 1.0 // log of favourites this instance has seen
	catchupWeightBoosts     = 0.8 // log of boosts this instance has seen
	catchupWeightReplies    = 0.6 // log of replies this instance has seen
	catchupWeightEmoji      = 1.0 // log of emoji reactions this instance has seen
	catchupWeightAuthor     = 1.2 // you interacted with this author in the last month
	catchupWeightTag        = 1.0 // carries a hashtag you follow
	catchupWeightThread     = 0.8 // continues a conversation you took part in
	catchupPenaltyReply     = 1.0 // top-level posts come first
	// catchupHalfLifeHours halves a post's score for every this many hours
	// since it reached the reader's feed.
	catchupHalfLifeHours = 12.0
)

// catchupWeights is the formula as shown to the reader, in the order it is
// explained. Built from the same constants the ranking uses, so the page and
// the code cannot drift apart.
var catchupWeights = []catchupWeight{
	{Kind: "booster", Value: catchupWeightBooster},
	{Kind: "favourites", Value: catchupWeightFavourites},
	{Kind: "boosts", Value: catchupWeightBoosts},
	{Kind: "replies", Value: catchupWeightReplies},
	{Kind: "emoji", Value: catchupWeightEmoji},
	{Kind: "author", Value: catchupWeightAuthor},
	{Kind: "tag", Value: catchupWeightTag},
	{Kind: "thread", Value: catchupWeightThread},
	{Kind: "reply_penalty", Value: -catchupPenaltyReply},
	{Kind: "half_life", Value: catchupHalfLifeHours},
}

type catchupWeight struct {
	Kind  string
	Value float64
}

// catchupReason is one displayed explanation for a ranked post. Kind selects
// the catalog message; Count and Tag fill its placeholders.
type catchupReason struct {
	Kind  string
	Count int
	Tag   string
}

// catchupCandidate is the scoring input for one post: the feed entry that gets
// rendered (a boost wrapper, when the post reached the reader as a boost), the
// content it points at, and the local signals gathered for it.
type catchupCandidate struct {
	Entry    *models.Status
	Content  *models.Status
	Boosters int    // distinct accounts the reader follows who boosted it
	Emoji    int    // Unicode and custom emoji reactions seen by this instance
	Tag      string // a followed hashtag on the post, "" if none
	Author   bool   // reader interacted with the author in the last month
	Thread   bool   // reader took part in this conversation
}

// CatchupItem is one ranked entry as rendered.
type CatchupItem struct {
	Status  *models.Status
	Reasons []catchupReason
	Score   float64

	authorID string
}

// CatchupData is the catch-up page's template data. Items are the posts that
// arrived after the reader last marked the page as read; SeenItems are the ones
// from before that point, kept on the page under their own heading rather than
// vanishing the moment the button is pressed.
type CatchupData struct {
	Account   *models.Account
	Items     []CatchupItem
	SeenItems []CatchupItem
	Since     time.Time
	Widened   bool
	NewestID  string
	Weights   []catchupWeight
	CSRFToken string
}

func (h *Handler) catchupEnabled() bool {
	return h.cfg == nil || h.cfg.Web.CatchupEnabled()
}

// CatchupPage renders the catch-up view.
func (h *Handler) CatchupPage(w http.ResponseWriter, r *http.Request) {
	if !h.catchupEnabled() {
		http.NotFound(w, r)
		return
	}
	account := getAuthAccount(r.Context())
	if account == nil {
		w.Header().Set("Cache-Control", "private, no-store")
		http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
		return
	}
	ctx := r.Context()
	now := time.Now()

	windowStart, since := h.catchupWindow(ctx, account, now)

	// Collect once over the widest window that could be needed, then narrow in
	// memory: a quiet window widens without a second trip to the database.
	wide := windowStart
	if widest := now.Add(-catchupWindowWide); widest.Before(wide) {
		wide = widest
	}
	candidates, newestID, err := h.collectCatchup(ctx, account, wide, now)
	if err != nil {
		slog.Error("webui: catchup collect error", "error", err)
		h.renderError(w, r, "Error building your catch-up")
		return
	}

	// New is everything that arrived after the reader last pressed the button;
	// on a first visit that is the whole window. Pressing the button therefore
	// moves the divider rather than emptying the page.
	freshFrom := windowStart
	if !since.IsZero() && since.After(freshFrom) {
		freshFrom = since
	}
	items := rankCatchup(catchupSince(candidates, freshFrom), now, catchupResults, catchupPerAuthor)

	// The already-seen recap fills whatever is left of the page. When almost
	// nothing is new, it reaches further back rather than leaving the reader
	// with a blank page.
	seenFrom := windowStart
	widened := false
	if len(items) < catchupThin && wide.Before(seenFrom) {
		seenFrom = wide
		widened = true
	}
	var seen []CatchupItem
	if budget := catchupResults - len(items); budget > 0 && freshFrom.After(seenFrom) {
		seen = rankCatchup(catchupRange(candidates, seenFrom, freshFrom), now, budget, catchupPerAuthor)
	}
	items, seen, err = h.finishCatchup(ctx, account, items, seen)
	if err != nil {
		slog.Error("webui: catchup filter error", "error", err)
		h.renderError(w, r, "Error building your catch-up")
		return
	}

	h.renderPage(w, r, h.pageTitle(r, "catchup.title"), &CatchupData{
		Account:   account,
		Items:     items,
		SeenItems: seen,
		Since:     since,
		Widened:   widened,
		NewestID:  newestID,
		Weights:   catchupWeights,
		CSRFToken: getCSRFToken(r.Context()),
	}, "catchup.html")
}

// CatchupSeen records where the reader stopped, so the next visit's window
// starts here. This single marker row is everything the feature writes.
func (h *Handler) CatchupSeen(w http.ResponseWriter, r *http.Request) {
	if !h.catchupEnabled() {
		http.NotFound(w, r)
		return
	}
	account := getAuthAccount(r.Context())
	if account == nil {
		http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
		return
	}
	newest := r.FormValue("newest_id")
	if newest == "" {
		// The marker column is NOT NULL and the position we care about is the
		// row's own timestamp, so an empty feed still records "caught up now".
		newest = "0"
	}
	if _, err := h.store.SetMarker(r.Context(), account.ID, catchupMarker, newest); err != nil {
		slog.Error("webui: catchup marker error", "error", err)
		h.renderError(w, r, "Error saving your catch-up position")
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// catchupWindow returns the start of the reader's window and the time they last
// caught up (zero when they never have).
func (h *Handler) catchupWindow(ctx context.Context, account *models.Account, now time.Time) (start, since time.Time) {
	markers, err := h.store.GetMarkers(ctx, account.ID, []string{catchupMarker})
	if err != nil {
		slog.Error("webui: catchup marker read error", "error", err)
	}
	for _, m := range markers {
		if m == nil || m.Timeline != catchupMarker || m.UpdatedAt.IsZero() {
			continue
		}
		age := now.Sub(m.UpdatedAt)
		if age < catchupWindowMin {
			age = catchupWindowMin
		}
		if age > catchupWindowMax {
			age = catchupWindowMax
		}
		return now.Add(-age), m.UpdatedAt
	}
	return now.Add(-catchupWindowFirst), time.Time{}
}

// collectCatchup gathers the scoring inputs for everything the reader's home
// feed received since from. It reads the feed once and adds four bounded
// lookups (missing boost targets, followed tags, the reader's own affinity, and
// the reader's existing interactions); enrichment is left until
// the shortlist exists, so only the posts actually shown pay for it.
func (h *Handler) collectCatchup(ctx context.Context, account *models.Account, from time.Time, now time.Time) ([]catchupCandidate, string, error) {
	// The reader's own timeline preferences are honoured as-is: with boosts
	// hidden, boost wrappers never enter the feed, so the booster signal is
	// simply absent rather than smuggled back in behind the preference.
	rows, err := h.store.GetHomeTimeline(ctx, account.ID, nil, nil, nil, catchupCandidates, account.HideReplies, account.HideBoosts)
	if err != nil {
		return nil, "", err
	}
	newestID := ""
	for _, row := range rows {
		if row != nil {
			newestID = row.ID
			break
		}
	}

	type group struct {
		entry    *models.Status
		boosters map[string]bool
	}
	groups := make(map[string]*group, len(rows))
	order := make([]string, 0, len(rows))
	have := make(map[string]*models.Status, len(rows))

	inFeed := make(map[string]bool, len(rows))

	for _, row := range rows {
		if row == nil {
			continue
		}
		have[row.ID] = row
		inFeed[row.ID] = true
		if row.CreatedAt.Before(from) {
			continue
		}
		contentID := row.ID
		boost := row.BoostOfID != nil && *row.BoostOfID != ""
		if boost {
			contentID = *row.BoostOfID
		}
		g := groups[contentID]
		if g == nil {
			// Rows arrive newest first, so the first one seen is the entry to
			// render for this post.
			g = &group{entry: row, boosters: map[string]bool{}}
			groups[contentID] = g
			order = append(order, contentID)
		}
		if boost && row.AccountID != account.ID {
			g.boosters[row.AccountID] = true
		}
	}
	if len(order) == 0 {
		return nil, newestID, nil
	}
	emojiCounts, err := h.store.CountEmojiReactions(ctx, order)
	if err != nil {
		return nil, newestID, err
	}

	// Boost targets whose author the reader does not follow are not in the
	// feed; fetch just those.
	var missing []string
	for _, id := range order {
		if have[id] == nil {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		fetched, err := h.store.GetStatusesByIDs(ctx, missing)
		if err != nil {
			return nil, newestID, err
		}
		for id, status := range fetched {
			have[id] = status
		}
	}

	followedTags := map[string]bool{}
	if ids, err := h.store.GetFollowedTagIDs(ctx, account.ID); err != nil {
		slog.Error("webui: catchup followed tags error", "error", err)
	} else {
		for _, id := range ids {
			followedTags[id] = true
		}
	}
	tagsByStatus := map[string][]*models.Tag{}
	if len(followedTags) > 0 {
		if tags, err := h.store.GetTagsForStatuses(ctx, order); err != nil {
			slog.Error("webui: catchup tags error", "error", err)
		} else {
			tagsByStatus = tags
		}
	}

	affinityAuthors := map[string]bool{}
	affinityThreads := map[string]bool{}
	if affinity, err := h.store.GetCatchupAffinity(ctx, account.ID, now.Add(-catchupAffinityWindow), catchupAffinityLimit); err != nil {
		slog.Error("webui: catchup affinity error", "error", err)
	} else if affinity != nil {
		for _, id := range affinity.AuthorIDs {
			affinityAuthors[id] = true
		}
		for _, id := range affinity.ThreadIDs {
			affinityThreads[id] = true
		}
	}

	// Anything the reader already favourited, bookmarked or boosted has been
	// read: one query, before enrichment, so those posts never reach scoring.
	faved, booked, boosted, err := h.store.ViewerStatusInteractions(ctx, account.ID, order)
	if err != nil {
		return nil, newestID, err
	}

	out := make([]catchupCandidate, 0, len(order))
	for _, id := range order {
		content := have[id]
		if content == nil {
			continue
		}
		if content.AccountID == account.ID {
			continue // your own post is not something you missed
		}
		if !inFeed[id] && !catchupBoostTargetVisible(content.Visibility) {
			// A boost target the reader's own feed never carried is only
			// shown when its own visibility makes it shareable. A remote
			// server announcing a restricted object cannot use the ranking to
			// get it in front of a reader the author did not address.
			continue
		}
		if faved[id] || booked[id] || boosted[id] || affinityThreads[id] {
			continue // already read, by your own action on it
		}
		candidate := catchupCandidate{
			Entry:    groups[id].entry,
			Content:  content,
			Boosters: len(groups[id].boosters),
			Emoji:    emojiCounts[id],
			Author:   affinityAuthors[content.AccountID],
			Thread:   content.InReplyToID != nil && affinityThreads[*content.InReplyToID],
		}
		for _, tag := range tagsByStatus[id] {
			if tag != nil && followedTags[tag.ID] {
				candidate.Tag = tag.Name
				break
			}
		}
		out = append(out, candidate)
	}
	return out, newestID, nil
}

// catchupBoostTargetVisible reports whether a boosted post's own visibility
// allows it to be shown to a reader who does not follow its author.
func catchupBoostTargetVisible(v models.Visibility) bool {
	switch v {
	case models.VisibilityPublic, models.VisibilityUnlisted, models.VisibilityLocal, models.VisibilityLocalUnlisted:
		return true
	default:
		return false
	}
}

// finishCatchup enriches and filters both shortlists in one pass, then trims
// them to a shared page budget with the new posts served first. Filters and
// moderation run here, at read time, exactly as they do for every other
// timeline - a ranking is never allowed to become a way around them.
func (h *Handler) finishCatchup(ctx context.Context, account *models.Account, fresh, seen []CatchupItem) ([]CatchupItem, []CatchupItem, error) {
	if len(fresh) == 0 && len(seen) == 0 {
		return fresh, seen, nil
	}
	statuses := make([]*models.Status, 0, len(fresh)+len(seen))
	for _, item := range fresh {
		statuses = append(statuses, item.Status)
	}
	for _, item := range seen {
		statuses = append(statuses, item.Status)
	}
	// Enrichment serves cached components by replacing slice entries rather than
	// mutating in place, so the items have to pick up the enriched pointers.
	// Keeping the raw feed rows renders a card with no author and no boost
	// target - which is to say, nothing at all.
	h.app.EnrichStatuses(ctx, statuses, account)
	for i := range fresh {
		fresh[i].Status = statuses[i]
	}
	for i := range seen {
		seen[i].Status = statuses[len(fresh)+i]
	}
	kept := make(map[string]bool, len(statuses))
	filtered, err := h.app.FilterStatuses(ctx, "home", statuses, account)
	if err != nil {
		return nil, nil, err
	}
	for _, status := range filtered {
		if status != nil {
			kept[status.ID] = true
		}
	}

	budget := catchupResults
	keep := func(items []CatchupItem) []CatchupItem {
		out := make([]CatchupItem, 0, len(items))
		for _, item := range items {
			if budget == 0 {
				break
			}
			if !kept[item.Status.ID] {
				continue
			}
			out = append(out, item)
			budget--
		}
		return out
	}
	return keep(fresh), keep(seen), nil
}

// catchupSince narrows already-collected candidates to those that reached the
// feed at or after start.
func catchupSince(candidates []catchupCandidate, start time.Time) []catchupCandidate {
	out := make([]catchupCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Entry != nil && !candidate.Entry.CreatedAt.Before(start) {
			out = append(out, candidate)
		}
	}
	return out
}

// catchupRange narrows already-collected candidates to those that reached the
// feed in [start, end) - the already-seen part of the window.
func catchupRange(candidates []catchupCandidate, start, end time.Time) []catchupCandidate {
	out := make([]catchupCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Entry == nil {
			continue
		}
		if candidate.Entry.CreatedAt.Before(start) || !candidate.Entry.CreatedAt.Before(end) {
			continue
		}
		out = append(out, candidate)
	}
	return out
}

// scoreCatchup applies the formula to one candidate and returns the reasons it
// would be shown for. A candidate with no reason is not ranked at all: if the
// page cannot say why a post is there, it does not belong there.
func scoreCatchup(candidate catchupCandidate, now time.Time) (float64, []catchupReason) {
	content := candidate.Content
	if content == nil || candidate.Entry == nil {
		return 0, nil
	}

	score := 0.0
	var reasons []catchupReason

	if candidate.Boosters > 0 {
		score += catchupWeightBooster * float64(candidate.Boosters)
		reasons = append(reasons, catchupReason{Kind: "boosted", Count: candidate.Boosters})
	}

	// Interaction counts are this instance's own view of the post (the
	// activities that were actually delivered here), not a global popularity
	// figure fetched from elsewhere.
	score += catchupWeightFavourites * math.Log1p(float64(max(content.FavouritesCount, 0)))
	score += catchupWeightBoosts * math.Log1p(float64(max(content.ReblogsCount, 0)))
	score += catchupWeightReplies * math.Log1p(float64(max(content.RepliesCount, 0)))
	score += catchupWeightEmoji * math.Log1p(float64(max(candidate.Emoji, 0)))
	if reactions := max(content.FavouritesCount, 0) + max(content.ReblogsCount, 0) + max(candidate.Emoji, 0); reactions >= catchupReactionsReason {
		reasons = append(reasons, catchupReason{Kind: "reactions", Count: reactions})
	}

	if candidate.Author {
		score += catchupWeightAuthor
		reasons = append(reasons, catchupReason{Kind: "author"})
	}
	if candidate.Tag != "" {
		score += catchupWeightTag
		reasons = append(reasons, catchupReason{Kind: "tag", Tag: candidate.Tag})
	}
	if candidate.Thread {
		score += catchupWeightThread
		reasons = append(reasons, catchupReason{Kind: "thread"})
	}
	if content.InReplyToID != nil || (content.InReplyToURI != nil && *content.InReplyToURI != "") {
		score -= catchupPenaltyReply
	}
	if score <= 0 || len(reasons) == 0 {
		return 0, nil
	}

	// Freshness is measured on the feed entry, which is when the post reached
	// the reader - so a months-old post boosted this morning still reads as
	// new, because for this reader it is.
	age := now.Sub(candidate.Entry.CreatedAt).Hours()
	if age < 0 {
		age = 0
	}
	return score * math.Exp2(-age/catchupHalfLifeHours), reasons
}

// rankCatchup scores, orders and caps the candidates. Ties break on status id
// (newest first) so the same input always produces the same page.
func rankCatchup(candidates []catchupCandidate, now time.Time, limit, perAuthor int) []CatchupItem {
	items := make([]CatchupItem, 0, len(candidates))
	for _, candidate := range candidates {
		score, reasons := scoreCatchup(candidate, now)
		if score <= 0 || len(reasons) == 0 {
			continue
		}
		items = append(items, CatchupItem{
			Status:   candidate.Entry,
			Reasons:  reasons,
			Score:    score,
			authorID: candidate.Content.AccountID,
		})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Score != items[j].Score {
			return items[i].Score > items[j].Score
		}
		return items[i].Status.ID > items[j].Status.ID
	})

	perAuthorSeen := make(map[string]int, len(items))
	out := make([]CatchupItem, 0, min(limit, len(items)))
	for _, item := range items {
		if perAuthor > 0 && perAuthorSeen[item.authorID] >= perAuthor {
			continue
		}
		perAuthorSeen[item.authorID]++
		out = append(out, item)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

package federation

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/emoji"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

const (
	maxInteractionActors = 80
	maxInteractionPages  = 3
)

type StatusInteractions struct {
	Favourites      []*models.Account
	Reblogs         []*models.Account
	EmojiReactions  []models.EmojiReactionSummary
	FavouritesCount int
	ReblogsCount    int
}

type InteractionResolver struct {
	store                storage.Storage
	fetchObject          func(context.Context, string) (map[string]interface{}, error)
	fetchActor           func(context.Context, string) (*models.Account, error)
	fetchOriginAccounts  func(context.Context, string, string, string) ([]*models.Account, error)
	fetchOriginReactions func(context.Context, string, string) ([]models.EmojiReactionSummary, error)
}

func NewInteractionResolver(store storage.Storage, federator *Federator) *InteractionResolver {
	r := &InteractionResolver{store: store}
	if federator != nil {
		r.fetchObject = federator.FetchObject
		r.fetchActor = federator.FetchActorLight
		r.fetchOriginAccounts = federator.FetchStatusInteractionAccounts
		r.fetchOriginReactions = federator.FetchStatusReactions
	}
	return r
}

// Resolve returns locally observed interactions and, for a public remote
// status, best-effort results from its origin's ActivityStreams likes/shares
// collections. Origin failures never hide locally known activity.
func (r *InteractionResolver) Resolve(ctx context.Context, status *models.Status) StatusInteractions {
	result := r.ResolveLocal(ctx, status)
	if status == nil {
		return result
	}
	if status.BoostOfID != nil && *status.BoostOfID != "" {
		if original, err := r.store.GetStatus(ctx, *status.BoostOfID); err == nil {
			status = original
		}
	}
	if status.Account == nil && status.AccountID != "" {
		copy := *status
		copy.Account, _ = r.store.GetAccount(ctx, status.AccountID)
		status = &copy
	}

	if (r.fetchObject == nil && r.fetchOriginAccounts == nil && r.fetchOriginReactions == nil) || status.Account == nil || status.Account.Domain == nil ||
		(status.Visibility != models.VisibilityPublic && status.Visibility != models.VisibilityUnlisted) {
		return result
	}

	fetchCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if !r.allowedRemoteURI(fetchCtx, status.URI) {
		return result
	}
	type originResult struct {
		collection string
		accounts   []*models.Account
	}
	restResults := make(chan originResult, 2)
	restJobs := 0
	if r.fetchOriginAccounts != nil {
		for _, collection := range []string{"favourited_by", "reblogged_by"} {
			restJobs++
			go func(collection string) {
				accounts, err := r.fetchOriginAccounts(fetchCtx, status.URI, status.URL, collection)
				if err != nil {
					slog.Debug("interactions: origin REST collection unavailable", "uri", status.URI, "collection", collection, "error", err)
				}
				restResults <- originResult{collection: collection, accounts: accounts}
			}(collection)
		}
	}

	var likes, shares []string
	var reactions []models.EmojiReactionSummary
	likesTotal, sharesTotal := 0, 0
	if r.fetchObject != nil {
		doc, err := r.fetchObject(fetchCtx, status.URI)
		if err != nil {
			slog.Debug("interactions: origin status fetch failed", "uri", status.URI, "error", err)
		} else if id := activityStreamsLink(doc["id"]); id != "" && id != status.URI {
			slog.Debug("interactions: origin returned a different object", "requested", status.URI, "id", id)
		} else {
			likes, likesTotal = r.collectionActors(fetchCtx, doc["likes"], status.URI)
			shares, sharesTotal = r.collectionActors(fetchCtx, doc["shares"], status.URI)
			reactions = r.collectionReactions(fetchCtx, doc["emojiReactions"], status.URI)
		}
	}
	if len(reactions) == 0 && r.fetchOriginReactions != nil {
		remote, err := r.fetchOriginReactions(fetchCtx, status.URI, status.URL)
		if err != nil {
			slog.Debug("interactions: origin reaction collection unavailable", "uri", status.URI, "error", err)
		} else {
			reactions = remote
		}
	}
	result.Favourites = r.mergeAccounts(fetchCtx, result.Favourites, likes)
	result.Reblogs = r.mergeAccounts(fetchCtx, result.Reblogs, shares)
	result.EmojiReactions = mergeReactions(result.EmojiReactions, reactions)
	for i := 0; i < restJobs; i++ {
		select {
		case remote := <-restResults:
			if remote.collection == "favourited_by" {
				result.Favourites = mergeInteractionAccounts(result.Favourites, remote.accounts)
			} else {
				result.Reblogs = mergeInteractionAccounts(result.Reblogs, remote.accounts)
			}
		case <-fetchCtx.Done():
			i = restJobs
		}
	}
	result.FavouritesCount = max(result.FavouritesCount, max(likesTotal, len(result.Favourites)))
	result.ReblogsCount = max(result.ReblogsCount, max(sharesTotal, len(result.Reblogs)))
	// Self-heal the stored counter from the origin's live total. The Announce
	// relevance gate (inbox.go) only counts boosts from locally-followed
	// actors, so a remote post's reblogs_count drifts low over time; this is
	// the only place that reconciles it, and it's a call we're already making.
	if delta := result.ReblogsCount - status.ReblogsCount; delta > 0 {
		if err := r.store.AdjustStatusReblogsCount(ctx, status.ID, delta); err != nil {
			slog.Debug("interactions: reblogs count self-heal failed", "status", status.ID, "error", err)
		}
	}
	return result
}

// ResolveLocal returns interaction data already present in local storage. It
// never performs federation requests, so it is safe to use on page-rendering
// paths where a slow remote instance must not delay the response.
func (r *InteractionResolver) ResolveLocal(ctx context.Context, status *models.Status) StatusInteractions {
	result := StatusInteractions{}
	if status == nil {
		return result
	}
	if status.BoostOfID != nil && *status.BoostOfID != "" {
		if original, err := r.store.GetStatus(ctx, *status.BoostOfID); err == nil {
			status = original
		}
	}
	result.Favourites, _ = r.store.ListFavouriteAccounts(ctx, status.ID, maxInteractionActors)
	result.Reblogs, _ = r.store.ListReblogAccounts(ctx, status.ID, maxInteractionActors)
	groups, _ := r.store.GetEmojiReactionSummaries(ctx, []string{status.ID}, "", maxInteractionActors)
	result.EmojiReactions = groups[status.ID]
	ids := make([]string, 0)
	for _, reaction := range result.EmojiReactions {
		ids = append(ids, reaction.AccountIDs...)
	}
	accounts, _ := r.store.GetAccountsByIDs(ctx, ids)
	for i := range result.EmojiReactions {
		for _, id := range result.EmojiReactions[i].AccountIDs {
			if account := accounts[id]; account != nil {
				result.EmojiReactions[i].Accounts = append(result.EmojiReactions[i].Accounts, account)
			}
		}
	}
	result.FavouritesCount = max(status.FavouritesCount, len(result.Favourites))
	result.ReblogsCount = max(status.ReblogsCount, len(result.Reblogs))
	return result
}

func mergeReactions(known, extra []models.EmojiReactionSummary) []models.EmojiReactionSummary {
	index := make(map[string]int, len(known)+len(extra))
	result := append([]models.EmojiReactionSummary(nil), known...)
	for i := range result {
		index[result[i].Name] = i
	}
	for _, reaction := range extra {
		if i, ok := index[reaction.Name]; ok {
			result[i].Count = max(result[i].Count, reaction.Count)
			result[i].Accounts = mergeInteractionAccounts(result[i].Accounts, reaction.Accounts)
			if result[i].Emoji == nil {
				result[i].Emoji = reaction.Emoji
			}
			continue
		}
		index[reaction.Name] = len(result)
		result = append(result, reaction)
	}
	return result
}

func mergeInteractionAccounts(known, extra []*models.Account) []*models.Account {
	seen := make(map[string]bool, len(known)+len(extra))
	result := make([]*models.Account, 0, min(maxInteractionActors, len(known)+len(extra)))
	for _, list := range [][]*models.Account{known, extra} {
		for _, account := range list {
			key := interactionAccountKey(account)
			if key == "" || seen[key] || len(result) >= maxInteractionActors {
				continue
			}
			seen[key] = true
			result = append(result, account)
		}
	}
	return result
}

func interactionAccountKey(account *models.Account) string {
	if account == nil {
		return ""
	}
	if account.Username != "" {
		domain := "local"
		if account.Domain != nil && *account.Domain != "" {
			domain = strings.ToLower(*account.Domain)
		}
		return strings.ToLower(account.Username) + "@" + domain
	}
	return account.ActorURI
}

func (r *InteractionResolver) mergeAccounts(ctx context.Context, known []*models.Account, actorURIs []string) []*models.Account {
	extra := make([]*models.Account, 0, len(actorURIs))
	seenURIs := make(map[string]bool, len(actorURIs))
	for _, actorURI := range actorURIs {
		if actorURI == "" || seenURIs[actorURI] || len(extra) >= maxInteractionActors {
			continue
		}
		seenURIs[actorURI] = true
		account, err := r.store.GetAccountByURI(ctx, actorURI)
		if err != nil && r.fetchActor != nil {
			if !r.allowedRemoteURI(ctx, actorURI) {
				continue
			}
			account, err = r.fetchActor(ctx, actorURI)
		}
		if err != nil || account == nil {
			slog.Debug("interactions: actor fetch failed", "uri", actorURI, "error", err)
			continue
		}
		extra = append(extra, account)
	}
	return mergeInteractionAccounts(known, extra)
}

// collectionActors reads inline and paginated ActivityStreams collections.
// Items may be Like/Announce objects, actor objects, actor IRIs, or activity
// IRIs. Linked collection pages stay on the status's origin host.
func (r *InteractionResolver) collectionActors(ctx context.Context, value interface{}, originURI string) ([]string, int) {
	if value == nil {
		return nil, 0
	}
	var doc map[string]interface{}
	switch v := value.(type) {
	case string:
		if !sameURLHost(originURI, v) {
			return nil, 0
		}
		fetched, err := r.fetchObject(ctx, v)
		if err != nil {
			return nil, 0
		}
		doc = fetched
	case map[string]interface{}:
		doc = v
	default:
		return nil, 0
	}

	total, _ := interactionInt(doc["totalItems"])
	// Some servers embed only a collection summary ({id, type,
	// totalItems}) in the status. Dereference that id to get its items.
	if !hasInlineItems(doc) && doc["first"] == nil {
		if id := activityStreamsLink(doc["id"]); id != "" && sameURLHost(originURI, id) {
			if fetched, err := r.fetchObject(ctx, id); err == nil {
				doc = fetched
			}
		}
	}
	if !hasInlineItems(doc) {
		switch first := doc["first"].(type) {
		case map[string]interface{}:
			doc = first
		case string:
			if first != "" && sameURLHost(originURI, first) {
				if fetched, err := r.fetchObject(ctx, first); err == nil {
					doc = fetched
				}
			}
		}
	}

	actors := make([]string, 0)
	seenActors := make(map[string]bool)
	seenPages := make(map[string]bool)
	for pages := 0; doc != nil && pages <= maxInteractionPages && len(actors) < maxInteractionActors; pages++ {
		items := collectionItems(doc)
		for _, item := range items {
			actor := r.interactionActor(ctx, item)
			if actor != "" && !seenActors[actor] {
				seenActors[actor] = true
				actors = append(actors, actor)
				if len(actors) >= maxInteractionActors {
					break
				}
			}
		}
		next := activityStreamsLink(doc["next"])
		if next == "" || seenPages[next] || !sameURLHost(originURI, next) || pages >= maxInteractionPages {
			break
		}
		seenPages[next] = true
		fetched, err := r.fetchObject(ctx, next)
		if err != nil {
			break
		}
		doc = fetched
	}
	return actors, total
}

func (r *InteractionResolver) collectionReactions(ctx context.Context, value interface{}, originURI string) []models.EmojiReactionSummary {
	if value == nil {
		return nil
	}
	var doc map[string]interface{}
	switch v := value.(type) {
	case string:
		if !sameURLHost(originURI, v) {
			return nil
		}
		doc, _ = r.fetchObject(ctx, v)
	case map[string]interface{}:
		doc = v
	}
	if doc == nil {
		return nil
	}
	if !hasInlineItems(doc) && doc["first"] == nil {
		if id := activityStreamsLink(doc["id"]); id != "" && sameURLHost(originURI, id) {
			doc, _ = r.fetchObject(ctx, id)
		}
	}
	if doc == nil {
		return nil
	}
	if !hasInlineItems(doc) {
		switch first := doc["first"].(type) {
		case map[string]interface{}:
			doc = first
		case string:
			if sameURLHost(originURI, first) {
				doc, _ = r.fetchObject(ctx, first)
			}
		}
	}
	type group struct {
		summary models.EmojiReactionSummary
		actors  []string
	}
	groups := make([]group, 0)
	positions := map[string]int{}
	seenActivities := map[string]bool{}
	seenPages := map[string]bool{}
	for pages := 0; doc != nil && pages <= maxInteractionPages; pages++ {
		for _, item := range collectionItems(doc) {
			activity, ok := item.(map[string]interface{})
			if !ok {
				uri, _ := item.(string)
				if uri == "" || !sameURLHost(originURI, uri) {
					continue
				}
				activity, _ = r.fetchObject(ctx, uri)
			}
			name, actor, custom := reactionActivity(activity, originURI)
			if name == "" || actor == "" {
				continue
			}
			activityID := activityStreamsLink(activity["id"])
			if activityID == "" {
				activityID = actor + "\x00" + name
			}
			if seenActivities[activityID] {
				continue
			}
			seenActivities[activityID] = true
			i, ok := positions[name]
			if !ok {
				i = len(groups)
				positions[name] = i
				groups = append(groups, group{summary: models.EmojiReactionSummary{Name: name, Key: name, Emoji: custom}})
			}
			groups[i].summary.Count++
			groups[i].actors = append(groups[i].actors, actor)
		}
		next := activityStreamsLink(doc["next"])
		if next == "" || seenPages[next] || !sameURLHost(originURI, next) || pages >= maxInteractionPages {
			break
		}
		seenPages[next] = true
		doc, _ = r.fetchObject(ctx, next)
	}
	result := make([]models.EmojiReactionSummary, 0, len(groups))
	for _, group := range groups {
		group.summary.Accounts = r.mergeAccounts(ctx, nil, group.actors)
		result = append(result, group.summary)
	}
	return result
}

func reactionActivity(activity map[string]interface{}, statusURI string) (string, string, *models.CustomEmoji) {
	if activity == nil || (activity["type"] != "EmojiReact" && activity["type"] != "Like") || activityStreamsLink(activity["object"]) != statusURI {
		return "", "", nil
	}
	content, _ := activity["content"].(string)
	content = strings.TrimSpace(content)
	actor := activityStreamsLink(activity["actor"])
	if emoji.Known(content) {
		return content, actor, nil
	}
	tags, ok := activity["tag"].([]interface{})
	if !ok || len(tags) != 1 || len(content) < 3 || content[0] != ':' || content[len(content)-1] != ':' {
		return "", "", nil
	}
	tag, ok := tags[0].(map[string]interface{})
	shortcode := content[1 : len(content)-1]
	if !ok || shortcode == "" || strings.ContainsAny(shortcode, ":@/\\\r\n\t ") {
		return "", "", nil
	}
	custom, ok := ParseEmojiTag(tag, nil)
	if !ok || custom.Shortcode != shortcode {
		return "", "", nil
	}
	u, err := url.Parse(custom.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return "", "", nil
	}
	domain := strings.ToLower(u.Hostname())
	if actorURL, parseErr := url.Parse(actor); parseErr == nil && actorURL.Hostname() != "" {
		domain = strings.ToLower(actorURL.Hostname())
	}
	custom.Domain = &domain
	return ":" + custom.Shortcode + "@" + domain + ":", actor, custom
}

func (r *InteractionResolver) interactionActor(ctx context.Context, item interface{}) string {
	if object, ok := item.(map[string]interface{}); ok {
		if actor := activityStreamsLink(object["actor"]); actor != "" {
			return actor
		}
		switch object["type"] {
		case "Person", "Service", "Application", "Group", "Organization":
			return activityStreamsLink(object["id"])
		}
		return ""
	}
	uri, ok := item.(string)
	if !ok || uri == "" {
		return ""
	}
	if !r.allowedRemoteURI(ctx, uri) {
		return ""
	}
	object, err := r.fetchObject(ctx, uri)
	if err != nil {
		// Some implementations put bare actor IRIs in the collection. Returning
		// the IRI lets mergeAccounts resolve it as an actor.
		return uri
	}
	if actor := activityStreamsLink(object["actor"]); actor != "" {
		return actor
	}
	switch object["type"] {
	case "Person", "Service", "Application", "Group", "Organization":
		return activityStreamsLink(object["id"])
	}
	return ""
}

func (r *InteractionResolver) allowedRemoteURI(ctx context.Context, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if block, err := r.store.GetDomainBlock(ctx, host); err == nil && block.Severity == "suspend" {
		return false
	}
	if instance, err := r.store.GetInstance(ctx, host); err == nil && instance.Suspended {
		return false
	}
	return true
}

func collectionItems(doc map[string]interface{}) []interface{} {
	if items, ok := doc["orderedItems"].([]interface{}); ok {
		return items
	}
	if items, ok := doc["items"].([]interface{}); ok {
		return items
	}
	return nil
}

func interactionInt(value interface{}) (int, bool) {
	switch n := value.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	default:
		return 0, false
	}
}

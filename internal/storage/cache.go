package storage

import (
	"context"
	"errors"
	"strings"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/cache"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// cachingStorage is a transparent decorator over a Storage that keeps a bounded
// in-RAM copy of the hottest *viewer-neutral* reads (statuses, accounts and
// their media/mentions/tags) and invalidates them precisely on the writes that
// flow through this same interface. Because every persistence call in littleFedi
// goes through Storage, the decorator sees all mutations and can evict exactly
// the affected keys; a per-entry TTL is a backstop for any path not covered.
//
// It does not cache anything viewer-specific (poll vote state and
// the favourited/bookmarked/reblogged flags stay live queries), so a shared
// cache entry can never leak one viewer's state to another. To make that
// guarantee airtight even for the viewer-neutral objects (enrichment mutates the
// returned *models.Status in place), every read returns a fresh copy; the cache
// only ever holds pristine, store-shaped objects.
type cachingStorage struct {
	Storage // embedded: every method not overridden below passes straight through
	c       *cache.MemoryCache
	ttl     time.Duration
}

// NewCachingStorage wraps inner with an in-memory entity cache bounded by c and
// expiring entries after ttl (ttl <= 0 disables expiry, relying solely on write
// invalidation). If c is nil the inner store is returned unchanged.
func NewCachingStorage(inner Storage, c *cache.MemoryCache, ttl time.Duration) Storage {
	if c == nil {
		return inner
	}
	return &cachingStorage{Storage: inner, c: c, ttl: ttl}
}

// keys

func keyStatus(id string) string      { return "status:" + id }
func keyStatusURI(uri string) string  { return "statusuri:" + uri }
func keyAccount(id string) string     { return "account:" + id }
func keyAccountURI(uri string) string { return "accounturi:" + uri }
func keyMentions(id string) string    { return "mentions:" + id }
func keyTags(id string) string        { return "tags:" + id }
func keyMedia(id string) string       { return "media:" + id }

func keyAccountUD(username string, domain *string) string {
	if domain == nil {
		return "acctud:" + strings.ToLower(username) + "|"
	}
	return "acctud:" + username + "|" + *domain
}

// copy-on-get helpers
//
// The cache stores objects exactly as the underlying store returns them (a
// status carries only its Account relation; an account carries no read-time
// extras). Copies reproduce that shape so a cached read is indistinguishable
// from a fresh store read, while isolating callers that mutate the result during
// enrichment.

func copyStatus(s *models.Status) *models.Status {
	if s == nil {
		return nil
	}
	cp := *s
	if s.Account != nil {
		a := *s.Account
		cp.Account = &a
	}
	return &cp
}

func copyAccount(a *models.Account) *models.Account {
	if a == nil {
		return nil
	}
	cp := *a
	return &cp
}

func copyMentions(in []*models.Mention) []*models.Mention {
	if in == nil {
		return nil
	}
	out := make([]*models.Mention, len(in))
	copy(out, in)
	return out
}

func copyTags(in []*models.Tag) []*models.Tag {
	if in == nil {
		return nil
	}
	out := make([]*models.Tag, len(in))
	copy(out, in)
	return out
}

func copyMedia(in []*models.MediaAttachment) []*models.MediaAttachment {
	if in == nil {
		return nil
	}
	out := make([]*models.MediaAttachment, len(in))
	copy(out, in)
	return out
}

// size estimation (approximate bytes)

func accountSize(a *models.Account) int {
	if a == nil {
		return 0
	}
	return 400 + len(a.ID) + len(a.Username) + len(a.DisplayName) + len(a.Note) +
		len(a.ActorURI) + len(a.Avatar) + len(a.Header)
}

func statusSize(s *models.Status) int {
	if s == nil {
		return 0
	}
	return 512 + len(s.ID) + len(s.URI) + len(s.URL) + len(s.ContentHTML) +
		len(s.ContentSource) + len(s.Summary) + len(s.Language) + len(s.ContentType) +
		accountSize(s.Account)
}

func mentionsSize(in []*models.Mention) int {
	n := 32
	for _, m := range in {
		if m != nil {
			n += 96 + len(m.URL)
		}
	}
	return n
}

func tagsSize(in []*models.Tag) int {
	n := 32
	for _, t := range in {
		if t != nil {
			n += 96 + len(t.Name) + len(t.URL)
		}
	}
	return n
}

func mediaSize(in []*models.MediaAttachment) int {
	n := 32
	for _, m := range in {
		if m != nil {
			n += 160 + len(m.URL)
		}
	}
	return n
}

// cached reads

func (cs *cachingStorage) cacheStatus(s *models.Status, version uint64) {
	if s != nil {
		cs.c.SetIfVersionWithDependencies(version, keyStatus(s.ID), s, statusSize(s), cs.ttl, statusDependencies(s))
	}
}

func statusDependencies(s *models.Status) []string {
	deps := []string{"status:" + s.ID, "account:" + s.AccountID}
	for _, ref := range []*string{s.BoostOfID, s.InReplyToID, s.QuoteOfID} {
		if ref != nil && *ref != "" {
			deps = append(deps, "status:"+*ref)
		}
	}
	return deps
}

func (cs *cachingStorage) GetStatus(ctx context.Context, id string) (*models.Status, error) {
	version := cs.c.Version()
	if v, ok := cs.c.Get(keyStatus(id)); ok {
		return copyStatus(v.(*models.Status)), nil
	}
	s, err := cs.Storage.GetStatus(ctx, id)
	if err != nil {
		return nil, err
	}
	cs.cacheStatus(s, version)
	return copyStatus(s), nil
}

func (cs *cachingStorage) GetStatusByURI(ctx context.Context, uri string) (*models.Status, error) {
	version := cs.c.Version()
	if v, ok := cs.c.Get(keyStatusURI(uri)); ok {
		return copyStatus(v.(*models.Status)), nil
	}
	s, err := cs.Storage.GetStatusByURI(ctx, uri)
	if err != nil {
		return nil, err
	}
	cs.cacheStatus(s, version)
	cs.c.SetIfVersionWithDependencies(version, keyStatusURI(uri), s, statusSize(s), cs.ttl, statusDependencies(s))
	return copyStatus(s), nil
}

func (cs *cachingStorage) GetStatusesByIDs(ctx context.Context, ids []string) (map[string]*models.Status, error) {
	version := cs.c.Version()
	out := make(map[string]*models.Status, len(ids))
	var miss []string
	for _, id := range ids {
		if v, ok := cs.c.Get(keyStatus(id)); ok {
			out[id] = copyStatus(v.(*models.Status))
		} else {
			miss = append(miss, id)
		}
	}
	if len(miss) == 0 {
		return out, nil
	}
	fetched, err := cs.Storage.GetStatusesByIDs(ctx, miss)
	if err != nil {
		return out, err
	}
	for id, s := range fetched {
		cs.cacheStatus(s, version)
		out[id] = copyStatus(s)
	}
	return out, nil
}

func (cs *cachingStorage) cacheAccount(a *models.Account) {
	if a != nil {
		cs.c.Set(keyAccount(a.ID), a, accountSize(a), cs.ttl)
	}
}

func (cs *cachingStorage) GetAccount(ctx context.Context, id string) (*models.Account, error) {
	if v, ok := cs.c.Get(keyAccount(id)); ok {
		a := v.(*models.Account)
		if a.Domain != nil && *a.Domain != "" {
			return copyAccount(a), nil
		}
		// Local account reads also authorize logins and administrative actions.
		return cs.Storage.GetAccount(ctx, id)
	}
	a, err := cs.Storage.GetAccount(ctx, id)
	if err != nil {
		return nil, err
	}
	cs.cacheAccount(a)
	return copyAccount(a), nil
}

func (cs *cachingStorage) GetAccountByURI(ctx context.Context, uri string) (*models.Account, error) {
	if v, ok := cs.c.Get(keyAccountURI(uri)); ok {
		return copyAccount(v.(*models.Account)), nil
	}
	a, err := cs.Storage.GetAccountByURI(ctx, uri)
	if err != nil {
		return nil, err
	}
	cs.cacheAccount(a)
	cs.c.Set(keyAccountURI(uri), a, accountSize(a), cs.ttl)
	return copyAccount(a), nil
}

func (cs *cachingStorage) GetAccountByUsernameDomain(ctx context.Context, username string, domain *string) (*models.Account, error) {
	if domain == nil || *domain == "" {
		return cs.Storage.GetAccountByUsernameDomain(ctx, username, domain)
	}
	k := keyAccountUD(username, domain)
	if v, ok := cs.c.Get(k); ok {
		return copyAccount(v.(*models.Account)), nil
	}
	a, err := cs.Storage.GetAccountByUsernameDomain(ctx, username, domain)
	if err != nil {
		return nil, err
	}
	cs.cacheAccount(a)
	cs.c.Set(k, a, accountSize(a), cs.ttl)
	return copyAccount(a), nil
}

func (cs *cachingStorage) GetAccountsByIDs(ctx context.Context, ids []string) (map[string]*models.Account, error) {
	out := make(map[string]*models.Account, len(ids))
	var miss []string
	for _, id := range ids {
		if v, ok := cs.c.Get(keyAccount(id)); ok {
			out[id] = copyAccount(v.(*models.Account))
		} else {
			miss = append(miss, id)
		}
	}
	if len(miss) == 0 {
		return out, nil
	}
	fetched, err := cs.Storage.GetAccountsByIDs(ctx, miss)
	if err != nil {
		return out, err
	}
	for id, a := range fetched {
		cs.cacheAccount(a)
		out[id] = copyAccount(a)
	}
	return out, nil
}

func (cs *cachingStorage) GetMentionsForStatuses(ctx context.Context, statusIDs []string) (map[string][]*models.Mention, error) {
	version := cs.c.Version()
	out := make(map[string][]*models.Mention, len(statusIDs))
	var miss []string
	for _, id := range statusIDs {
		if v, ok := cs.c.Get(keyMentions(id)); ok {
			out[id] = copyMentions(v.([]*models.Mention))
		} else {
			miss = append(miss, id)
		}
	}
	if len(miss) == 0 {
		return out, nil
	}
	fetched, err := cs.Storage.GetMentionsForStatuses(ctx, miss)
	if err != nil {
		return out, err
	}
	for _, id := range miss {
		m := fetched[id]
		cs.c.SetIfVersionWithDependencies(version, keyMentions(id), m, mentionsSize(m), cs.ttl, []string{"status:" + id})
		out[id] = copyMentions(m)
	}
	return out, nil
}

func (cs *cachingStorage) GetTagsForStatuses(ctx context.Context, statusIDs []string) (map[string][]*models.Tag, error) {
	version := cs.c.Version()
	out := make(map[string][]*models.Tag, len(statusIDs))
	var miss []string
	for _, id := range statusIDs {
		if v, ok := cs.c.Get(keyTags(id)); ok {
			out[id] = copyTags(v.([]*models.Tag))
		} else {
			miss = append(miss, id)
		}
	}
	if len(miss) == 0 {
		return out, nil
	}
	fetched, err := cs.Storage.GetTagsForStatuses(ctx, miss)
	if err != nil {
		return out, err
	}
	for _, id := range miss {
		t := fetched[id]
		cs.c.SetIfVersionWithDependencies(version, keyTags(id), t, tagsSize(t), cs.ttl, []string{"status:" + id})
		out[id] = copyTags(t)
	}
	return out, nil
}

func (cs *cachingStorage) GetMediaForStatuses(ctx context.Context, statusIDs []string) (map[string][]*models.MediaAttachment, error) {
	version := cs.c.Version()
	out := make(map[string][]*models.MediaAttachment, len(statusIDs))
	var miss []string
	for _, id := range statusIDs {
		if v, ok := cs.c.Get(keyMedia(id)); ok {
			out[id] = copyMedia(v.([]*models.MediaAttachment))
		} else {
			miss = append(miss, id)
		}
	}
	if len(miss) == 0 {
		return out, nil
	}
	fetched, err := cs.Storage.GetMediaForStatuses(ctx, miss)
	if err != nil {
		return out, err
	}
	for _, id := range miss {
		m := fetched[id]
		cs.c.SetIfVersionWithDependencies(version, keyMedia(id), m, mediaSize(m), cs.ttl, []string{"status:" + id})
		out[id] = copyMedia(m)
	}
	return out, nil
}

// invalidating writes

// invalidateStatus drops every cache entry keyed by a status id: the status
// itself, its URI alias (looked up from the cached object), and its relation
// lists.
func (cs *cachingStorage) invalidateStatus(id string) {
	if v, ok := cs.c.Get(keyStatus(id)); ok {
		if st, _ := v.(*models.Status); st != nil && st.URI != "" {
			cs.c.Delete(keyStatusURI(st.URI))
		}
	}
	cs.c.DeleteDependencies("status:" + id)
	cs.c.Delete(keyStatus(id), keyMentions(id), keyTags(id), keyMedia(id))
}

// invalidateStatusRelatives drops the parent/boost/quote targets whose
// denormalized counts move when a status is created or removed.
func (cs *cachingStorage) invalidateStatusRelatives(s *models.Status) {
	if s == nil {
		return
	}
	if s.InReplyToID != nil && *s.InReplyToID != "" {
		cs.c.Delete(keyStatus(*s.InReplyToID))
	}
	if s.BoostOfID != nil && *s.BoostOfID != "" {
		cs.c.Delete(keyStatus(*s.BoostOfID))
	}
	if s.QuoteOfID != nil && *s.QuoteOfID != "" {
		cs.c.Delete(keyStatus(*s.QuoteOfID))
	}
	if s.AccountID != "" {
		cs.c.Delete(keyAccount(s.AccountID)) // statuses_count
	}
}

func (cs *cachingStorage) CreateStatus(ctx context.Context, status *models.Status) error {
	err := cs.Storage.CreateStatus(ctx, status)
	cs.statusCreated(status, err)
	return err
}

func (cs *cachingStorage) CreateStatusWithJobs(ctx context.Context, status *models.Status, jobs []*models.Job) error {
	err := cs.Storage.CreateStatusWithJobs(ctx, status, jobs)
	cs.statusCreated(status, err)
	return err
}

func (cs *cachingStorage) statusCreated(status *models.Status, err error) {
	if err == nil {
		cs.invalidateStatus(status.ID)
		if status.URI != "" {
			cs.c.Delete(keyStatusURI(status.URI))
		}
		cs.invalidateStatusRelatives(status)
	}
}

func (cs *cachingStorage) SyncPoll(ctx context.Context, poll *models.Poll) error {
	err := cs.Storage.SyncPoll(ctx, poll)
	if err == nil && poll != nil && poll.StatusID != "" {
		cs.invalidateStatus(poll.StatusID)
	}
	return err
}

func (cs *cachingStorage) UpdateStatus(ctx context.Context, status *models.Status) error {
	err := cs.Storage.UpdateStatus(ctx, status)
	if err == nil {
		cs.invalidateStatus(status.ID)
		if status.URI != "" {
			cs.c.Delete(keyStatusURI(status.URI))
		}
	}
	return err
}

func (cs *cachingStorage) SetStatusContextOnly(ctx context.Context, statusID string, contextOnly bool) error {
	err := cs.Storage.SetStatusContextOnly(ctx, statusID, contextOnly)
	if err == nil {
		cs.invalidateStatus(statusID)
	}
	return err
}

func (cs *cachingStorage) SetStatusBlog(ctx context.Context, statusID string, isBlogPost bool, slug string) error {
	err := cs.Storage.SetStatusBlog(ctx, statusID, isBlogPost, slug)
	if err == nil {
		cs.invalidateStatus(statusID)
	}
	return err
}

func (cs *cachingStorage) UpdateStatusContent(ctx context.Context, status *models.Status) error {
	err := cs.Storage.UpdateStatusContent(ctx, status)
	if err == nil {
		cs.invalidateStatus(status.ID)
		if status.URI != "" {
			cs.c.Delete(keyStatusURI(status.URI))
		}
	}
	return err
}

func (cs *cachingStorage) UpdateStatusQuoteState(ctx context.Context, statusID, state string, approvalURI *string) error {
	err := cs.Storage.UpdateStatusQuoteState(ctx, statusID, state, approvalURI)
	if err == nil {
		cs.invalidateStatus(statusID)
	}
	return err
}

func (cs *cachingStorage) SetBlogSlug(ctx context.Context, statusID, slug string) error {
	err := cs.Storage.SetBlogSlug(ctx, statusID, slug)
	if err == nil {
		cs.invalidateStatus(statusID)
	}
	return err
}

func (cs *cachingStorage) AdjustStatusRepliesCount(ctx context.Context, statusID string, delta int) error {
	err := cs.Storage.AdjustStatusRepliesCount(ctx, statusID, delta)
	if err == nil {
		cs.invalidateStatus(statusID)
	}
	return err
}

func (cs *cachingStorage) DeleteStatus(ctx context.Context, id string) error {
	// Capture relatives from the cached copy before the row (and its cache
	// entry) disappear, so parent/boost counts are refreshed too.
	var relatives *models.Status
	if v, ok := cs.c.Get(keyStatus(id)); ok {
		relatives, _ = v.(*models.Status)
	}
	err := cs.Storage.DeleteStatus(ctx, id)
	if err == nil {
		cs.invalidateStatus(id)
		cs.invalidateStatusRelatives(relatives)
	}
	return err
}

func (cs *cachingStorage) UpdateAccount(ctx context.Context, account *models.Account) error {
	err := cs.Storage.UpdateAccount(ctx, account)
	if err == nil {
		cs.c.DeleteDependencies("account:" + account.ID)
		cs.invalidateAccount(account)
	}
	return err
}

func (cs *cachingStorage) SetAccountMoved(ctx context.Context, accountID, movedToURI string) error {
	account, err := cs.Storage.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if err := cs.Storage.SetAccountMoved(ctx, accountID, movedToURI); err != nil {
		return err
	}
	cs.invalidateAccount(account)
	cs.c.DeleteDependencies("account:" + accountID)
	return nil
}

func (cs *cachingStorage) DeleteAccount(ctx context.Context, id string) error {
	var cached *models.Account
	if v, ok := cs.c.Get(keyAccount(id)); ok {
		cached, _ = v.(*models.Account)
	}
	err := cs.Storage.DeleteAccount(ctx, id)
	if err == nil {
		cs.c.Delete(keyAccount(id))
		cs.invalidateAccount(cached)
	}
	return err
}

func (cs *cachingStorage) ScheduleAccountDeletion(ctx context.Context, accountID string, deadline time.Time) (*models.AccountDeletionRun, bool, error) {
	before, _ := cs.Storage.GetAccount(ctx, accountID)
	run, created, err := cs.Storage.ScheduleAccountDeletion(ctx, accountID, deadline)
	if err == nil && created {
		cs.invalidateAccount(before)
		cs.c.DeletePrefix("session:")
		cs.c.DeletePrefix("oauthtok:")
	}
	return run, created, err
}

func (cs *cachingStorage) CompleteAccountDeletion(ctx context.Context, runID string) error {
	run, _ := cs.Storage.GetAccountDeletionRun(ctx, runID)
	var before *models.Account
	if run != nil {
		before, _ = cs.Storage.GetAccount(ctx, run.AccountID)
	}
	err := cs.Storage.CompleteAccountDeletion(ctx, runID)
	if err == nil {
		cs.invalidateAccount(before)
		cs.c.DeletePrefix("session:")
		cs.c.DeletePrefix("oauthtok:")
	}
	return err
}

func (cs *cachingStorage) VerifyEmail(ctx context.Context, tokenHash string, now time.Time) (*models.Account, error) {
	account, err := cs.Storage.VerifyEmail(ctx, tokenHash, now)
	if err == nil {
		cs.invalidateAccount(account)
	}
	return account, err
}

func (cs *cachingStorage) SetAccountMFA(ctx context.Context, accountID string, secret string, hashes []string) error {
	before, err := cs.Storage.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if err := cs.Storage.SetAccountMFA(ctx, accountID, secret, hashes); err != nil {
		return err
	}
	cs.invalidateAccount(before)
	cs.c.DeleteDependencies("account:" + accountID)
	return nil
}

func (cs *cachingStorage) DisableAccountMFA(ctx context.Context, accountID string) error {
	before, err := cs.Storage.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if err := cs.Storage.DisableAccountMFA(ctx, accountID); err != nil {
		return err
	}
	cs.invalidateAccount(before)
	cs.c.DeleteDependencies("account:" + accountID)
	return nil
}

func (cs *cachingStorage) ReplaceMFARecoveryCodes(ctx context.Context, accountID string, hashes []string) error {
	before, err := cs.Storage.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if err := cs.Storage.ReplaceMFARecoveryCodes(ctx, accountID, hashes); err != nil {
		return err
	}
	cs.invalidateAccount(before)
	cs.c.DeleteDependencies("account:" + accountID)
	return nil
}

func (cs *cachingStorage) invalidateAccount(a *models.Account) {
	if a == nil {
		return
	}
	cs.c.Delete(keyAccount(a.ID), keyAccountUD(a.Username, a.Domain))
	if a.ActorURI != "" {
		cs.c.Delete(keyAccountURI(a.ActorURI))
	}
}

func (cs *cachingStorage) AdjustStatusReblogsCount(ctx context.Context, statusID string, delta int) error {
	err := cs.Storage.AdjustStatusReblogsCount(ctx, statusID, delta)
	if err == nil {
		cs.c.Delete(keyStatus(statusID)) // reblogs_count
	}
	return err
}

func (cs *cachingStorage) CreateFavourite(ctx context.Context, fave *models.Favourite) error {
	err := cs.Storage.CreateFavourite(ctx, fave)
	if err == nil && fave != nil {
		cs.c.Delete(keyStatus(fave.StatusID)) // favourites_count
	}
	return err
}

func (cs *cachingStorage) DeleteFavourite(ctx context.Context, accountID, statusID string) error {
	err := cs.Storage.DeleteFavourite(ctx, accountID, statusID)
	if err == nil {
		cs.c.Delete(keyStatus(statusID))
	}
	return err
}

func (cs *cachingStorage) CreateEmojiReaction(ctx context.Context, reaction *models.EmojiReaction) (bool, error) {
	created, err := cs.Storage.CreateEmojiReaction(ctx, reaction)
	if err == nil && created && reaction != nil {
		cs.c.Delete(keyStatus(reaction.StatusID))
	}
	return created, err
}

func (cs *cachingStorage) DeleteEmojiReaction(ctx context.Context, accountID, statusID, key string) (*models.EmojiReaction, error) {
	reaction, err := cs.Storage.DeleteEmojiReaction(ctx, accountID, statusID, key)
	if err == nil {
		cs.c.Delete(keyStatus(statusID))
	}
	return reaction, err
}

func (cs *cachingStorage) DeleteEmojiReactionByURI(ctx context.Context, accountID, uri string) (*models.EmojiReaction, error) {
	reaction, err := cs.Storage.DeleteEmojiReactionByURI(ctx, accountID, uri)
	if err == nil && reaction != nil {
		cs.c.Delete(keyStatus(reaction.StatusID))
	}
	return reaction, err
}

func (cs *cachingStorage) CreateMediaAttachment(ctx context.Context, media *models.MediaAttachment) error {
	err := cs.Storage.CreateMediaAttachment(ctx, media)
	if err == nil && media != nil && media.StatusID != nil && *media.StatusID != "" {
		cs.c.Delete(keyMedia(*media.StatusID))
	}
	return err
}

func (cs *cachingStorage) UpdateMediaAttachment(ctx context.Context, media *models.MediaAttachment) error {
	err := cs.Storage.UpdateMediaAttachment(ctx, media)
	if err == nil && media != nil && media.StatusID != nil && *media.StatusID != "" {
		cs.c.Delete(keyMedia(*media.StatusID))
	}
	return err
}

func (cs *cachingStorage) CreateMention(ctx context.Context, mention *models.Mention) error {
	err := cs.Storage.CreateMention(ctx, mention)
	if err == nil && mention != nil {
		cs.c.Delete(keyMentions(mention.StatusID))
	}
	return err
}

func (cs *cachingStorage) CreateMentions(ctx context.Context, mentions []*models.Mention) error {
	err := cs.Storage.CreateMentions(ctx, mentions)
	if err == nil {
		for _, m := range mentions {
			if m != nil {
				cs.c.Delete(keyMentions(m.StatusID))
			}
		}
	}
	return err
}

func (cs *cachingStorage) CreateStatusTags(ctx context.Context, statusTags []*models.StatusTag) error {
	err := cs.Storage.CreateStatusTags(ctx, statusTags)
	if err == nil {
		for _, st := range statusTags {
			if st != nil {
				cs.c.Delete(keyTags(st.StatusID))
			}
		}
	}
	return err
}

func (cs *cachingStorage) DeleteStatusTags(ctx context.Context, statusID string) error {
	err := cs.Storage.DeleteStatusTags(ctx, statusID)
	if err == nil {
		cs.c.Delete(keyTags(statusID))
	}
	return err
}

func (cs *cachingStorage) SyncStatusTags(ctx context.Context, statusID string, tags []*models.Tag) (bool, error) {
	changed, err := cs.Storage.SyncStatusTags(ctx, statusID, tags)
	if err == nil && changed {
		cs.c.Delete(keyTags(statusID))
	}
	return changed, err
}

func (cs *cachingStorage) LinkStatusRepliesByURI(ctx context.Context, parentURI, parentID string) error {
	err := cs.Storage.LinkStatusRepliesByURI(ctx, parentURI, parentID)
	if err == nil {
		cs.c.Delete(keyStatus(parentID)) // replies_count
	}
	return err
}

func (cs *cachingStorage) LinkStatusQuotesByURI(ctx context.Context, quotedURI, quotedID string) error {
	err := cs.Storage.LinkStatusQuotesByURI(ctx, quotedURI, quotedID)
	if err == nil {
		cs.c.Delete(keyStatus(quotedID))
	}
	return err
}

func (cs *cachingStorage) CreateFollow(ctx context.Context, follow *models.Follow) error {
	err := cs.Storage.CreateFollow(ctx, follow)
	if err == nil && follow != nil {
		cs.c.Delete(keyAccount(follow.AccountID), keyAccount(follow.TargetAccountID))
	}
	return err
}

func (cs *cachingStorage) AdvanceFollowImport(ctx context.Context, imp *models.AccountImport, expected string, follow *models.Follow, jobs []*models.Job) (bool, error) {
	advanced, err := cs.Storage.AdvanceFollowImport(ctx, imp, expected, follow, jobs)
	if err == nil && advanced && follow != nil {
		cs.c.Delete(keyAccount(follow.AccountID), keyAccount(follow.TargetAccountID))
	}
	if err == nil && advanced && imp.State == "complete" {
		cs.invalidateNotifications(imp.AccountID)
	}
	return advanced, err
}

func (cs *cachingStorage) FailJob(ctx context.Context, id string, cause error) error {
	err := cs.Storage.FailJob(ctx, id, cause)
	if err == nil {
		if notif, readErr := cs.Storage.GetNotification(ctx, id); readErr == nil {
			if _, importID := models.ParseFollowImportNotificationType(notif.Type); importID != "" {
				cs.invalidateNotifications(notif.AccountID)
			}
		}
	}
	return err
}

func (cs *cachingStorage) DeleteFollow(ctx context.Context, id string) error {
	// Best-effort: fetch the follow before deletion so both endpoints' follower
	// counts can be refreshed. Falls back to the TTL backstop if unavailable.
	var f *models.Follow
	if got, gErr := cs.Storage.GetFollow(ctx, id); gErr == nil {
		f = got
	}
	err := cs.Storage.DeleteFollow(ctx, id)
	if err == nil && f != nil {
		cs.c.Delete(keyAccount(f.AccountID), keyAccount(f.TargetAccountID))
	}
	return err
}

// Sessions and OAuth tokens use the underlying store directly. Revocation and
// rotation must take effect across processes and concurrent cache misses.

func (s *cachingStorage) EditStatusWithJobs(ctx context.Context, st *models.Status, expected time.Time, jobs []*models.Job) error {
	err := s.Storage.EditStatusWithJobs(ctx, st, expected, jobs)
	// A conflict proves the cached version is stale; reloading must fetch it again.
	if err == nil || errors.Is(err, ErrStatusConflict) {
		s.invalidateStatus(st.ID)
		s.c.Delete(keyStatusURI(st.URI))
	}
	return err
}

func (s *cachingStorage) CreateFavouriteWithNotification(ctx context.Context, f *models.Favourite, notif *models.Notification, jobs []*models.Job) (bool, error) {
	notified, err := s.Storage.CreateFavouriteWithNotification(ctx, f, notif, jobs)
	if err == nil {
		s.invalidateStatus(f.StatusID)
		if notified && notif != nil {
			s.invalidateNotifications(notif.AccountID)
		}
	}
	return notified, err
}

func (s *cachingStorage) CreateBoostWithNotification(ctx context.Context, st *models.Status, notif *models.Notification, jobs []*models.Job) (bool, error) {
	notified, err := s.Storage.CreateBoostWithNotification(ctx, st, notif, jobs)
	if err == nil {
		s.statusCreated(st, nil)
		if notified && notif != nil {
			s.invalidateNotifications(notif.AccountID)
		}
	}
	return notified, err
}

func (cs *cachingStorage) PublishScheduledStatusWithJobs(ctx context.Context, expected *models.ScheduledStatus, status *models.Status, jobs []*models.Job) error {
	err := cs.Storage.PublishScheduledStatusWithJobs(ctx, expected, status, jobs)
	cs.statusCreated(status, err)
	return err
}

func (cs *cachingStorage) DeleteStatusWithJobs(ctx context.Context, expected *models.Status, expiredOnly bool, tombstone *models.Tombstone, jobs []*models.Job) error {
	err := cs.Storage.DeleteStatusWithJobs(ctx, expected, expiredOnly, tombstone, jobs)
	if err == nil {
		cs.invalidateStatus(expected.ID)
		cs.invalidateStatusRelatives(expected)
	}
	return err
}

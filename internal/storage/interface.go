// Package storage defines the Storage interface and provides database implementations.
// Define interfaces first, implement behind them.
package storage

import (
	"context"
	"errors"
	"io"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

var ErrScheduleChanged = errors.New("scheduled status changed or is not due")

var ErrStatusConflict = errors.New("status changed; reload before editing")
var ErrInvalidMedia = errors.New("invalid media attachments")

var ErrInviteUnavailable = errors.New("invite is expired or exhausted")
var ErrAccountDeletionLastAdmin = errors.New("the last administrator cannot be deleted")
var ErrAccountDeletionPending = errors.New("account deletion deliveries are still pending")
var ErrEmailVerificationInvalid = errors.New("email verification is invalid or expired")
var ErrFeaturedTagExists = errors.New("tag is already featured")
var ErrInvalidDomainBlock = errors.New("invalid domain block")
var ErrFederationBlocked = errors.New("federation domain blocked by policy")

// PruneExpiredDataResult is the per-category summary emitted by housekeeping.
type PruneExpiredDataResult struct {
	PushSubscriptions, Sessions, OAuthTokens             int64
	PasswordResets, EmailVerifications, Filters          int64
	AccountExports, AccountImports, OrphanAccountImports int64
	Tags, Emojis, Instances                              int64
}

// Storage is the persistence boundary for every domain entity.
// Two implementations: SQLite (WAL) and PostgreSQL, behind the same interface.
type Storage interface {
	// Lifecycle
	Close() error
	Ping(ctx context.Context) error
	RunMigrations(ctx context.Context) error

	// ExportDatabase writes a portable database dump to w.
	// For SQLite this uses its online backup API; for PostgreSQL it shells out to pg_dump.
	ExportDatabase(ctx context.Context, w io.Writer) error
	// ExportDatabaseToFile publishes a complete database backup atomically at path.
	ExportDatabaseToFile(ctx context.Context, path string) error

	// ImportDatabase restores a database from a portable dump.
	// For SQLite this reads a .db file; for PostgreSQL it pipes through psql.
	ImportDatabase(ctx context.Context, r io.Reader, dialect string) error

	// Accounts

	CreateAccount(ctx context.Context, account *models.Account) error
	GetAccount(ctx context.Context, id string) (*models.Account, error)
	GetAccountByURI(ctx context.Context, uri string) (*models.Account, error)
	GetAccountByUsernameDomain(ctx context.Context, username string, domain *string) (*models.Account, error)
	// UpdateAccount rejects stale UpdatedAt values and revokes credentials on password changes.
	UpdateAccount(ctx context.Context, account *models.Account) error
	DeleteAccount(ctx context.Context, id string) error
	ListAccounts(ctx context.Context, offset, limit int) ([]*models.Account, error)
	GetLocalAccount(ctx context.Context) (*models.Account, error) // human account in single-user mode
	RepairLocalAccountCounts(ctx context.Context) error

	// Search accounts by username, handle, or display name. Results are ranked
	// (exact handle match first, then prefix, then substring) and paginated.
	SearchAccounts(ctx context.Context, query string, offset, limit int) ([]*models.Account, error)
	SearchAccountPrefixes(ctx context.Context, query, followingID string, offset, limit int) ([]*models.Account, error)

	// GetRemoteAccountsStaleSince returns remote accounts whose FetchedAt is
	// older than the given time, or NULL, ordered by oldest first.
	GetRemoteAccountsStaleSince(ctx context.Context, since time.Time, limit int) ([]*models.Account, error)
	// ListRemoteAccounts keyset-paginates every known remote actor ordered by
	// id ASC, for bulk operations like a full refresh sweep.
	ListRemoteAccounts(ctx context.Context, afterID string, limit int) ([]*models.Account, error)
	ClaimDueActorRefreshes(ctx context.Context, now, leaseUntil time.Time, limit int) ([]*models.Account, error)
	ClaimActorRefresh(ctx context.Context, accountID string, now, leaseUntil time.Time) (bool, error)
	SetActorRefreshSchedule(ctx context.Context, accountID string, next time.Time, failures int) error
	ReleaseActorRefreshClaim(ctx context.Context, accountID string) error

	// Statuses

	CreateStatus(ctx context.Context, status *models.Status) error
	// CreateStatusWithJobs commits the status and its follow-up work together.
	CreateStatusWithJobs(ctx context.Context, status *models.Status, jobs []*models.Job) error
	// PublishScheduledStatusWithJobs consumes the expected due schedule with the post and jobs.
	PublishScheduledStatusWithJobs(ctx context.Context, expected *models.ScheduledStatus, status *models.Status, jobs []*models.Job) error
	// StatusAffectsHomeTimeline reports whether a stored status can appear in at
	// least one local user's home timeline (author followed, local author, or a
	// followed tag). It is used to avoid invalidating every home on shared-inbox
	// deliveries that no local user requested.
	StatusAffectsHomeTimeline(ctx context.Context, status *models.Status) (bool, error)
	// HomeTimelineViewers returns the local account IDs whose home timeline can
	// include the status (local author, local followers, local tag-followers).
	// Used for per-viewer home cache invalidation and home-feed fan-out.
	HomeTimelineViewers(ctx context.Context, status *models.Status) ([]string, error)
	// PruneHomeFeeds caps each local user's materialized home feed at
	// keepPerUser rows (0 = default cap). Called by housekeeping.
	PruneHomeFeeds(ctx context.Context, keepPerUser int) (int, error)
	// Optimize refreshes query-planner statistics (SQLite PRAGMA optimize;
	// no-op on PostgreSQL). Called by housekeeping.
	Optimize(ctx context.Context) error
	GetStatus(ctx context.Context, id string) (*models.Status, error)
	GetStatusByURI(ctx context.Context, uri string) (*models.Status, error)
	ListStatusQuotes(ctx context.Context, statusID string, limit int) ([]*models.Status, error)

	// Batch loaders used to enrich whole timelines without a per-status query
	// fan-out (the EnrichStatus N+1). Each returns a map keyed by id; missing
	// ids are simply absent (or map to an empty slice for the ...ForStatuses
	// relation loaders).
	GetStatusesByIDs(ctx context.Context, ids []string) (map[string]*models.Status, error)
	GetAccountsByIDs(ctx context.Context, ids []string) (map[string]*models.Account, error)
	GetMentionsForStatuses(ctx context.Context, statusIDs []string) (map[string][]*models.Mention, error)
	GetTagsForStatuses(ctx context.Context, statusIDs []string) (map[string][]*models.Tag, error)
	GetMediaForStatuses(ctx context.Context, statusIDs []string) (map[string][]*models.MediaAttachment, error)
	// ViewerStatusInteractions returns the viewer's favourited/bookmarked/boosted
	// subsets of statusIDs in one query.
	ViewerStatusInteractions(ctx context.Context, accountID string, statusIDs []string) (faved, booked, reblogged map[string]bool, err error)
	GetReblogByAccountStatus(ctx context.Context, accountID, statusID string) (*models.Status, error)
	UpdateStatus(ctx context.Context, status *models.Status) error
	// SetStatusBlog atomically updates a status's blog membership and permalink.
	SetStatusBlog(ctx context.Context, statusID string, isBlogPost bool, slug string) error
	// UpdateStatusContent updates only editable content fields, preserving all
	// concurrently maintained relationship counters and routing columns.
	UpdateStatusContent(ctx context.Context, status *models.Status) error
	EditStatusWithJobs(ctx context.Context, status *models.Status, expected time.Time, jobs []*models.Job) error
	CanReadStatusVersion(ctx context.Context, status *models.Status, viewerID string) (bool, error)
	UpdateStatusExpiresAt(ctx context.Context, statusID string, expiresAt *time.Time) error
	UpdateStatusQuoteState(ctx context.Context, statusID, state string, approvalURI *string) error
	AdjustStatusRepliesCount(ctx context.Context, statusID string, delta int) error
	// AdjustStatusReblogsCount bumps reblogs_count by delta (floored at 0) with
	// a targeted single-column UPDATE - no full-row write, no read-before-write.
	AdjustStatusReblogsCount(ctx context.Context, statusID string, delta int) error
	SetStatusContextFetchedAt(ctx context.Context, statusID string, fetchedAt time.Time) error
	SetStatusContextOnly(ctx context.Context, statusID string, contextOnly bool) error
	// LinkStatusRepliesByURI connects children that only knew their parent's
	// ActivityPub URI and refreshes the parent's direct reply count.
	LinkStatusRepliesByURI(ctx context.Context, parentURI, parentID string) error
	// LinkStatusQuotesByURI connects quoting statuses that only knew the quoted
	// post's ActivityPub URI to its now-known local id.
	LinkStatusQuotesByURI(ctx context.Context, quotedURI, quotedID string) error
	DeleteStatus(ctx context.Context, id string) error
	// DeleteStatusWithJobs checks the version and optional expiry before committing deletion and delivery work.
	DeleteStatusWithJobs(ctx context.Context, expected *models.Status, expiredOnly bool, tombstone *models.Tombstone, jobs []*models.Job) error
	GetStatusPreview(ctx context.Context, statusID string) (*models.StatusPreview, error)
	UpsertStatusPreview(ctx context.Context, preview *models.StatusPreview) error
	SyncStatusPreview(ctx context.Context, statusID string, preview *models.StatusPreview) error
	GetCachedStatusPreviewsOlderThan(ctx context.Context, cutoff time.Time, limit int) ([]*models.StatusPreview, error)

	// Timeline queries
	GetHomeTimeline(ctx context.Context, accountID string, maxID, minID, sinceID *string, limit int, excludeReplies, excludeReblogs bool) ([]*models.Status, error)
	GetPublicTimeline(ctx context.Context, viewerID string, local bool, maxID, minID, sinceID *string, limit int, excludeReplies, excludeReblogs bool) ([]*models.Status, error)
	GetAccountStatuses(ctx context.Context, viewerID, accountID string, maxID, minID, sinceID *string, limit int, excludeReplies, excludeReblogs, onlyMedia, pinnedOnly bool) ([]*models.Status, error)
	// GetBlogStatuses returns the account's public blog posts, newest first.
	// limit <= 0 returns all of them (what the static site generator wants).
	GetBlogStatuses(ctx context.Context, accountID string, maxID *string, limit int) ([]*models.Status, error)
	// GetRSSStatuses returns the latest 20 federatable top-level originals.
	GetRSSStatuses(ctx context.Context, accountID string) ([]*models.Status, error)
	// GetBlogSlugs returns every permalink slug already assigned to one of the
	// account's statuses, mapped to the status that owns it. The site
	// generator seeds its collision check with this so a slug taken by a post
	// it is not rendering (one turned non-public since) is still respected.
	GetBlogSlugs(ctx context.Context, accountID string) (map[string]string, error)
	// SetBlogSlug records the permalink slug the generator assigned to a post.
	SetBlogSlug(ctx context.Context, statusID, slug string) error
	GetStatusContext(ctx context.Context, statusID string) (ancestors, descendants []*models.Status, err error)

	CountStatuses(ctx context.Context) (int, error)
	CountLocalStatuses(ctx context.Context) (int, error)
	CountAccountStatuses(ctx context.Context, accountID string) (int, error)

	// Polls
	CreatePoll(ctx context.Context, poll *models.Poll) error
	GetPoll(ctx context.Context, id, viewerID string) (*models.Poll, error)
	GetPollForStatus(ctx context.Context, statusID, viewerID string) (*models.Poll, error)
	GetStatusPreviewsForStatuses(ctx context.Context, statusIDs []string) (map[string]*models.StatusPreview, error)
	GetPollsForStatuses(ctx context.Context, statusIDs []string, viewerID string) (map[string]*models.Poll, error)
	SyncPoll(ctx context.Context, poll *models.Poll) error
	VotePoll(ctx context.Context, pollID, accountID string, choices []int) (*models.Poll, error)
	ClosePoll(ctx context.Context, pollID string) error

	// Filters v2
	CreateFilter(ctx context.Context, filter *models.Filter) error
	GetFilter(ctx context.Context, id, accountID string) (*models.Filter, error)
	ListFilters(ctx context.Context, accountID string) ([]*models.Filter, error)
	UpdateFilter(ctx context.Context, filter *models.Filter) error
	DeleteFilter(ctx context.Context, id, accountID string) error

	// Scheduled statuses and announcements
	CreateScheduledStatus(ctx context.Context, status *models.ScheduledStatus) error
	GetScheduledStatus(ctx context.Context, id, accountID string) (*models.ScheduledStatus, error)
	ListScheduledStatuses(ctx context.Context, accountID string, limit int) ([]*models.ScheduledStatus, error)
	UpdateScheduledStatusTime(ctx context.Context, id, accountID string, scheduledAt time.Time) error
	DeleteScheduledStatus(ctx context.Context, id, accountID string) error
	ListAnnouncements(ctx context.Context, now time.Time) ([]*models.Announcement, error)

	// SearchStatuses performs a full-text search over status content (and CW
	// summary). Only public-ish statuses are returned, plus any authored by
	// viewerID (pass "" to restrict to public). accountID optionally restricts
	// results to a single author. Boosts are excluded.
	SearchStatuses(ctx context.Context, query string, accountID *string, viewerID string, offset, limit int) ([]*models.Status, error)

	// Housekeeping queries
	GetRemoteStatusesOlderThan(ctx context.Context, days int, limit int) ([]*models.Status, error)
	PruneExpiredData(ctx context.Context, now, remoteCutoff, metadataCutoff time.Time, limit int) (PruneExpiredDataResult, error)
	GetOrphanedRemoteAccounts(ctx context.Context, seenBefore time.Time, limit int) ([]*models.Account, error)
	// GetLocalStatusesOlderThanForAccount returns statuses for a specific account older than the given days
	// with fewer than minLikes likes and minBoosts boosts. Pass 0 for a threshold to disable it.
	GetLocalStatusesOlderThanForAccount(ctx context.Context, accountID string, days, minLikes, minBoosts, limit int) ([]*models.Status, error)

	// Follows

	CreateFollow(ctx context.Context, follow *models.Follow) error
	GetFollow(ctx context.Context, id string) (*models.Follow, error)
	GetFollowByPair(ctx context.Context, accountID, targetID string) (*models.Follow, error)
	GetFollowByURI(ctx context.Context, uri string) (*models.Follow, error)
	UpdateFollow(ctx context.Context, follow *models.Follow) error
	DeleteFollow(ctx context.Context, id string) error
	AcceptFollow(ctx context.Context, id string) error
	RejectFollow(ctx context.Context, id string) error

	ListFollowers(ctx context.Context, accountID string, maxID *string, limit int) ([]*models.Account, error)
	ListFollowing(ctx context.Context, accountID string, maxID *string, limit int) ([]*models.Account, error)
	ListFollowRequests(ctx context.Context, accountID string, limit int) ([]*models.Follow, error)
	CountFollowers(ctx context.Context, accountID string) (int, error)
	CountFollowing(ctx context.Context, accountID string) (int, error)
	IsFollowing(ctx context.Context, accountID, targetID string) (bool, error)
	// HasLocalFollowers reports whether at least one local account has an
	// accepted follow of accountID. The inbox uses it to decide whether an
	// inbound activity from that actor has any audience here at all.
	HasLocalFollowers(ctx context.Context, accountID string) (bool, error)
	// AnyTagFollowed reports whether any of the given hashtag names (with or
	// without leading '#', any case) is followed by a local account.
	AnyTagFollowed(ctx context.Context, names []string) (bool, error)

	// SetFollowNotify toggles the "bell": whether accountID receives a status
	// notification each time targetID publishes a new post.
	SetFollowNotify(ctx context.Context, accountID, targetID string, notify bool) error
	// ListNotifyFollowerIDs returns the IDs of local accounts that follow
	// targetAccountID with the notify bell enabled.
	ListNotifyFollowerIDs(ctx context.Context, targetAccountID string) ([]string, error)

	// GetFollowerDestinations returns one distinct effective remote destination
	// per follower server, preferring sharedInbox when advertised.
	GetFollowerDestinations(ctx context.Context, accountID string) ([]string, error)
	// ListLocalFollowers returns every local account with an accepted follow of
	// accountID, unpaginated (unlike ListFollowers, which is for API responses).
	// Used for Collection-Synchronization digest computation and reconciliation,
	// where a partial view would corrupt the hash or trigger a bogus unfollow.
	ListLocalFollowers(ctx context.Context, accountID string) ([]*models.Account, error)

	// LitePub relays

	CreateRelay(ctx context.Context, relay *models.Relay) error
	GetRelay(ctx context.Context, id string) (*models.Relay, error)
	GetRelayByAccountID(ctx context.Context, accountID string) (*models.Relay, error)
	GetRelayByActorURI(ctx context.Context, actorURI string) (*models.Relay, error)
	ListRelays(ctx context.Context) ([]*models.Relay, error)
	UpdateRelay(ctx context.Context, relay *models.Relay) error
	DeleteRelay(ctx context.Context, id string) error
	GetRelayState(ctx context.Context, id string) (models.RelayState, error)
	GetActiveRelayInboxes(ctx context.Context) ([]string, error)

	// Favourites

	CreateFavourite(ctx context.Context, fave *models.Favourite) error
	// The boolean reports a new interaction or a repaired/refreshed notification.
	CreateFavouriteWithNotification(ctx context.Context, fave *models.Favourite, notif *models.Notification, jobs []*models.Job) (bool, error)
	CreateBoostWithNotification(ctx context.Context, boost *models.Status, notif *models.Notification, jobs []*models.Job) (bool, error)
	DeleteFavourite(ctx context.Context, accountID, statusID string) error
	GetFavourite(ctx context.Context, accountID, statusID string) (*models.Favourite, error)
	ListFavourites(ctx context.Context, accountID string, maxID *string, limit int) ([]*models.Status, error)
	ListFavouriteAccounts(ctx context.Context, statusID string, limit int) ([]*models.Account, error)
	ListReblogAccounts(ctx context.Context, statusID string, limit int) ([]*models.Account, error)
	GetFavouriteByURI(ctx context.Context, uri string) (*models.Favourite, error)

	// Emoji reactions
	CreateEmojiReaction(ctx context.Context, reaction *models.EmojiReaction) (bool, error)
	GetEmojiReaction(ctx context.Context, accountID, statusID, key string) (*models.EmojiReaction, error)
	GetEmojiReactionByURI(ctx context.Context, uri string) (*models.EmojiReaction, error)
	DeleteEmojiReaction(ctx context.Context, accountID, statusID, key string) (*models.EmojiReaction, error)
	DeleteEmojiReactionByURI(ctx context.Context, accountID, uri string) (*models.EmojiReaction, error)
	GetEmojiReactionSummaries(ctx context.Context, statusIDs []string, viewerID string, accountLimit int) (map[string][]models.EmojiReactionSummary, error)
	ListEmojiReactions(ctx context.Context, statusID string, limit int) ([]*models.EmojiReaction, error)
	ListEmojiReactionAccounts(ctx context.Context, statusID, key string, limit int) ([]*models.Account, error)
	// CountEmojiReactions returns reaction totals for the requested statuses.
	CountEmojiReactions(ctx context.Context, statusIDs []string) (map[string]int, error)

	// Media

	CreateMediaAttachment(ctx context.Context, media *models.MediaAttachment) error
	GetMediaAttachment(ctx context.Context, id string) (*models.MediaAttachment, error)
	GetMediaForStatus(ctx context.Context, statusID string) ([]*models.MediaAttachment, error)
	GetMediaForAccount(ctx context.Context, accountID string) ([]*models.MediaAttachment, error)
	UpdateMediaAttachment(ctx context.Context, media *models.MediaAttachment) error
	DeleteMediaAttachment(ctx context.Context, id string) error
	GetUnattachedMedia(ctx context.Context, accountID string, olderThan time.Duration) ([]*models.MediaAttachment, error)
	// GetAllMedia returns all media attachments (for cleanup/reference checking)
	GetAllMedia(ctx context.Context) ([]*models.MediaAttachment, error)
	ListOwnedMediaAfter(ctx context.Context, cursor, destinationBackend string, limit int) ([]*models.MediaAttachment, error)
	CreateMediaStorageMigration(ctx context.Context, migration *models.MediaStorageMigration) error
	GetMediaStorageMigration(ctx context.Context, id string) (*models.MediaStorageMigration, error)
	GetActiveMediaStorageMigration(ctx context.Context) (*models.MediaStorageMigration, error)
	UpdateMediaStorageMigration(ctx context.Context, migration *models.MediaStorageMigration) error
	InvalidateRemoteMediaCache(ctx context.Context) (int64, error)
	UpdateLocalMediaReferences(ctx context.Context, oldURL, newURL string) error
	// GetOrphanedMediaAttachments returns media whose status_id points to a non-existent status
	GetOrphanedMediaAttachments(ctx context.Context) ([]*models.MediaAttachment, error)
	// GetMediaAttachmentsByRemoteURL returns all media rows whose remote_url matches.
	GetMediaAttachmentsByRemoteURL(ctx context.Context, remoteURL string) ([]*models.MediaAttachment, error)
	// GetUncachedRemoteMedia returns remote media not yet cached locally (for the eager sweep).
	GetUncachedRemoteMedia(ctx context.Context, limit int) ([]*models.MediaAttachment, error)
	// GetCachedMediaOlderThan returns cached remote media whose cached_at is before cutoff (for pruning).
	GetCachedMediaOlderThan(ctx context.Context, cutoff time.Time, limit int) ([]*models.MediaAttachment, error)
	// CachedMediaBytes totals the recorded size of cached remote media, which is
	// what a size-bounded cache measures itself against. Owned uploads are not
	// included.
	CachedMediaBytes(ctx context.Context) (int64, error)
	// CountMediaAttachmentsByURL counts media rows currently pointing at the given local URL.
	CountMediaAttachmentsByURL(ctx context.Context, url string) (int, error)
	// GetAccountsByImageURL returns accounts whose avatar or header equals the given URL.
	GetAccountsByImageURL(ctx context.Context, url string) ([]*models.Account, error)
	// GetUncachedRemoteAccountImageURLs returns distinct remote avatar/header URLs not yet cached.
	GetUncachedRemoteAccountImageURLs(ctx context.Context, limit int) ([]string, error)
	// GetReferencedCachePaths returns the set of /media/cache/ URLs still referenced by any row.
	GetReferencedCachePaths(ctx context.Context) (map[string]struct{}, error)
	// ReconcileMissingCacheURL handles a cache URL whose file is missing: it resets
	// every row referencing that URL back to its remote origin (so it is proxied
	// and re-cached) and returns that origin URL ("" if none was found).
	ReconcileMissingCacheURL(ctx context.Context, cacheURL string) (string, error)
	// RecordMediaCacheFailure records that a remote URL could not be cached, so the
	// eager sweep and lazy proxy hook stop re-enqueueing it until the entry expires.
	RecordMediaCacheFailure(ctx context.Context, url, reason string) error
	// MediaCacheFailedSince reports whether a non-cacheable failure for the URL was
	// recorded at or after cutoff (i.e. is still within the retry window).
	MediaCacheFailedSince(ctx context.Context, url string, cutoff time.Time) (bool, error)
	// PruneMediaCacheFailures deletes failure entries older than cutoff and returns
	// how many were removed.
	PruneMediaCacheFailures(ctx context.Context, cutoff time.Time) (int64, error)

	// Custom emojis
	// UpsertEmoji inserts or refreshes a custom emoji keyed by (shortcode, domain).
	UpsertEmoji(ctx context.Context, emoji *models.CustomEmoji) error
	// GetEmojisByShortcodes returns emoji rows matching any of the shortcodes for
	// the given domain (nil = local).
	GetEmojisByShortcodes(ctx context.Context, shortcodes []string, domain *string) ([]*models.CustomEmoji, error)
	// GetUncachedRemoteEmoji returns remote emoji not yet cached locally (for the eager sweep).
	GetUncachedRemoteEmoji(ctx context.Context, limit int) ([]*models.CustomEmoji, error)
	// GetEmojisByRemoteURL returns emoji rows whose remote_url matches.
	GetEmojisByRemoteURL(ctx context.Context, remoteURL string) ([]*models.CustomEmoji, error)
	// UpdateEmoji persists changes to an existing emoji row.
	UpdateEmoji(ctx context.Context, emoji *models.CustomEmoji) error
	// ListLocalCustomEmojis returns every locally-hosted emoji (domain IS NULL).
	ListLocalCustomEmojis(ctx context.Context) ([]*models.CustomEmoji, error)
	// SearchEmojis returns emoji rows whose shortcode starts with query (prefix
	// match), ordered local-first then alphabetically.
	SearchEmojis(ctx context.Context, query string, offset, limit int) ([]*models.CustomEmoji, error)

	// Notifications

	CreateNotification(ctx context.Context, notif *models.Notification) error
	// CreateNotificationOnce atomically inserts a notification and its durable
	// delivery jobs. A duplicate deduplication key is left untouched and returns
	// created=false, so retries cannot resurrect a read notification.
	CreateNotificationOnce(ctx context.Context, notif *models.Notification, jobs []*models.Job) (created bool, err error)
	FindExistingNotification(ctx context.Context, accountID, fromAccountID, notifType string, statusID *string) (*models.Notification, error)
	// UpsertNotification creates a notification or refreshes an existing one
	// (marks it unread and bumps created_at) if a match exists by dedup key.
	UpsertNotification(ctx context.Context, notif *models.Notification) error
	// UpsertNotifications commits a batch atomically. Returned objects have the
	// winning ID/API ID populated and are safe to stream after the call returns.
	UpsertNotifications(ctx context.Context, notifs []*models.Notification) error
	GetNotification(ctx context.Context, id string) (*models.Notification, error)
	ListNotifications(ctx context.Context, accountID string, maxID *string, limit int, excludeTypes []string) ([]*models.Notification, error)
	// ListNotificationsSince returns notifications newer than sinceID (newest
	// first, capped at limit) - used to fetch what arrived while a viewer had
	// the notifications page open, for prepending without a full reload.
	ListNotificationsSince(ctx context.Context, accountID string, sinceID string, limit int, excludeTypes []string) ([]*models.Notification, error)
	MarkNotificationRead(ctx context.Context, id string) error
	MarkNotificationsRead(ctx context.Context, accountID string, ids []string) error
	DeleteNotification(ctx context.Context, accountID, id string) error
	MarkAllNotificationsRead(ctx context.Context, accountID string) error
	DeleteAllNotifications(ctx context.Context, accountID string) error
	CountUnreadNotifications(ctx context.Context, accountID string) (int, error)
	CountUnreadNotificationsFiltered(ctx context.Context, accountID string, excludeTypes []string) (int, error)

	// Tags

	GetOrCreateTag(ctx context.Context, name string) (*models.Tag, error)
	GetTag(ctx context.Context, id string) (*models.Tag, error)
	GetTagByName(ctx context.Context, name string) (*models.Tag, error)
	GetTagTimeline(ctx context.Context, viewerID, tag string, maxID, minID, sinceID *string, limit int, excludeReplies, excludeReblogs, localOnly bool) ([]*models.Status, error)
	IncrementTagCount(ctx context.Context, tagID string) error
	// SearchTags finds hashtags by name substring, ranked by exact/prefix match
	// then popularity (statuses_count). SearchTagPrefixes is the indexed,
	// prefix-only composer autocomplete path.
	SearchTags(ctx context.Context, query string, offset, limit int) ([]*models.Tag, error)
	SearchTagPrefixes(ctx context.Context, query string, offset, limit int) ([]*models.Tag, error)

	// Followed (subscribed) tags. Following a tag is local-only: it surfaces
	// statuses already known to this instance into the account's home timeline.
	FollowTag(ctx context.Context, accountID, tagID string) error
	UnfollowTag(ctx context.Context, accountID, tagID string) error
	IsFollowingTag(ctx context.Context, accountID, tagID string) (bool, error)
	ListFollowedTags(ctx context.Context, accountID string) ([]*models.Tag, error)
	GetFollowedTagIDs(ctx context.Context, accountID string) ([]string, error)

	// CreateFeaturedTag pins a tag to an account's profile. Returns
	// ErrFeaturedTagExists if the pair is already featured.
	CreateFeaturedTag(ctx context.Context, accountID, tagID string) (*models.FeaturedTag, error)
	// DeleteFeaturedTag unpins a featured tag by its own id, scoped to the
	// owning account. Returns false if no row matched (wrong id or not owner).
	DeleteFeaturedTag(ctx context.Context, accountID, featuredTagID string) (bool, error)
	// ListFeaturedTags returns an account's featured tags joined with each
	// tag's name/url/statuses_count and that account's own last use of it.
	ListFeaturedTags(ctx context.Context, accountID string) ([]*models.FeaturedTagView, error)

	// Mentions

	CreateMention(ctx context.Context, mention *models.Mention) error
	CreateMentions(ctx context.Context, mentions []*models.Mention) error
	GetMentionsForStatus(ctx context.Context, statusID string) ([]*models.Mention, error)
	GetTagsForStatus(ctx context.Context, statusID string) ([]*models.Tag, error)

	// Blocks

	CreateBlock(ctx context.Context, block *models.Block) error
	DeleteBlock(ctx context.Context, accountID, targetID string) error
	// BlockExists reports whether accountID has blocked targetID (one direction only).
	BlockExists(ctx context.Context, accountID, targetID string) (bool, error)
	ListBlocks(ctx context.Context, accountID string, maxID *string, limit int) ([]*models.Account, error)
	// BlockedAccountIDs returns the IDs of accounts on either side of a block
	// relationship with accountID (accounts it blocks and accounts blocking it).
	BlockedAccountIDs(ctx context.Context, accountID string) ([]string, error)

	// Mutes

	CreateMute(ctx context.Context, mute *models.Mute) error
	DeleteMute(ctx context.Context, accountID, targetID string) error
	IsMuted(ctx context.Context, accountID, targetID string) (bool, error)
	ListMutes(ctx context.Context, accountID string, maxID *string, limit int) ([]*models.Account, error)
	// MutedAccountIDs returns the target IDs of the account's active (unexpired) mutes.
	MutedAccountIDs(ctx context.Context, accountID string) ([]string, error)
	// DeleteExpiredMutes removes mutes whose duration has elapsed and returns
	// the muting accounts' IDs so callers can invalidate only those viewers'
	// caches. Called by housekeeping.
	DeleteExpiredMutes(ctx context.Context) ([]string, error)

	// Markers

	// GetMarkers returns the account's stored timeline markers, optionally
	// filtered to the given timeline names ("home", "notifications").
	GetMarkers(ctx context.Context, accountID string, timelines []string) ([]*models.Marker, error)
	// SetMarker upserts a timeline marker, bumping its version.
	SetMarker(ctx context.Context, accountID, timeline, lastReadID string) (*models.Marker, error)

	// Catch-up ranking

	// GetCatchupAffinity summarizes the account's own recent activity (the
	// authors it favourited, boosted or replied to, and the statuses it replied
	// to) since the given time. Each signal is capped at limit rows so the
	// cost stays bounded on a busy account.
	GetCatchupAffinity(ctx context.Context, accountID string, since time.Time, limit int) (*models.CatchupAffinity, error)

	// Domain Blocks

	CreateDomainBlock(ctx context.Context, block *models.DomainBlock) error
	ImportDomainBlocks(ctx context.Context, blocks []*models.DomainBlock) (int, error)
	DeleteDomainBlock(ctx context.Context, id string) error
	GetDomainBlock(ctx context.Context, domain string) (*models.DomainBlock, error)
	GetDomainBlockByID(ctx context.Context, id string) (*models.DomainBlock, error)
	ListDomainBlocks(ctx context.Context) ([]*models.DomainBlock, error)
	CreateDomainAllow(ctx context.Context, allow *models.DomainAllow) error
	ImportDomainAllows(ctx context.Context, allows []*models.DomainAllow) (int, error)
	DeleteDomainAllow(ctx context.Context, id string) error
	GetDomainAllow(ctx context.Context, id string) (*models.DomainAllow, error)
	ListDomainAllows(ctx context.Context) ([]*models.DomainAllow, error)
	GetFederationPolicyState(ctx context.Context) (*models.FederationPolicyState, error)
	SetFederationMode(ctx context.Context, mode string) error
	SetCommercialBigTechEnabled(ctx context.Context, enabled bool) error
	AcquireFederationPolicyLease(ctx context.Context, until time.Time) (bool, error)
	ReleaseFederationPolicyLease(ctx context.Context) error
	UpdateFederationPolicySync(ctx context.Context, state *models.FederationPolicyState) error
	ReplaceCommercialDomainBlocks(ctx context.Context, blocks []*models.CommercialDomainBlock, state *models.FederationPolicyState) error
	ListCommercialDomainBlocks(ctx context.Context) ([]*models.CommercialDomainBlock, error)
	FederationPolicyMode(ctx context.Context) (string, error)
	FederationDomainAllowed(ctx context.Context, domain string) (bool, string, error)
	InvalidateFederationPolicy()
	// InboundHostBlocksPossible reports whether any suspend-severity domain
	// block or suspended instance exists at all. The inbox checks it before the
	// per-host lookups so unmoderated instances (the common case) pay a cached
	// flag read per activity instead of two queries.
	InboundHostBlocksPossible(ctx context.Context) (bool, error)

	// Per-user Domain Blocks

	CreateAccountDomainBlock(ctx context.Context, accountID, domain string) error
	DeleteAccountDomainBlock(ctx context.Context, accountID, domain string) error
	ListAccountDomainBlocks(ctx context.Context, accountID string) ([]string, error)

	// Moderation Audit Log

	CreateModerationLog(ctx context.Context, entry *models.ModerationLog) error
	ListModerationLog(ctx context.Context, maxID *string, limit int) ([]*models.ModerationLog, error)

	// Reports

	CreateReport(ctx context.Context, report *models.Report) error
	GetReport(ctx context.Context, id string) (*models.Report, error)
	// ListReports returns reports filtered by state ("" = all), newest first.
	ListReports(ctx context.Context, state string, maxID *string, limit int) ([]*models.Report, error)
	ResolveReport(ctx context.Context, id, adminID, action string) error
	CountOpenReports(ctx context.Context) (int, error)

	// Instances

	CreateInstance(ctx context.Context, instance *models.Instance) error
	GetInstance(ctx context.Context, domain string) (*models.Instance, error)
	GetInstanceByID(ctx context.Context, id string) (*models.Instance, error)
	UpdateInstance(ctx context.Context, instance *models.Instance) error
	ListInstances(ctx context.Context) ([]*models.Instance, error)
	CountDomains(ctx context.Context) (int, error)
	MarkInstanceUnreachable(ctx context.Context, domain string) (bool, error) // returns true if just became unreachable
	MarkInstanceReachable(ctx context.Context, domain string) error
	IsInstanceReachable(ctx context.Context, domain string) (bool, error)
	IsInstanceMediaCacheable(ctx context.Context, domain string) (bool, error)
	CountUnreachableInstances(ctx context.Context) (int, error)
	SuspendInstance(ctx context.Context, id string, suspended bool) error
	DeleteInstanceData(ctx context.Context, id string) error
	CountFollowersByDomain(ctx context.Context, domain string) (int, error)
	CountFollowingByDomain(ctx context.Context, domain string) (int, error)

	// OAuth

	CreateOAuthApp(ctx context.Context, app *models.OAuthApplication) error
	GetOAuthAppByClientID(ctx context.Context, clientID string) (*models.OAuthApplication, error)
	GetOAuthApp(ctx context.Context, id string) (*models.OAuthApplication, error)
	CreateOAuthToken(ctx context.Context, token *models.OAuthToken) error
	GetOAuthTokenByAccess(ctx context.Context, accessToken string) (*models.OAuthToken, error)
	GetOAuthTokenByRefresh(ctx context.Context, refreshToken string) (*models.OAuthToken, error)
	// RotateOAuthToken replaces a token's access/refresh secrets in place (used
	// by the refresh_token grant) so the row's id, scopes and account binding
	// survive the rotation instead of accumulating a new row per refresh.
	RotateOAuthToken(ctx context.Context, tokenID, refreshToken, newAccessToken, newRefreshToken string) error
	ConsumeOAuthAuthorizationCode(ctx context.Context, code string) (bool, error)
	RevokeOAuthToken(ctx context.Context, accessToken string) error
	RevokeOAuthTokensForAccount(ctx context.Context, accountID string) error

	// Jobs / Queue

	EnqueueJob(ctx context.Context, job *models.Job) error
	// EnqueueJobs inserts a batch of jobs in one statement (used by delivery
	// fan-out); nil/empty input is a no-op.
	EnqueueJobs(ctx context.Context, jobs []*models.Job) error
	DequeueJob(ctx context.Context, kinds []string) (*models.Job, error)
	CompleteJob(ctx context.Context, id string) error
	FailJob(ctx context.Context, id string, err error) error
	RequeueJob(ctx context.Context, id string, runAt time.Time) error
	// YieldJob returns a cooperatively preempted job to the queue without
	// consuming an attempt.
	YieldJob(ctx context.Context, id string, runAt time.Time) error
	CountPendingJobs(ctx context.Context) (int, error)
	GetQueueStats(ctx context.Context, now, staleBefore time.Time) (*models.QueueStats, error)
	ListActiveQueueJobs(ctx context.Context, limit int) ([]models.Job, error)
	ListQueueProblemJobs(ctx context.Context, limit int) ([]models.QueueProblemJob, error)
	DeleteQueueProblemJob(ctx context.Context, id string) (bool, error)
	RecoverStaleJobs(ctx context.Context, takenBefore time.Time) (int, error)
	DeleteTerminalJobsBefore(ctx context.Context, updatedBefore time.Time, limit int) (int, error)
	// CreateJobPayload stores a body shared by many jobs (delivery fan-out)
	// and returns its ID; jobs reference it instead of embedding the body.
	CreateJobPayload(ctx context.Context, body string) (string, error)
	GetJobPayload(ctx context.Context, id string) (string, error)
	// DeleteJobPayloadsBefore prunes shared payload rows older than the given
	// time. Callers must pass a horizon comfortably past the delivery retry
	// window so no queued job can still reference a pruned row.
	DeleteJobPayloadsBefore(ctx context.Context, createdBefore time.Time) (int, error)
	// ScheduleSelfDestruct creates the one durable shutdown fan-out, if absent.
	// It returns the persisted run and whether this call created it.
	ScheduleSelfDestruct(ctx context.Context) (*models.SelfDestructRun, bool, error)
	GetSelfDestructRun(ctx context.Context) (*models.SelfDestructRun, error)
	ScheduleAccountDeletion(ctx context.Context, accountID string, deadline time.Time) (*models.AccountDeletionRun, bool, error)
	GetAccountDeletionRun(ctx context.Context, id string) (*models.AccountDeletionRun, error)
	GetAccountDeletionDelivery(ctx context.Context, runID, destinationID string) (*models.AccountDeletionRun, *models.AccountDeletionDestination, error)
	MarkAccountDeletionDelivered(ctx context.Context, runID, inbox string) error
	CompleteAccountDeletion(ctx context.Context, runID string) error
	CreateAccountExport(ctx context.Context, export *models.AccountExport) error
	GetAccountExport(ctx context.Context, id, accountID string) (*models.AccountExport, error)
	GetLatestAccountExport(ctx context.Context, accountID string) (*models.AccountExport, error)
	ListAccountExports(ctx context.Context, accountID string) ([]*models.AccountExport, error)
	BuildAccountExportSnapshot(ctx context.Context, accountID string) (*models.AccountExportSnapshot, error)
	CompleteAccountExport(ctx context.Context, id, storageKey string, size int64, expiresAt time.Time) error
	RecordAccountExportError(ctx context.Context, id, message string) error
	DeleteAccountExport(ctx context.Context, id, accountID string) error
	SetAccountExportDownloadToken(ctx context.Context, id, accountID, tokenHash string) error
	ConsumeAccountExportDownload(ctx context.Context, id, accountID, tokenHash string, now time.Time) (*models.AccountExport, error)
	ExpireAccountExport(ctx context.Context, id string) error

	CreateAccountImport(ctx context.Context, imp *models.AccountImport) error
	CreateFollowImport(ctx context.Context, imp *models.AccountImport, blocks []string, job *models.Job) error
	AdvanceFollowImport(ctx context.Context, imp *models.AccountImport, expectedResult string, follow *models.Follow, jobs []*models.Job) (bool, error)
	GetAccountImport(ctx context.Context, id, accountID string) (*models.AccountImport, error)
	GetLatestAccountImport(ctx context.Context, accountID string) (*models.AccountImport, error)
	ListAccountImports(ctx context.Context, accountID string) ([]*models.AccountImport, error)
	StartAccountImport(ctx context.Context, id string) error
	CompleteAccountImport(ctx context.Context, id, resultJSON string) error
	RecordAccountImportError(ctx context.Context, id, message string) error
	DeleteAccountImport(ctx context.Context, id, accountID string) error

	// Notifications

	// Delete all notifications for a given status (used when a status is deleted)
	DeleteNotificationsForStatus(ctx context.Context, statusID string) error

	// Delete all notifications of a given type from a given account
	DeleteNotifications(ctx context.Context, accountID, fromAccountID string, notifType string) error

	// Admin / User Management

	SetAdmin(ctx context.Context, accountID string, isAdmin bool) error
	GetAdmins(ctx context.Context) ([]*models.Account, error)
	SetBlogAllowed(ctx context.Context, accountID string, allowed bool) error
	CountLocalAccounts(ctx context.Context) (int, error)             // human local accounts only
	GetLocalAccounts(ctx context.Context) ([]*models.Account, error) // all human local accounts
	// GetAllLocalAccounts also returns the instance actor, which the human
	// listing hides but which owns a signing key like any other account.
	GetAllLocalAccounts(ctx context.Context) ([]*models.Account, error)
	PurgeRetiredPrivateKeyData(ctx context.Context) error
	GetWelcomeDirectMessage(ctx context.Context) (*models.WelcomeDirectMessage, error)
	SetWelcomeDirectMessage(ctx context.Context, senderAccountID, content string) error
	DeleteWelcomeDirectMessage(ctx context.Context) error

	// Instance profile (singleton). GetInstanceMeta returns (nil, nil) when no
	// administrator has saved one yet, which is what tells callers to fall back
	// to the [instance] config section.
	GetInstanceMeta(ctx context.Context) (*models.InstanceMeta, error)
	SaveInstanceMeta(ctx context.Context, meta *models.InstanceMeta) error

	// ListLocalIdentities returns every account with a nil Domain, including
	// the service actor - a domain-filtered variant of ListAccounts for
	// startup identity checks that must not pull the whole (remote-actor-
	// dominated) accounts table into memory just to inspect the handful of
	// local rows.
	ListLocalIdentities(ctx context.Context) ([]*models.Account, error)

	// Invites

	CreateInvite(ctx context.Context, invite *models.Invite) error
	GetInviteByCode(ctx context.Context, code string) (*models.Invite, error)
	IncrementInviteUse(ctx context.Context, id string) error
	DecrementInviteUse(ctx context.Context, id string) error
	ListInvites(ctx context.Context) ([]*models.Invite, error)
	DeleteInvite(ctx context.Context, id string) error

	// Bubble Instances

	CreateBubbleInstance(ctx context.Context, b *models.BubbleInstance) error
	DeleteBubbleInstance(ctx context.Context, id string) error
	ListBubbleInstances(ctx context.Context) ([]*models.BubbleInstance, error)

	// Bubble Timeline

	GetBubbleTimeline(ctx context.Context, viewerID string, maxID, minID, sinceID *string, limit int, excludeReplies, excludeReblogs bool) ([]*models.Status, error)

	// Password Resets

	CreatePasswordReset(ctx context.Context, reset *models.PasswordReset) error
	GetPasswordResetByTokenHash(ctx context.Context, tokenHash string) (*models.PasswordReset, error)
	MarkPasswordResetUsed(ctx context.Context, id string) error

	// Registration / Approval

	GetPendingApprovals(ctx context.Context) ([]*models.Account, error)
	ApproveAccount(ctx context.Context, id string) error
	RejectAccount(ctx context.Context, id string) error

	// Email

	GetAccountByEmail(ctx context.Context, email string) (*models.Account, error)

	CreateEmailVerification(ctx context.Context, v *models.EmailVerification) error
	VerifyEmail(ctx context.Context, tokenHash string, now time.Time) (*models.Account, error)
	SetAccountMFA(ctx context.Context, accountID, encryptedSecret string, recoveryCodeHashes []string) error
	ReplaceMFARecoveryCodes(ctx context.Context, accountID string, recoveryCodeHashes []string) error
	DisableAccountMFA(ctx context.Context, accountID string) error
	ConsumeMFARecoveryCode(ctx context.Context, accountID, codeHash string) (bool, error)

	UpsertEmailDigestState(ctx context.Context, accountID string, sentAt time.Time) error
	GetEmailDigestDue(ctx context.Context, before time.Time) ([]*models.Account, error)

	// Push Subscriptions

	CreatePushSubscription(ctx context.Context, sub *models.PushSubscription) error
	GetPushSubscription(ctx context.Context, id string) (*models.PushSubscription, error)
	// ListPushSubscriptionsByAccountID returns every app/device subscription.
	ListPushSubscriptionsByAccountID(ctx context.Context, accountID string) ([]*models.PushSubscription, error)
	ListPushSubscriptionsByAccountIDs(ctx context.Context, accountIDs []string) (map[string][]*models.PushSubscription, error)
	// GetPushSubscriptionByOAuthTokenID returns the subscription created by a
	// specific OAuth token, if any.
	GetPushSubscriptionByOAuthTokenID(ctx context.Context, oauthTokenID string) (*models.PushSubscription, error)
	// GetPushSubscriptionBySessionID returns the current browser session's PWA subscription.
	GetPushSubscriptionBySessionID(ctx context.Context, sessionID string) (*models.PushSubscription, error)
	GetPushSubscriptionByEndpoint(ctx context.Context, endpoint string) (*models.PushSubscription, error)
	UpdatePushSubscription(ctx context.Context, sub *models.PushSubscription) error
	DeletePushSubscription(ctx context.Context, id string) error
	// GetOrCreateVAPIDKeyPair stores candidate only when no singleton exists.
	GetOrCreateVAPIDKeyPair(ctx context.Context, candidate *models.VAPIDKeyPair) (*models.VAPIDKeyPair, error)
	// ReplaceVAPIDKeyPair applies configured keys and clears subscriptions when they rotate.
	ReplaceVAPIDKeyPair(ctx context.Context, pair *models.VAPIDKeyPair) (bool, error)

	// GetOrCreateMeshIdentity stores candidate only when no littleMesh
	// identity exists yet, and always returns the identity in force. The
	// insert-then-read shape makes concurrent starts converge on one key
	// instead of silently minting a second node ID.
	GetOrCreateMeshIdentity(ctx context.Context, candidate *models.MeshIdentity) (*models.MeshIdentity, error)

	// Pinned Statuses

	GetPinnedStatuses(ctx context.Context, accountID string, limit int) ([]*models.Status, error)
	PinStatus(ctx context.Context, statusID string) error
	UnpinStatus(ctx context.Context, statusID string) error

	// Bookmarks

	CreateBookmark(ctx context.Context, bookmark *models.Bookmark) error
	DeleteBookmark(ctx context.Context, accountID, statusID string) error
	GetBookmark(ctx context.Context, accountID, statusID string) (*models.Bookmark, error)
	ListBookmarks(ctx context.Context, accountID string, maxID *string, limit int) ([]*models.Status, error)

	// GetDirectTimeline returns direct-visibility statuses the account is part of
	// (as author or mentioned recipient), newest first.
	GetDirectTimeline(ctx context.Context, accountID string, maxID, minID, sinceID *string, limit int) ([]*models.Status, error)

	// Status Tags (pivot)

	CreateStatusTags(ctx context.Context, statusTags []*models.StatusTag) error
	// DeleteStatusTags removes a status's tag pivots and adjusts tag usage counts.
	DeleteStatusTags(ctx context.Context, statusID string) error
	// SyncStatusTags replaces a status's hashtag pivot with exactly the given
	// tags, adjusting tag usage counts for added/removed tags. Used when a
	// remote edit changes a post's hashtags. It reports whether the tag set
	// actually changed so callers can skip cache invalidation when an edit
	// (or a poll tally refresh delivered as Update) left the tags untouched.
	SyncStatusTags(ctx context.Context, statusID string, tags []*models.Tag) (bool, error)
	// SyncStatusMentions replaces a status's mentions with exactly the given set.
	SyncStatusMentions(ctx context.Context, statusID string, mentions []*models.Mention) error

	// Lists

	CreateList(ctx context.Context, list *models.List) error
	GetList(ctx context.Context, id string) (*models.List, error)
	UpdateList(ctx context.Context, list *models.List) error
	DeleteList(ctx context.Context, id string) error
	ListLists(ctx context.Context, accountID string) ([]*models.List, error)

	// ListAccountsForList returns the list's members ordered account_id DESC.
	// Cursors are account IDs (opaque to API clients); limit <= 0 returns all
	// members (Mastodon's limit=0 mode).
	ListAccountsForList(ctx context.Context, listID string, maxID, sinceID *string, limit int) ([]*models.Account, error)
	AddAccountToList(ctx context.Context, listID, accountID string) error
	RemoveAccountFromList(ctx context.Context, listID, accountID string) error
	// RemoveAccountFromAllLists removes memberID from every list ownerID owns
	// (unfollow hook: Mastodon drops list memberships on unfollow). Reports
	// whether any membership was actually removed.
	RemoveAccountFromAllLists(ctx context.Context, ownerID, memberID string) (bool, error)
	// ListListsContainingAccount returns the lists owned by ownerID that
	// contain memberID (GET /api/v1/accounts/:id/lists).
	ListListsContainingAccount(ctx context.Context, ownerID, memberID string) ([]*models.List, error)
	// ListsContainingAccount returns every list (any owner) containing
	// memberID; powers streaming fan-out and exclusive home suppression.
	ListsContainingAccount(ctx context.Context, memberID string) ([]*models.List, error)

	// GetListTimeline walks the owner's materialized home_feed restricted to
	// the list's members, applying the list's replies_policy. Depth is bounded
	// by home_feed retention (PruneHomeFeeds).
	GetListTimeline(ctx context.Context, list *models.List, maxID, minID, sinceID *string, limit int) ([]*models.Status, error)

	// Account Migration

	// GetFollowersToMigrate returns followers of an account that should receive a Move activity.
	// Excludes suspended/blocked accounts. Used for outgoing migration.
	GetFollowersToMigrate(ctx context.Context, accountID string, limit int) ([]*models.Account, error)

	// GetFollowingToMigrate returns accounts that the local user follows (for import).
	GetFollowingToMigrate(ctx context.Context, accountID string, limit int) ([]*models.Account, error)

	// SetAccountMoved marks an account as having moved to the given URI and
	// records the moved_at timestamp. Also locks the account.
	SetAccountMoved(ctx context.Context, accountID string, movedToURI string) error

	// SetAccountAlias sets the alsoKnownAs field on an account (used when we
	// receive a Move activity targeting a local account).
	SetAccountAlias(ctx context.Context, accountID string, alsoKnownAs string) error

	// MigrateFollower replaces a follow from followerID->oldTargetID with a follow
	// to followerID->newTargetID. If already following newTarget, just removes old follow.
	MigrateFollower(ctx context.Context, followerID, oldTargetID, newTargetID string) error

	// Quote Authorizations

	CreateQuoteAuthorization(ctx context.Context, qa *models.QuoteAuthorization) error
	GetQuoteAuthorizationByURI(ctx context.Context, uri string) (*models.QuoteAuthorization, error)
	GetQuoteAuthorizationForQuoting(ctx context.Context, quotingURI string) (*models.QuoteAuthorization, error)

	// Tombstones

	CreateTombstone(ctx context.Context, t *models.Tombstone) error
	CreateTombstones(ctx context.Context, tombstones []*models.Tombstone) error
	GetTombstoneByURI(ctx context.Context, uri string) (*models.Tombstone, error)
	DeleteTombstone(ctx context.Context, id string) error
	DeleteTombstoneByURI(ctx context.Context, uri string) error
	ListTombstonesOlderThan(ctx context.Context, since time.Time, limit int) ([]*models.Tombstone, error)

	// Sessions

	CreateSession(ctx context.Context, session *models.Session) error
	GetSessionByTokenHash(ctx context.Context, tokenHash string) (*models.Session, error)
	UpdateSessionExpiresAt(ctx context.Context, id string, expiresAt time.Time) error
	DeleteSession(ctx context.Context, id string) error
	DeleteSessionsForAccount(ctx context.Context, accountID string) error
	ListSessionsByAccount(ctx context.Context, accountID string) ([]*models.Session, error)
}

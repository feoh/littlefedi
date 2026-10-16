// Package models defines the core domain entities for littleFedi.
// Names borrowed from Mastodon's vocabulary for API mapping simplicity.
package models

import (
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

// Visibility represents a status's visibility level.
type Visibility string

const (
	VisibilityPublic        Visibility = "public"
	VisibilityUnlisted      Visibility = "unlisted"
	VisibilityPrivate       Visibility = "private"
	VisibilityDirect        Visibility = "direct"
	VisibilityLocal         Visibility = "local"
	VisibilityLocalUnlisted Visibility = "local_unlisted"
)

// FollowState represents the state of a follow relationship.
type FollowState string

const (
	FollowPending  FollowState = "pending"
	FollowAccepted FollowState = "accepted"
	FollowRejected FollowState = "rejected"
)

// Account represents either a local or remote user.
type Account struct {
	ID        string    `bun:"id,pk,type:text" json:"id"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`

	// Identity
	Username       string  `bun:"username,notnull,unique:idx_account_username_domain" json:"username"`
	Domain         *string `bun:"domain,unique:idx_account_username_domain" json:"domain"` // null = local
	ActorURI       string  `bun:"actor_uri,unique,notnull" json:"actor_uri"`
	InboxURI       string  `bun:"inbox_uri" json:"inbox_uri"`
	SharedInboxURI *string `bun:"shared_inbox_uri" json:"shared_inbox_uri"`
	OutboxURI      string  `bun:"outbox_uri" json:"outbox_uri"`
	FollowersURI   string  `bun:"followers_uri" json:"followers_uri"`
	FollowingURI   string  `bun:"following_uri" json:"following_uri"`

	// Keys (local only for private key)
	PublicKeyPem   string `bun:"public_key_pem,notnull" json:"public_key_pem"`
	PublicKeyID    string `bun:"public_key_id,notnull,default:''" json:"-"`
	PublicKeysJSON string `bun:"public_keys_json,notnull,default:'{}'" json:"-"`
	PrivateKeyPem  string `bun:"private_key_pem" json:"-"` // never exposed via API
	// Ed25519 pair used for Object Integrity Proofs (FEP-8b32) and ed25519
	// HTTP Signatures. Empty for remote actors and for local accounts that
	// predate ed25519 support; kept alongside the RSA pair (dual-key) so
	// legacy peers continue to verify HTTP Signatures.
	Ed25519PublicKeyPem  string `bun:"ed25519_public_key_pem,notnull,default:''" json:"ed25519_public_key_pem,omitempty"`
	Ed25519PublicKeyID   string `bun:"ed25519_public_key_id,notnull,default:''" json:"-"`
	Ed25519PrivateKeyPem string `bun:"ed25519_private_key_pem" json:"-"` // never exposed via API

	// Profile
	DisplayName  string `bun:"display_name" json:"display_name"`
	Note         string `bun:"note" json:"note"`               // bio in HTML
	NoteSource   string `bun:"note_source" json:"note_source"` // original markdown
	Avatar       string `bun:"avatar" json:"avatar"`
	AvatarRemote string `bun:"avatar_remote" json:"-"`
	Header       string `bun:"header" json:"header"`
	HeaderRemote string `bun:"header_remote" json:"-"`

	// Flags
	ManuallyApprovesFollowers bool `bun:"manually_approves_followers,notnull,default:false" json:"manually_approves_followers"`
	Locked                    bool `bun:"locked,notnull,default:false" json:"locked"` // same as above, mastodon compat
	// IsServiceActor distinguishes non-human local actors used for federation plumbing.
	IsServiceActor bool `bun:"is_service_actor,notnull,default:false" json:"-"`
	// IsBot marks the account as automated (AP type Service/Application), mastodon "bot" field.
	IsBot       bool       `bun:"is_bot,notnull,default:false" json:"bot"`
	SuspendedAt *time.Time `bun:"suspended_at" json:"suspended_at"`
	FetchedAt   *time.Time `bun:"fetched_at" json:"fetched_at"`
	// FirstSeenAt is when this row was inserted here. CreatedAt carries the
	// remote actor's self-declared "published" date, which is neither local
	// nor trustworthy (observed values range from 1970 to 2137), so retention
	// decisions must use this instead. Set once, never refreshed.
	FirstSeenAt time.Time `bun:"first_seen_at,notnull" json:"-"`
	// Actor refresh scheduling is persisted on the account rather than inferred
	// from UpdatedAt. This keeps the dispatcher query indexed and lets multiple
	// processes claim bounded work without a global sweep.
	ActorNextRefreshAt     *time.Time `bun:"actor_next_refresh_at" json:"-"`
	ActorRefreshLeaseUntil *time.Time `bun:"actor_refresh_lease_until" json:"-"`
	ActorRefreshFailures   int        `bun:"actor_refresh_failures,notnull,default:0" json:"-"`
	CountsFetchedAt        *time.Time `bun:"counts_fetched_at" json:"-"`

	// Local-only
	Email           string     `bun:"email" json:"-"`
	EmailVerified   bool       `bun:"email_verified,notnull,default:false" json:"-"`
	EmailVerifiedAt *time.Time `bun:"email_verified_at" json:"-"`
	PasswordHash    string     `bun:"password_hash" json:"-"`

	// Email notification preferences
	EmailNotifyMention          bool `bun:"email_notify_mention,notnull,default:false" json:"-"`
	EmailNotifyFollow           bool `bun:"email_notify_follow,notnull,default:false" json:"-"`
	EmailNotifyReblog           bool `bun:"email_notify_reblog,notnull,default:false" json:"-"`
	EmailNotifyFavourite        bool `bun:"email_notify_favourite,notnull,default:false" json:"-"`
	EmailNotifyPoll             bool `bun:"email_notify_poll,notnull,default:false" json:"-"`
	EmailNotifyFollowRequest    bool `bun:"email_notify_follow_request,notnull,default:false" json:"-"`
	EmailNotifyQuote            bool `bun:"email_notify_quote,notnull,default:false" json:"-"`
	EmailNotifyReport           bool `bun:"email_notify_report,notnull,default:true" json:"-"`
	EmailNotifyApproval         bool `bun:"email_notify_approval,notnull,default:true" json:"-"`
	EmailNotifyNewAccount       bool `bun:"email_notify_new_account,notnull,default:false" json:"-"`
	EmailNotifyInviteSignup     bool `bun:"email_notify_invite_signup,notnull,default:false" json:"-"`
	EmailNotifyDigest           bool `bun:"email_notify_digest,notnull,default:false" json:"-"`
	EmailNotifyBackupSuccess    bool `bun:"email_notify_backup_success,notnull,default:true" json:"-"`
	EmailNotifyBackupFailure    bool `bun:"email_notify_backup_failure,notnull,default:true" json:"-"`
	EmailNotifyInstance         bool `bun:"email_notify_instance,notnull,default:false" json:"-"`
	EmailNotifyBlogRebuild      bool `bun:"email_notify_blog_rebuild,notnull,default:false" json:"-"`
	EmailNotifyAccountActivity  bool `bun:"email_notify_account_activity,notnull,default:false" json:"-"`
	EmailNotifyAccountDeparture bool `bun:"email_notify_account_departure,notnull,default:false" json:"-"`

	// Admin / moderation flags
	IsAdmin    bool       `bun:"is_admin,notnull,default:false" json:"-"`
	Approved   bool       `bun:"approved,notnull,default:true" json:"-"`
	ApprovedAt *time.Time `bun:"approved_at" json:"-"`
	InviteID   *string    `bun:"invite_id" json:"-"`
	// BlogAllowed is the admin-controlled per-account permission for the
	// optional static blog feature (in addition to the instance-wide
	// [blog] enabled switch). Defaults false: an admin must opt each
	// account in.
	BlogAllowed bool `bun:"blog_allowed,notnull,default:false" json:"-"`
	// BlogSlugFormat overrides the instance-wide blog permalink template for
	// this account (empty = use the instance default). Honoured only while
	// [blog] allow_user_slug_format is on.
	BlogSlugFormat string `bun:"blog_slug_format,notnull,default:''" json:"-"`
	// BlogRepliesEnabled includes known public federated replies in static posts.
	BlogRepliesEnabled bool `bun:"blog_replies_enabled,notnull,default:true" json:"-"`
	// BlogTheme and BlogCustomCSS style the generated static site. They are
	// baked into its style.css at build time, so changing either one needs a
	// full rebuild (a single-post rebuild never rewrites the stylesheet).
	// Empty BlogTheme means the stock blog look. Validated on write.
	BlogTheme     string `bun:"blog_theme,notnull,default:''" json:"-"`
	BlogCustomCSS string `bun:"blog_custom_css,notnull,default:''" json:"-"`
	// ModerationState is "" (active), "silenced", "quiet", "suspended" or "disabled".
	// SuspendedAt mirrors the "suspended" state for backwards compatibility.
	ModerationState string `bun:"moderation_state,notnull,default:''" json:"-"`
	ModerationNote  string `bun:"moderation_note,notnull,default:''" json:"-"`
	// DeletionState is empty or "delivering". The account and signing key stay
	// present while its durable Delete(Person) fan-out is retryable.
	DeletionState       string     `bun:"deletion_state,notnull,default:''" json:"-"`
	DeletionRequestedAt *time.Time `bun:"deletion_requested_at" json:"-"`
	MFAEnabled          bool       `bun:"mfa_enabled,notnull,default:false" json:"-"`
	TOTPSecretEncrypted string     `bun:"totp_secret_encrypted" json:"-"`

	// Lists
	FollowersCount int `bun:"followers_count,notnull,default:0" json:"followers_count"`
	FollowingCount int `bun:"following_count,notnull,default:0" json:"following_count"`
	StatusesCount  int `bun:"statuses_count,notnull,default:0" json:"statuses_count"`

	// Privacy settings: hide follower/following info from profile
	HideFollowers bool `bun:"hide_followers,notnull,default:false" json:"-"`
	HideFollowing bool `bun:"hide_following,notnull,default:false" json:"-"`

	// Timeline preferences
	HideReplies         bool `bun:"hide_replies,notnull,default:false" json:"-"`
	HideBoosts          bool `bun:"hide_boosts,notnull,default:false" json:"-"`
	DataSaver           bool `bun:"data_saver,notnull,default:false" json:"-"`
	HidePostsWithoutAlt bool `bun:"hide_posts_without_alt,notnull,default:false" json:"-"`

	// Wellness preferences for the web interface.
	HideFavouriteCounts bool `bun:"hide_favourite_counts,notnull,default:false" json:"-"`
	HideBoostCounts     bool `bun:"hide_boost_counts,notnull,default:false" json:"-"`
	HideFollowerCounts  bool `bun:"hide_follower_counts,notnull,default:false" json:"-"`

	// Notification page preferences. Excluded types are stored as a comma-separated
	// list of Mastodon notification type names; an empty list shows every type.
	GroupNotifications           bool   `bun:"group_notifications,notnull,default:false" json:"-"`
	NotificationExcludedTypes    string `bun:"notification_excluded_types,notnull,default:''" json:"-"`
	NotifySensitiveRelationships bool   `bun:"notify_sensitive_relationships,notnull,default:false" json:"-"`

	// QuotePolicy governs who may quote this account's posts: everyone,
	// followers, or nobody. Enforced when granting a QuoteAuthorization.
	QuotePolicy string `bun:"quote_policy,notnull,default:'everyone'" json:"-"`

	// Locale is the account's preferred UI/email/push language (a short code
	// like "en"/"it" from i18n.SupportedLocales()). Validated on write.
	Locale string `bun:"locale,notnull,default:'en'" json:"-"`

	// Theme and Appearance select the web UI's skin and its light/dark mode.
	// Empty means "follow the instance default" (config Web.DefaultTheme /
	// Web.DefaultAppearance), so a server can restyle itself for accounts
	// that never expressed a preference. Validated on write.
	Theme      string `bun:"theme,notnull,default:''" json:"-"`
	Appearance string `bun:"appearance,notnull,default:''" json:"-"`

	// CustomCSS is the account's own stylesheet, inlined after the theme
	// stylesheet and only ever for the account that owns it. It is validated
	// on write (webui.ValidateCustomCSS) and again on render, because a value
	// that predates a validation change must not reach the template unchecked.
	CustomCSS string `bun:"custom_css,notnull,default:''" json:"-"`

	// DefaultPost* prefill new posts. They remain overridable per post, and
	// edit/redraft keep the original post's settings.
	DefaultPostLanguage   string     `bun:"default_post_language,notnull,default:'en'" json:"-"`
	DefaultPostMarkdown   bool       `bun:"default_post_markdown,notnull,default:false" json:"-"`
	DefaultPostVisibility Visibility `bun:"default_post_visibility,notnull,default:'public'" json:"-"`
	HideTimelineComposer  bool       `bun:"hide_timeline_composer,notnull,default:false" json:"-"`

	// Pruning preferences (nil = use server default)
	PruneOwnStatusesAfterDays *int `bun:"prune_own_statuses_after_days" json:"-"`
	PruneOwnStatusesMinLikes  *int `bun:"prune_own_statuses_min_likes" json:"-"`
	PruneOwnStatusesMinBoosts *int `bun:"prune_own_statuses_min_boosts" json:"-"`

	// Metadata
	AlsoKnownAs string     `bun:"also_known_as" json:"also_known_as"`
	MovedToURI  string     `bun:"moved_to_uri" json:"moved_to_uri"`
	MovedAt     *time.Time `bun:"moved_at" json:"moved_at"`
	Discovery   bool       `bun:"discovery,notnull,default:true" json:"-"`
	// RSSFeedEnabled publishes the account's opt-in federated RSS feed.
	RSSFeedEnabled bool `bun:"rss_feed_enabled,notnull,default:false" json:"-"`

	// Emojis holds the custom emojis referenced by the display name / note,
	// populated at read time (not a stored column).
	Emojis []CustomEmoji `bun:"-" json:"emojis,omitempty"`
}

// CanAuthenticate is the single account-policy check used by web, OAuth, API,
// and streaming authentication.

func (a *Account) CanAuthenticate(requireVerifiedEmail bool, requireAdminMFA ...bool) bool {
	mfaRequired := len(requireAdminMFA) > 0 && requireAdminMFA[0] && a != nil && a.IsAdmin
	return a != nil && a.Domain == nil && !a.IsServiceActor && a.Approved &&
		a.SuspendedAt == nil && a.ModerationState != "suspended" && a.ModerationState != "disabled" &&
		a.DeletionState == "" && (!requireVerifiedEmail || a.EmailVerified) && (!mfaRequired || a.MFAEnabled)
}

// Status represents a post/note/status.
type Status struct {
	ID        string    `bun:"id,pk,type:text" json:"id"`
	URI       string    `bun:"uri,unique,notnull" json:"uri"`
	URL       string    `bun:"url" json:"url"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`

	AccountID string   `bun:"account_id,notnull" json:"account_id"`
	Account   *Account `bun:"rel:belongs-to,join:account_id=id" json:"account,omitempty"`

	ContentHTML   string     `bun:"content_html,notnull" json:"content_html"`
	ContentSource string     `bun:"content_source" json:"content_source"`                          // original source (plain text or markdown)
	ContentType   string     `bun:"content_type,notnull,default:'text/plain'" json:"content_type"` // source format: text/plain or text/markdown
	Summary       string     `bun:"summary" json:"summary"`                                        // CW text
	Sensitive     bool       `bun:"sensitive,notnull,default:false" json:"sensitive"`
	Visibility    Visibility `bun:"visibility,notnull,default:'public'" json:"visibility"`
	Language      string     `bun:"language" json:"language"`

	InReplyToID  *string `bun:"in_reply_to_id" json:"in_reply_to_id"`
	InReplyToURI *string `bun:"in_reply_to_uri" json:"in_reply_to_uri"`
	InReplyTo    *Status `bun:"rel:belongs-to,join:in_reply_to_id=id" json:"in_reply_to,omitempty"`
	BoostOfID    *string `bun:"boost_of_id" json:"boost_of_id"`
	BoostOf      *Status `bun:"rel:belongs-to,join:boost_of_id=id" json:"boost_of,omitempty"`
	// BoostOfURI is not a column; BuildAccountExportSnapshot fills it in with a
	// batch lookup so the export can remap boosts by URI on import.
	BoostOfURI *string `bun:"-" json:"boost_of_uri,omitempty"`

	// Quote posts (FEP-044f). QuoteOfURI is the AP URI of the quoted object;
	// QuoteOfID is its local id when known. QuoteState tracks the approval
	// handshake (pending|accepted|rejected|revoked) and QuoteApprovalURI holds
	// the QuoteAuthorization URI (ours when we quote a local post, the remote
	// author's when granted to us).
	QuoteOfID        *string `bun:"quote_of_id" json:"quote_of_id,omitempty"`
	QuoteOfURI       *string `bun:"quote_of_uri" json:"quote_of_uri,omitempty"`
	QuoteOf          *Status `bun:"rel:belongs-to,join:quote_of_id=id" json:"quote_of,omitempty"`
	QuoteState       string  `bun:"quote_state" json:"-"`
	QuoteApprovalURI *string `bun:"quote_approval_uri" json:"-"`
	// BoostedBy is populated by timeline views when several boost wrappers for
	// the same original status are collapsed into one entry.
	BoostedBy []*Account `bun:"-" json:"-"`

	ConversationID *string `bun:"conversation_id" json:"conversation_id"`

	EditedAt *time.Time `bun:"edited_at" json:"edited_at"`

	// ExpiresAt, if set, is when this status is auto-deleted by the
	// status_expire job (StatusExpireJobKind). Enforced by a background job
	// poll, so actual deletion time is approximate, not exact.
	ExpiresAt *time.Time `bun:"expires_at" json:"expires_at,omitempty"`

	// ContextFetchedAt throttles on-demand remote thread refreshes. ContextOnly
	// marks statuses learned by pulling a remote replies collection; those are
	// visible in threads and profiles but must not be injected into timelines.
	ContextFetchedAt *time.Time `bun:"context_fetched_at" json:"-"`
	ContextOnly      bool       `bun:"context_only,notnull,default:false" json:"-"`

	// Denormalized counts
	FavouritesCount int `bun:"favourites_count,notnull,default:0" json:"favourites_count"`
	ReblogsCount    int `bun:"reblogs_count,notnull,default:0" json:"reblogs_count"`
	RepliesCount    int `bun:"replies_count,notnull,default:0" json:"replies_count"`

	// Lists populated on read
	// Preview travels with a status on the way in and is written by
	// CreateStatus, the way Poll and QuoteAuthorization are. It is never read
	// back into this field: the card is loaded from its own table.
	Preview          *StatusPreview         `bun:"-" json:"-"`
	MediaAttachments []MediaAttachment      `bun:"-" json:"media_attachments,omitempty"`
	Mentions         []Mention              `bun:"-" json:"mentions,omitempty"`
	Tags             []Tag                  `bun:"-" json:"tags,omitempty"`
	Emojis           []CustomEmoji          `bun:"-" json:"emojis,omitempty"`
	EmojiReactions   []EmojiReactionSummary `bun:"-" json:"-"`

	Pinned bool `bun:"pinned,notnull,default:false" json:"pinned,omitempty"`

	// IsBlogPost marks a top-level post for inclusion in the account's static
	// blog site (built only with -tags blog). Always present in the schema so
	// a non-blog build's storage layer never has to special-case the column.
	IsBlogPost bool `bun:"is_blog_post,notnull,default:false" json:"is_blog_post,omitempty"`
	// BlogSlug is the post's permalink path inside the account's blog, e.g.
	// "2026/07/25/hello-world". Empty until the generator assigns it (or
	// until the author supplies a custom one at compose time); once set it is
	// never recomputed, so editing the post cannot move its URL.
	BlogSlug string `bun:"blog_slug,notnull,default:''" json:"-"`

	// Interaction tracking (not stored, populated at read time)
	Favourited bool  `bun:"-" json:"favourited,omitempty"`
	Bookmarked bool  `bun:"-" json:"bookmarked,omitempty"`
	Reblogged  bool  `bun:"-" json:"reblogged,omitempty"`
	Muted      bool  `bun:"-" json:"muted,omitempty"`
	Poll       *Poll `bun:"-" json:"poll,omitempty"`
	// QuoteAuthorization is populated only while creating a local quote. The
	// storage layer persists it in the same transaction as the quoting status.
	QuoteAuthorization *QuoteAuthorization `bun:"-" json:"-"`
	FilterMatches      []FilterMatch       `bun:"-" json:"-"`

	// HomeFeedViewers is stashed by the home-feed fan-out during CreateStatus so
	// the timeline cache decorator can bump the same viewers without re-running
	// the audience query. nil = not computed (fall back to the query);
	// non-nil empty = computed, no viewers.
	HomeFeedViewers []string `bun:"-" json:"-"`
}
type FilterMatch struct {
	Filter          *Filter  `json:"filter"`
	KeywordMatches  []string `json:"keyword_matches"`
	StatusMatches   []string `json:"status_matches"`
	LanguageMatches []string `json:"language_matches,omitempty"`
}

type Poll struct {
	ID          string       `bun:"id,pk,type:text" json:"id"`
	StatusID    string       `bun:"status_id,notnull,unique" json:"-"`
	ExpiresAt   *time.Time   `bun:"expires_at" json:"expires_at"`
	Expired     bool         `bun:"expired,notnull,default:false" json:"expired"`
	Multiple    bool         `bun:"multiple,notnull,default:false" json:"multiple"`
	HideTotals  bool         `bun:"hide_totals,notnull,default:false" json:"-"`
	VotesCount  int          `bun:"votes_count,notnull,default:0" json:"votes_count"`
	VotersCount int          `bun:"voters_count,notnull,default:0" json:"voters_count"`
	Voted       bool         `bun:"-" json:"voted"`
	OwnVotes    []int        `bun:"-" json:"own_votes"`
	Options     []PollOption `bun:"-" json:"options"`
}

type PollOption struct {
	ID         string `bun:"id,pk,type:text" json:"-"`
	PollID     string `bun:"poll_id,notnull" json:"-"`
	Position   int    `bun:"position,notnull" json:"-"`
	Title      string `bun:"title,notnull" json:"title"`
	VotesCount int    `bun:"votes_count,notnull,default:0" json:"votes_count"`
}

type PollVote struct {
	ID        string    `bun:"id,pk,type:text"`
	PollID    string    `bun:"poll_id,notnull"`
	OptionID  string    `bun:"option_id,notnull"`
	AccountID string    `bun:"account_id,notnull"`
	CreatedAt time.Time `bun:"created_at,notnull"`
}

type Filter struct {
	ID                   string          `bun:"id,pk,type:text" json:"id"`
	AccountID            string          `bun:"account_id,notnull" json:"-"`
	Title                string          `bun:"title,notnull" json:"title"`
	Context              []string        `bun:"-" json:"context"`
	ContextJSON          string          `bun:"context_json,notnull" json:"-"`
	Languages            []string        `bun:"-" json:"languages"`
	LanguagesJSON        string          `bun:"languages_json,notnull,default:'[]'" json:"-"`
	InvertLanguages      bool            `bun:"invert_languages,notnull,default:false" json:"invert_languages"`
	ShowUnknownLanguages bool            `bun:"show_unknown_languages,notnull,default:false" json:"show_unknown_languages"`
	FilterAction         string          `bun:"filter_action,notnull,default:'warn'" json:"filter_action"`
	ExpiresAt            *time.Time      `bun:"expires_at" json:"expires_at"`
	Keywords             []FilterKeyword `bun:"-" json:"keywords"`
	Statuses             []any           `bun:"-" json:"statuses"`
}

type FilterKeyword struct {
	ID        string `bun:"id,pk,type:text" json:"id"`
	FilterID  string `bun:"filter_id,notnull" json:"-"`
	Keyword   string `bun:"keyword,notnull" json:"keyword"`
	WholeWord bool   `bun:"whole_word,notnull,default:true" json:"whole_word"`
}

type ScheduledStatus struct {
	ID          string    `bun:"id,pk,type:text" json:"id"`
	AccountID   string    `bun:"account_id,notnull" json:"-"`
	ParamsJSON  string    `bun:"params_json,notnull" json:"-"`
	ScheduledAt time.Time `bun:"scheduled_at,notnull" json:"scheduled_at"`
	CreatedAt   time.Time `bun:"created_at,notnull" json:"-"`
}

type Announcement struct {
	ID          string     `bun:"id,pk,type:text" json:"id"`
	Content     string     `bun:"content,notnull" json:"content"`
	StartsAt    *time.Time `bun:"starts_at" json:"starts_at"`
	EndsAt      *time.Time `bun:"ends_at" json:"ends_at"`
	AllDay      bool       `bun:"all_day,notnull,default:false" json:"all_day"`
	PublishedAt time.Time  `bun:"published_at,notnull" json:"published_at"`
}

// Follow represents a follow relationship between two accounts.
type Follow struct {
	ID        string    `bun:"id,pk,type:text" json:"id"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`

	AccountID       string `bun:"account_id,notnull,unique:idx_follow_pair" json:"account_id"`
	TargetAccountID string `bun:"target_account_id,notnull,unique:idx_follow_pair" json:"target_account_id"`
	URI             string `bun:"uri,notnull,unique" json:"uri"`

	State       FollowState `bun:"state,notnull,default:'accepted'" json:"state"`
	ShowReblogs bool        `bun:"show_reblogs,notnull,default:true" json:"show_reblogs"`
	Notify      bool        `bun:"notify,notnull,default:false" json:"notify"`
}

// Relay is a saved LitePub relay actor. The remote actor and inbox remain on
// Account; FollowID identifies the instance-to-relay half of the handshake.
type Relay struct {
	ID        string    `bun:"id,pk,type:text" json:"id"`
	AccountID string    `bun:"account_id,notnull,unique" json:"account_id"`
	FollowID  *string   `bun:"follow_id,unique" json:"follow_id,omitempty"`
	Enabled   bool      `bun:"enabled,notnull,default:false" json:"enabled"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`
}

type RelayState string

const (
	RelayDisabled           RelayState = "disabled"
	RelayPending            RelayState = "pending"
	RelayRejected           RelayState = "rejected"
	RelayAwaitingFollowBack RelayState = "awaiting-follow-back"
	RelayActive             RelayState = "active"
	RelayError              RelayState = "error"
)

// Favourite represents a like/favourite on a status.
type Favourite struct {
	ID        string    `bun:"id,pk,type:text" json:"id"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	AccountID string    `bun:"account_id,notnull,unique:idx_fave_unique" json:"account_id"`
	StatusID  string    `bun:"status_id,notnull,unique:idx_fave_unique" json:"status_id"`
	URI       string    `bun:"uri" json:"uri"`
}

type EmojiReaction struct {
	ID        string    `bun:"id,pk,type:text" json:"id"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	URI       string    `bun:"uri,notnull,unique" json:"uri"`
	AccountID string    `bun:"account_id,notnull,unique:idx_emoji_reaction" json:"account_id"`
	StatusID  string    `bun:"status_id,notnull,unique:idx_emoji_reaction" json:"status_id"`
	Content   string    `bun:"content,notnull" json:"content"`
	Key       string    `bun:"reaction_key,notnull,unique:idx_emoji_reaction" json:"reaction_key"`
	EmojiID   *string   `bun:"emoji_id" json:"emoji_id,omitempty"`
}

type EmojiReactionSummary struct {
	Name       string       `json:"name"`
	Key        string       `json:"-"`
	Count      int          `json:"count"`
	Me         bool         `json:"me"`
	AccountIDs []string     `json:"account_ids"`
	Accounts   []*Account   `json:"-"`
	Emoji      *CustomEmoji `json:"-"`
}

// QuoteAuthorization is the grant we issue (FEP-044f) when a remote actor is
// allowed to quote one of our local statuses. It is served as an AS2 object at
// URI and referenced by the quoting post via its quoteAuthorization property.
type QuoteAuthorization struct {
	ID              string    `bun:"id,pk,type:text" json:"id"`
	CreatedAt       time.Time `bun:"created_at,notnull" json:"created_at"`
	URI             string    `bun:"uri,notnull,unique" json:"uri"`
	QuotedStatusID  string    `bun:"quoted_status_id,notnull" json:"quoted_status_id"`
	QuotingURI      string    `bun:"quoting_uri,notnull" json:"quoting_uri"`
	QuotingActorURI string    `bun:"quoting_actor_uri,notnull" json:"quoting_actor_uri"`
}

// MediaAttachment represents an attached media file.
type MediaAttachment struct {
	ID        string    `bun:"id,pk,type:text" json:"id"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`

	StatusID  *string `bun:"status_id" json:"status_id"`
	AccountID string  `bun:"account_id,notnull" json:"account_id"`

	Type              string `bun:"type,notnull" json:"type"` // image, video, audio, unknown
	URL               string `bun:"url" json:"url"`
	PreviewURL        string `bun:"preview_url" json:"preview_url"`
	RemoteURL         string `bun:"remote_url" json:"remote_url"`
	Blurhash          string `bun:"blurhash" json:"blurhash"`
	Description       string `bun:"description" json:"description"` // alt text
	Meta              string `bun:"meta" json:"meta"`               // JSON metadata
	FileSize          int64  `bun:"file_size" json:"file_size"`
	MIMEType          string `bun:"mime_type" json:"mime_type"`
	StorageBackend    string `bun:"storage_backend,notnull,default:'local'" json:"-"`
	StorageKey        string `bun:"storage_key" json:"-"`
	PreviewStorageKey string `bun:"preview_storage_key" json:"-"`

	// CachedAt records when a remote media file was fetched and stored locally.
	// nil means the media is not cached (served via the live proxy or directly).
	// When set, URL/PreviewURL point at the local cache and RemoteURL holds the origin.
	CachedAt *time.Time `bun:"cached_at" json:"-"`
}

// MaxMediaDescriptionLength caps alt text in characters, the same limit
// Mastodon enforces, so a description written here fits everywhere it goes.
const MaxMediaDescriptionLength = 1500

// Dimensions returns the pixel size recorded by image processing. Zero means
// the attachment has not been measured, so callers should omit dimensions
// rather than reserve the wrong shape.
func (m MediaAttachment) Dimensions() (width, height int) {
	if m.Meta == "" {
		return 0, 0
	}
	var meta struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	}
	if err := json.Unmarshal([]byte(m.Meta), &meta); err != nil || meta.Width <= 0 || meta.Height <= 0 {
		return 0, 0
	}
	return meta.Width, meta.Height
}

type MediaStorageMigration struct {
	bun.BaseModel     `bun:"table:media_storage_migrations"`
	ID                string    `bun:"id,pk,type:text" json:"id"`
	CreatedAt         time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt         time.Time `bun:"updated_at,notnull" json:"updated_at"`
	SourceConfig      string    `bun:"source_config,notnull" json:"-"`
	DestinationConfig string    `bun:"destination_config,notnull" json:"-"`
	State             string    `bun:"state,notnull" json:"state"`
	Cursor            string    `bun:"cursor" json:"cursor"`
	Total             int64     `bun:"total,notnull,default:0" json:"total"`
	Copied            int64     `bun:"copied,notnull,default:0" json:"copied"`
	Failed            int64     `bun:"failed,notnull,default:0" json:"failed"`
	LastError         string    `bun:"last_error" json:"last_error"`
	CancelRequested   bool      `bun:"cancel_requested,notnull,default:false" json:"cancel_requested"`
}

// MediaCacheFailure is a negative-cache entry: a remote media URL that returned
// a non-cacheable result (404, oversized, wrong content type, ...). The eager
// sweep and lazy proxy hook consult it so they stop re-enqueueing the same dead
// URL every cycle. Entries are retried once they age past the retry window, in
// case the remote recovers.
type MediaCacheFailure struct {
	bun.BaseModel `bun:"table:media_cache_failures"`

	URL      string    `bun:"url,pk,type:text" json:"url"`
	Reason   string    `bun:"reason,notnull" json:"reason"`
	FailedAt time.Time `bun:"failed_at,notnull" json:"failed_at"`
}

// StatusPreview is a durable Open Graph card for a status. Its lifetime is
// tied to the status: deleting the status deletes this row, and
// cache GC can then reclaim any locally cached image it referenced.
type StatusPreview struct {
	StatusID    string     `bun:"status_id,pk,type:text" json:"-"`
	SourceURL   string     `bun:"source_url,notnull" json:"-"`
	URL         string     `bun:"url,notnull" json:"url"`
	Title       string     `bun:"title,notnull" json:"title"`
	Description string     `bun:"description" json:"description"`
	Image       string     `bun:"image" json:"image"`
	ImageRemote string     `bun:"image_remote" json:"-"`
	SiteName    string     `bun:"site_name" json:"site_name"`
	FetchedAt   time.Time  `bun:"fetched_at,notnull" json:"-"`
	CachedAt    *time.Time `bun:"cached_at" json:"-"`
}

// Mention represents a mention of an account within a status.
type Mention struct {
	ID        string `bun:"id,pk,type:text" json:"id"`
	StatusID  string `bun:"status_id,notnull" json:"-"`
	AccountID string `bun:"account_id,notnull" json:"-"`
	Username  string `bun:"username,notnull" json:"username"`
	URL       string `bun:"url,notnull" json:"url"`
	Acct      string `bun:"acct,notnull" json:"acct"` // username@domain or just username
}

// Tag represents a hashtag.
type Tag struct {
	ID            string `bun:"id,pk,type:text" json:"id"`
	Name          string `bun:"name,notnull,unique" json:"name"` // without #
	URL           string `bun:"url,notnull" json:"url"`
	StatusesCount int    `bun:"statuses_count,notnull,default:0" json:"statuses_count"`

	// Following is populated at read time when a viewer is present.
	Following bool `bun:"-" json:"following,omitempty"`
}

// StatusTag joins tags to statuses.
type StatusTag struct {
	StatusID string `bun:"status_id,notnull,pk"`
	TagID    string `bun:"tag_id,notnull,pk"`
}

// Marker records a client's last-read position for one timeline (Mastodon
// GET/POST /api/v1/markers). Apps use it to compute the unread badge; without
// persistence every poll made all notifications look unread again.
type Marker struct {
	bun.BaseModel `bun:"table:markers"`
	AccountID     string    `bun:"account_id,pk,notnull" json:"-"`
	Timeline      string    `bun:"timeline,pk,notnull" json:"-"` // "home" or "notifications"
	LastReadID    string    `bun:"last_read_id,notnull" json:"last_read_id"`
	Version       int       `bun:"version,notnull,default:0" json:"version"`
	UpdatedAt     time.Time `bun:"updated_at,notnull" json:"updated_at"`
}

// CatchupAffinity is the read shape of an account's own recent interaction
// history, used to weight the catch-up ranking. Nothing here is stored: it is
// derived on demand from the account's favourites and its own statuses, so the
// feature keeps no behavioural profile of its own.
type CatchupAffinity struct {
	// AuthorIDs are the accounts whose posts this account favourited, boosted
	// or replied to inside the window.
	AuthorIDs []string
	// ThreadIDs are the statuses this account replied to inside the window.
	// A reply to one of them means "a conversation you took part in"; the
	// status itself has by definition already been read.
	ThreadIDs []string
}

// HomeFeedEntry materializes one status into one local viewer's home feed
// (fan-out on write). The home timeline read becomes an indexed range scan on
// (account_id, created_at) instead of a scan across every followed author -
// the difference between milliseconds and seconds on SD-card-class storage.
// AuthorAccountID makes unfollow cleanup a single indexed delete.
type HomeFeedEntry struct {
	bun.BaseModel   `bun:"table:home_feed"`
	AccountID       string    `bun:"account_id,pk,notnull"`
	StatusID        string    `bun:"status_id,pk,notnull"`
	AuthorAccountID string    `bun:"author_account_id,notnull"`
	CreatedAt       time.Time `bun:"created_at,notnull"`
}

// FollowedTag records a local account subscribing to a hashtag.
type FollowedTag struct {
	ID        string    `bun:"id,pk,type:text" json:"id"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	AccountID string    `bun:"account_id,notnull,unique:idx_followed_tag_pair" json:"account_id"`
	TagID     string    `bun:"tag_id,notnull,unique:idx_followed_tag_pair" json:"tag_id"`
}

// FeaturedTag records a local account pinning a hashtag to their profile
// (Mastodon GET/POST/DELETE /api/v1/featured_tags).
type FeaturedTag struct {
	ID        string    `bun:"id,pk,type:text" json:"id"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	AccountID string    `bun:"account_id,notnull,unique:idx_featured_tag_pair" json:"account_id"`
	TagID     string    `bun:"tag_id,notnull,unique:idx_featured_tag_pair" json:"tag_id"`
}

// FeaturedTagView is the read shape for a featured tag: the pin row joined
// with its tag's display fields and the owning account's own usage of it.
type FeaturedTagView struct {
	ID            string     `bun:"id" json:"id"`
	Name          string     `bun:"name" json:"name"`
	URL           string     `bun:"url" json:"url"`
	StatusesCount int        `bun:"statuses_count" json:"statuses_count"`
	LastStatusAt  *time.Time `bun:"last_status_at" json:"last_status_at"`
}

// CustomEmoji represents a custom emoji. Rows are shared globally and keyed by
// (shortcode, domain): a remote instance's `:blobcat:` is a distinct row from a
// different instance's. Local emoji (none yet) would use a null domain.
type CustomEmoji struct {
	ID        string    `bun:"id,pk,type:text" json:"id"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`

	Shortcode string  `bun:"shortcode,notnull,unique:idx_emoji_shortcode_domain" json:"shortcode"`
	Domain    *string `bun:"domain,unique:idx_emoji_shortcode_domain" json:"domain"` // null = local

	// URL is the image to load (the local cache path once cached, otherwise the
	// remote origin). StaticURL is the non-animated variant (same as URL when the
	// remote provides none). RemoteURL preserves the origin across caching so the
	// cache GC and re-cache logic can map a local file back to its source.
	URL       string `bun:"url,notnull" json:"url"`
	StaticURL string `bun:"static_url" json:"static_url"`
	RemoteURL string `bun:"remote_url" json:"-"`

	VisibleInPicker bool `bun:"visible_in_picker,notnull,default:true" json:"visible_in_picker"`

	// CachedAt records when the remote image was fetched and stored locally.
	// nil means it is served via the live proxy (or directly for local emoji).
	CachedAt *time.Time `bun:"cached_at" json:"-"`
}

// Notification represents a notification for the local user.
type Notification struct {
	ID            string    `bun:"id,pk,type:text" json:"id"`
	APIID         int64     `bun:"api_id,notnull,unique" json:"-"`
	CreatedAt     time.Time `bun:"created_at,notnull" json:"created_at"`
	Type          string    `bun:"type,notnull" json:"type"` // content and relationship event type
	AccountID     string    `bun:"account_id,notnull" json:"account_id"`
	FromAccountID string    `bun:"from_account_id,notnull" json:"from_account_id"`
	StatusID      *string   `bun:"status_id" json:"status_id"`
	Emoji         string    `bun:"emoji" json:"emoji,omitempty"`
	Read          bool      `bun:"read,notnull,default:false" json:"read"`
}

// Block represents a block relationship.
type Block struct {
	ID              string    `bun:"id,pk,type:text" json:"id"`
	CreatedAt       time.Time `bun:"created_at,notnull" json:"created_at"`
	AccountID       string    `bun:"account_id,notnull,unique:idx_block_pair" json:"account_id"`
	TargetAccountID string    `bun:"target_account_id,notnull,unique:idx_block_pair" json:"target_account_id"`
}

// Mute represents a mute relationship.
type Mute struct {
	ID                string    `bun:"id,pk,type:text" json:"id"`
	CreatedAt         time.Time `bun:"created_at,notnull" json:"created_at"`
	AccountID         string    `bun:"account_id,notnull,unique:idx_mute_pair" json:"account_id"`
	TargetAccountID   string    `bun:"target_account_id,notnull,unique:idx_mute_pair" json:"target_account_id"`
	HideNotifications bool      `bun:"hide_notifications,notnull,default:false" json:"hide_notifications"`
	Duration          *int      `bun:"duration" json:"duration"` // in minutes, nil = indefinite
}

// DomainBlock represents an instance-wide block against an entire domain,
// configured by an admin.
type DomainBlock struct {
	ID        string    `bun:"id,pk,type:text" json:"id"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	Domain    string    `bun:"domain,notnull,unique" json:"domain"`
	Severity  string    `bun:"severity,notnull,default:'silence'" json:"severity"` // noop, silence or suspend
	Public    bool      `bun:"public,notnull,default:false" json:"public"`
	Comment   string    `bun:"comment" json:"comment"`
	// Mastodon-parity options.
	RejectMedia   bool `bun:"reject_media,notnull,default:false" json:"reject_media"`
	RejectReports bool `bun:"reject_reports,notnull,default:false" json:"reject_reports"`
	Obfuscate     bool `bun:"obfuscate,notnull,default:false" json:"obfuscate"`
}

// DomainAllow is an explicit federation exception for one remote hostname.
type DomainAllow struct {
	ID        string    `bun:"id,pk,type:text" json:"id"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	Domain    string    `bun:"domain,notnull,unique" json:"domain"`
}

// CommercialDomainBlock is one entry from the replaceable Big Tech list.
type CommercialDomainBlock struct {
	Domain       string `bun:"domain,pk,type:text" json:"domain"`
	Organization string `bun:"organization" json:"organization"`
	Service      string `bun:"service" json:"service"`
	Reference    string `bun:"reference" json:"reference"`
}

// FederationPolicyState is the singleton persisted federation configuration.
type FederationPolicyState struct {
	bun.BaseModel            `bun:"table:federation_policy_state"`
	ID                       int        `bun:"id,pk" json:"-"`
	Mode                     string     `bun:"mode,notnull,default:'open'" json:"mode"`
	CommercialBigTechEnabled bool       `bun:"commercial_bigtech_enabled,notnull,default:false" json:"commercial_bigtech_enabled"`
	SourceURL                string     `bun:"source_url" json:"source_url"`
	ETag                     string     `bun:"etag" json:"etag"`
	LastModified             string     `bun:"last_modified" json:"last_modified"`
	LastAttempt              *time.Time `bun:"last_attempt" json:"last_attempt"`
	LastSuccess              *time.Time `bun:"last_success" json:"last_success"`
	LastError                string     `bun:"last_error" json:"last_error"`
	LeaseUntil               *time.Time `bun:"lease_until" json:"lease_until"`
}

// Report is a moderation report, either filed locally by a user or received as
// a federated Flag activity from a remote instance.
type Report struct {
	ID              string     `bun:"id,pk,type:text" json:"id"`
	CreatedAt       time.Time  `bun:"created_at,notnull" json:"created_at"`
	AccountID       *string    `bun:"account_id" json:"account_id"` // local reporter; nil if remote
	TargetAccountID string     `bun:"target_account_id,notnull" json:"target_account_id"`
	StatusIDs       string     `bun:"status_ids" json:"-"` // comma-separated local status IDs
	Comment         string     `bun:"comment" json:"comment"`
	Category        string     `bun:"category,notnull,default:'other'" json:"category"`
	Forwarded       bool       `bun:"forwarded,notnull,default:false" json:"forwarded"`
	State           string     `bun:"state,notnull,default:'open'" json:"state"` // open | resolved
	AssignedAdminID *string    `bun:"assigned_admin_id" json:"assigned_admin_id"`
	ActionTaken     string     `bun:"action_taken" json:"action_taken"`
	OriginDomain    *string    `bun:"origin_domain" json:"origin_domain"`
	ResolvedAt      *time.Time `bun:"resolved_at" json:"resolved_at"`

	// Joined for display, not persisted.
	Reporter *Account `bun:"-" json:"-"`
	Target   *Account `bun:"-" json:"-"`
}

// ModerationLog records an admin moderation action for the audit trail.
type ModerationLog struct {
	ID             string    `bun:"id,pk,type:text" json:"id"`
	CreatedAt      time.Time `bun:"created_at,notnull" json:"created_at"`
	AdminAccountID string    `bun:"admin_account_id,notnull" json:"admin_account_id"`
	TargetType     string    `bun:"target_type,notnull" json:"target_type"` // account | domain | report | instance
	TargetID       string    `bun:"target_id,notnull" json:"target_id"`
	Action         string    `bun:"action,notnull" json:"action"`
	Note           string    `bun:"note" json:"note"`
}

// AccountDomainBlock represents a per-user block of an entire domain. Unlike the
// admin DomainBlock it only affects the owning account's own timelines.
type AccountDomainBlock struct {
	ID        string    `bun:"id,pk,type:text" json:"id"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	AccountID string    `bun:"account_id,notnull,unique:idx_acct_domain_block" json:"account_id"`
	Domain    string    `bun:"domain,notnull,unique:idx_acct_domain_block" json:"domain"`
}

// Instance stores per-domain metadata about known remote servers.
type Instance struct {
	ID               string     `bun:"id,pk,type:text" json:"id"`
	Domain           string     `bun:"domain,notnull,unique" json:"domain"`
	Software         string     `bun:"software" json:"software"`
	Version          string     `bun:"version" json:"version"`
	NodeInfoURL      string     `bun:"nodeinfo_url" json:"nodeinfo_url"`
	UnreachableSince *time.Time `bun:"unreachable_since" json:"unreachable_since"`
	BackoffCount     int        `bun:"backoff_count,notnull,default:0" json:"backoff_count"`
	LastSuccess      *time.Time `bun:"last_success" json:"last_success"`
	LastFailure      *time.Time `bun:"last_failure" json:"last_failure"`
	Suspended        bool       `bun:"suspended,notnull,default:false" json:"suspended"`
	CreatedAt        time.Time  `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt        time.Time  `bun:"updated_at,notnull" json:"updated_at"`
}

// OAuthApplication represents a registered API client.
type OAuthApplication struct {
	bun.BaseModel `bun:"table:oauth_applications"`

	ID           string    `bun:"id,pk,type:text" json:"id"`
	CreatedAt    time.Time `bun:"created_at,notnull" json:"created_at"`
	Name         string    `bun:"name,notnull" json:"name"`
	Website      string    `bun:"website" json:"website"`
	RedirectURIs string    `bun:"redirect_uris,notnull" json:"redirect_uris"`
	ClientID     string    `bun:"client_id,unique,notnull" json:"client_id"`
	ClientSecret string    `bun:"client_secret,notnull" json:"-"` // SHA-256 digest at rest
	Scopes       string    `bun:"scopes,notnull,default:'read'" json:"scopes"`
}

// OAuthToken represents an OAuth2 access token.
type OAuthToken struct {
	bun.BaseModel `bun:"table:oauth_tokens"`

	ID                  string     `bun:"id,pk,type:text" json:"id"`
	CreatedAt           time.Time  `bun:"created_at,notnull" json:"created_at"`
	AccessToken         string     `bun:"access_token,unique,notnull" json:"-"` // SHA-256 digest at rest
	RefreshToken        string     `bun:"refresh_token,unique" json:"-"`        // SHA-256 digest at rest
	AccountID           string     `bun:"account_id,notnull" json:"account_id"`
	ApplicationID       string     `bun:"application_id,notnull" json:"application_id"`
	Scopes              string     `bun:"scopes,notnull" json:"scopes"`
	AuthorizationCode   bool       `bun:"authorization_code,notnull,default:false" json:"-"`
	RedirectURI         string     `bun:"redirect_uri" json:"-"`
	CodeChallenge       string     `bun:"code_challenge" json:"-"`
	CodeChallengeMethod string     `bun:"code_challenge_method" json:"-"`
	ExpiresAt           *time.Time `bun:"expires_at" json:"expires_at"`
	RevokedAt           *time.Time `bun:"revoked_at" json:"revoked_at"`
}

// DeliveryMaxAttempts is the MaxAttempts used for outbound ActivityPub
// delivery jobs. It is high so that a remote instance which is
// unreachable for an extended period (e.g. overnight maintenance) still
// eventually receives the activity. Combined with the queue's capped
// exponential backoff (6h cap) this spreads ~16 attempts over ~2 days,
// mirroring Mastodon's ActivityPub::DeliveryWorker (retry: 16). Media-cache and
// push jobs use lower limits since their retry window need not span days.
const DeliveryMaxAttempts = 16

// SelfDestructDeliveryJobKind is separate from normal delivery:
// shutdown deliveries must run while the rest of the queue remains paused.
const SelfDestructDeliveryJobKind = "self_destruct_delivery"

const (
	AccountDeletionDeliveryJobKind = "account_deletion_delivery"
	AccountDeletionFinalizeJobKind = "account_deletion_finalize"
	AccountExportJobKind           = "account_export"
	AccountExportCleanupJobKind    = "account_export_cleanup"
	AccountImportJobKind           = "account_import"
	FollowImportJobKind            = "follow_import"
	NotificationCreatedJobKind     = "notification_created"
	StatusExpireJobKind            = "status_expire"
)

// AccountDeletionRun records a durable local-account deletion. It deliberately
// has no foreign key to accounts so its final state survives the purge.
type AccountDeletionRun struct {
	bun.BaseModel    `bun:"table:account_deletion_runs"`
	ID               string     `bun:"id,pk,type:text" json:"id"`
	AccountID        string     `bun:"account_id,notnull,unique" json:"-"`
	ActorURI         string     `bun:"actor_uri,notnull" json:"-"`
	RequestedAt      time.Time  `bun:"requested_at,notnull" json:"requested_at"`
	DeadlineAt       time.Time  `bun:"deadline_at,notnull" json:"deadline_at"`
	CompletedAt      *time.Time `bun:"completed_at" json:"completed_at,omitempty"`
	State            string     `bun:"state,notnull" json:"state"`
	DestinationCount int        `bun:"destination_count,notnull" json:"destination_count"`
	DeliveredCount   int        `bun:"delivered_count,notnull" json:"delivered_count"`
}

type AccountDeletionDestination struct {
	bun.BaseModel `bun:"table:account_deletion_destinations"`
	ID            string     `bun:"id,pk,type:text" json:"-"`
	RunID         string     `bun:"run_id,notnull,unique:idx_account_deletion_destination" json:"-"`
	Inbox         string     `bun:"inbox,notnull,unique:idx_account_deletion_destination" json:"-"`
	DeliveredAt   *time.Time `bun:"delivered_at" json:"-"`
}

type AccountDeletionDeliveryPayload struct {
	RunID         string `json:"run_id"`
	DestinationID string `json:"destination_id"`
}

type MFARecoveryCode struct {
	bun.BaseModel `bun:"table:mfa_recovery_codes"`
	ID            string     `bun:"id,pk,type:text"`
	AccountID     string     `bun:"account_id,notnull"`
	CodeHash      string     `bun:"code_hash,notnull,unique"`
	CreatedAt     time.Time  `bun:"created_at,notnull"`
	UsedAt        *time.Time `bun:"used_at"`
}

// AccountExport tracks a private, short-lived user data archive. StorageKey is
// never exposed to the browser; downloads are authenticated, CSRF-protected,
// and claimed with a single-use token hash.
type AccountExport struct {
	bun.BaseModel `bun:"table:account_exports"`
	ID            string     `bun:"id,pk,type:text" json:"id"`
	AccountID     string     `bun:"account_id,notnull" json:"-"`
	State         string     `bun:"state,notnull" json:"state"`
	StorageKey    string     `bun:"storage_key" json:"-"`
	Size          int64      `bun:"size,notnull,default:0" json:"size"`
	TokenHash     string     `bun:"token_hash" json:"-"`
	ExpiresAt     *time.Time `bun:"expires_at" json:"expires_at,omitempty"`
	DownloadedAt  *time.Time `bun:"downloaded_at" json:"downloaded_at,omitempty"`
	CreatedAt     time.Time  `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt     time.Time  `bun:"updated_at,notnull" json:"updated_at"`
	LastError     string     `bun:"last_error" json:"-"`
}

// AccountExportSnapshot is an explicit allow-list for a user's export. It
// cannot contain password/key/token/MFA or moderation-only data.
type AccountExportSnapshot struct {
	Profile          AccountExportProfile `json:"profile"`
	Preferences      map[string]any       `json:"preferences"`
	Statuses         []*Status            `json:"statuses"`
	Following        []string             `json:"following"`
	Followers        []string             `json:"followers"`
	Lists            []AccountExportList  `json:"lists"`
	Blocks           []string             `json:"blocks"`
	Mutes            []AccountExportMute  `json:"mutes"`
	DomainBlocks     []string             `json:"domain_blocks"`
	Bookmarks        []string             `json:"bookmarks"`
	Media            []*MediaAttachment   `json:"media"`
	FollowedHashtags []string             `json:"followed_hashtags"`
}

type AccountExportProfile struct {
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	Note        string    `json:"note"`
	Email       string    `json:"email"`
	Locale      string    `json:"locale"`
	ActorURI    string    `json:"actor_uri"`
	Avatar      string    `json:"avatar"`
	Header      string    `json:"header"`
	CreatedAt   time.Time `json:"created_at"`
}

type AccountExportList struct {
	Title         string   `json:"title"`
	RepliesPolicy string   `json:"replies_policy"`
	Exclusive     bool     `json:"exclusive"`
	Members       []string `json:"members"`
}

type AccountExportMute struct {
	Account           string `json:"account"`
	HideNotifications bool   `json:"hide_notifications"`
	DurationMinutes   *int   `json:"duration_minutes,omitempty"`
}

// AccountImport tracks an archive or CSV import. ResultJSON holds the counters
// and, for CSV imports, the cursor needed to resume the next step.
type AccountImport struct {
	bun.BaseModel `bun:"table:account_imports"`
	ID            string    `bun:"id,pk,type:text" json:"id"`
	AccountID     string    `bun:"account_id,notnull" json:"-"`
	State         string    `bun:"state,notnull" json:"state"`
	ResultJSON    string    `bun:"result_json" json:"result,omitempty"`
	LastError     string    `bun:"last_error" json:"-"`
	CreatedAt     time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt     time.Time `bun:"updated_at,notnull" json:"updated_at"`
}

// AccountImportResult is the per-category outcome of an AccountImport job,
// serialized into AccountImport.ResultJSON.
type AccountImportResult struct {
	FollowImport   *FollowImportProgress `json:"follow_import,omitempty"`
	Statuses       ImportCount           `json:"statuses"`
	Media          ImportCount           `json:"media"`
	Following      ImportCount           `json:"following"`
	Blocks         ImportCount           `json:"blocks"`
	Mutes          ImportCount           `json:"mutes"`
	DomainBlocks   ImportCount           `json:"domain_blocks"`
	Lists          ImportCount           `json:"lists"`
	FollowedTags   ImportCount           `json:"followed_hashtags"`
	Bookmarks      ImportCount           `json:"bookmarks"`
	FollowersNoted int                   `json:"followers_noted"`
}

type FollowImportProgress struct {
	Total     int `json:"total"`
	Processed int `json:"processed"`
	Existing  int `json:"existing"`
	Failed    int `json:"failed"`
	Retries   int `json:"retries,omitempty"`
}

type ImportCount struct {
	Imported int `json:"imported"`
	Skipped  int `json:"skipped"`
}

// SelfDestructRun records the single, immutable shutdown fan-out.
type SelfDestructRun struct {
	bun.BaseModel `bun:"table:self_destruct_runs"`
	ID            string    `bun:"id,pk,type:text"`
	StartedAt     time.Time `bun:"started_at,notnull"`
	IdentityCount int       `bun:"identity_count,notnull"`
	InboxCount    int       `bun:"inbox_count,notnull"`
	DeliveryCount int       `bun:"delivery_count,notnull"`
}

// JobPayload is a shared job body referenced by many jobs: delivery fan-out
// stores the activity JSON once here and each per-inbox delivery job carries
// only the row ID. Rows are pruned by age in queue maintenance, well after
// the delivery retry window has passed.
type JobPayload struct {
	ID        string    `bun:"id,pk,type:text" json:"id"`
	Body      string    `bun:"body,notnull" json:"body"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
}

// Job represents a background job in the queue.
type Job struct {
	ID          string     `bun:"id,pk,type:text" json:"id"`
	CreatedAt   time.Time  `bun:"created_at,notnull" json:"created_at"`
	Kind        string     `bun:"kind,notnull" json:"kind"`
	Payload     string     `bun:"payload,notnull" json:"payload"`
	RunAt       time.Time  `bun:"run_at,notnull,default:now()" json:"run_at"`
	Attempts    int        `bun:"attempts,notnull" json:"attempts"`
	MaxAttempts int        `bun:"max_attempts,notnull,default:10" json:"max_attempts"`
	State       string     `bun:"state,notnull,default:'queued'" json:"state"` // queued, running, completed, failed
	Error       string     `bun:"error" json:"error"`
	UpdatedAt   time.Time  `bun:"updated_at,notnull" json:"updated_at"`
	TakenAt     *time.Time `bun:"taken_at" json:"taken_at"`
	Priority    int        `bun:"priority,notnull" json:"priority"`
}

// QueueKindStats contains current durable queue state for one job kind.
type QueueKindStats struct {
	Kind                              string
	Ready, Scheduled, Running, Failed int
}

// QueueStats is an operational snapshot derived from durable job rows.
type QueueStats struct {
	Ready, Scheduled, Running, Failed int
	Retrying, StaleRunning            int
	MaxAttemptsObserved               int
	OldestReadyAt, OldestRunningAt    *time.Time
	ByKind                            []QueueKindStats
}

// QueueProblemJob is the safe subset rendered by the admin queue page. Payload
// is excluded because it may contain private notification data.
type QueueProblemJob struct {
	ID, Kind, State, Error      string
	Attempts, MaxAttempts       int
	CreatedAt, UpdatedAt, RunAt time.Time
	TakenAt                     *time.Time
}

// Session represents an authenticated web UI session.
type Session struct {
	ID        string    `bun:"id,pk,type:text" json:"id"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`
	ExpiresAt time.Time `bun:"expires_at,notnull" json:"expires_at"`

	AccountID string `bun:"account_id,notnull" json:"account_id"`
	Token     string `bun:"-" json:"-"`                         // raw browser token, carried into CreateSession and never stored
	TokenHash string `bun:"token_hash,unique,notnull" json:"-"` // SHA-256 hash of the browser token
	UserAgent string `bun:"user_agent" json:"-"`
	IPAddress string `bun:"ip_address" json:"-"`
}

// Replies-policy values for List.RepliesPolicy (Mastodon-compatible).
const (
	ListRepliesFollowed = "followed" // show replies to anyone the owner follows
	ListRepliesList     = "list"     // show replies to other list members only
	ListRepliesNone     = "none"     // hide all replies (self-replies excepted)
)

// List represents a user-defined list of accounts for timeline filtering.
type List struct {
	ID        string    `bun:"id,pk,type:text" json:"id"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`

	Title     string `bun:"title,notnull" json:"title"`
	AccountID string `bun:"account_id,notnull" json:"account_id"` // owner

	RepliesPolicy string `bun:"replies_policy,notnull,default:'list'" json:"replies_policy"`
	Exclusive     bool   `bun:"exclusive,notnull,default:false" json:"exclusive"`
}

// ListAccount joins accounts to a list.
type ListAccount struct {
	ListID    string `bun:"list_id,notnull,pk" json:"list_id"`
	AccountID string `bun:"account_id,notnull,pk" json:"account_id"`
}

// Bookmark represents a bookmarked status.
type Bookmark struct {
	ID        string    `bun:"id,pk,type:text" json:"id"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	AccountID string    `bun:"account_id,notnull,unique:idx_bookmark_unique" json:"account_id"`
	StatusID  string    `bun:"status_id,notnull,unique:idx_bookmark_unique" json:"status_id"`
}

// Invite represents an invitation token for registration.
type Invite struct {
	ID          string     `bun:"id,pk,type:text" json:"id"`
	CreatedAt   time.Time  `bun:"created_at,notnull" json:"created_at"`
	ExpiresAt   *time.Time `bun:"expires_at" json:"expires_at"`
	MaxUses     int        `bun:"max_uses,notnull,default:0" json:"max_uses"` // 0 = unlimited
	UseCount    int        `bun:"use_count,notnull,default:0" json:"use_count"`
	CreatedByID string     `bun:"created_by_id,notnull" json:"created_by_id"`
	Code        string     `bun:"code,unique,notnull" json:"code"` // short code like "summer2026"
}

// PushSubscription represents one app or browser's Web Push subscription.
type PushSubscription struct {
	ID        string    `bun:"id,pk,type:text" json:"id"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`

	// Owner of this subscription (local account only)
	AccountID string `bun:"account_id,notnull" json:"account_id"`

	// Exactly one of OAuthTokenID (API client) and SessionID (web/PWA) is set.
	OAuthTokenID *string `bun:"oauth_token_id" json:"-"`
	SessionID    *string `bun:"session_id" json:"-"`

	// Human-readable identifier for Settings UI ("Web (Firefox)", an OAuth
	// application's name, ...). Best-effort, not used for delivery.
	Label string `bun:"label" json:"-"`

	// Push endpoint URL (provided by the browser/client push service)
	Endpoint string `bun:"endpoint,notnull" json:"endpoint"`

	// Encryption keys (base64-encoded)
	P256DH string `bun:"p256dh,notnull" json:"p256dh"` // client's public key (ECDH P-256)
	Auth   string `bun:"auth,notnull" json:"auth"`     // shared auth secret

	// Notification type preferences (what types to send push for)
	NotifyMention       bool `bun:"notify_mention,notnull,default:true" json:"notify_mention"`
	NotifyFollow        bool `bun:"notify_follow,notnull,default:false" json:"notify_follow"`
	NotifyReblog        bool `bun:"notify_reblog,notnull,default:false" json:"notify_reblog"`
	NotifyFavourite     bool `bun:"notify_favourite,notnull,default:false" json:"notify_favourite"`
	NotifyPoll          bool `bun:"notify_poll,notnull,default:true" json:"notify_poll"`
	NotifyFollowRequest bool `bun:"notify_follow_request,notnull,default:false" json:"notify_follow_request"`
	NotifyStatus        bool `bun:"notify_status,notnull,default:true" json:"notify_status"`
	NotifyQuote         bool `bun:"notify_quote,notnull,default:true" json:"notify_quote"`
	NotifyBlogRebuild   bool `bun:"notify_blog_rebuild,notnull,default:true" json:"notify_blog_rebuild"`

	// Policy: "all" or "none" or "followed" - who can trigger notifications
	Policy string `bun:"policy,notnull,default:'all'" json:"policy"`
}

// VAPIDKeyPair is the singleton application-server key used for Web Push.
// It lives in the database so subscriptions survive process restarts.
type VAPIDKeyPair struct {
	bun.BaseModel `bun:"table:vapid_keys"`

	ID         string    `bun:"id,pk,type:text"`
	PublicKey  string    `bun:"public_key,notnull"`
	PrivateKey string    `bun:"private_key,notnull"`
	UpdatedAt  time.Time `bun:"updated_at,notnull"`
}

// MeshIdentity is the singleton Ed25519 key pair that gives this instance its
// littleMesh node ID, and with it the domain its ActivityPub URIs are minted
// from. Losing it means losing the instance's identity, so it belongs in the
// database beside the actor keys and inside the same backups.
type MeshIdentity struct {
	bun.BaseModel `bun:"table:mesh_identity"`

	ID         string    `bun:"id,pk,type:text"`
	NodeID     string    `bun:"node_id,notnull"`
	PrivateKey string    `bun:"private_key,notnull" json:"-"`
	CreatedAt  time.Time `bun:"created_at,notnull"`
}

// WelcomeDirectMessage is the optional instance-wide DM sent to new local users.
// ID is always 1; SenderAccountID records which administrator sends it.
type WelcomeDirectMessage struct {
	bun.BaseModel `bun:"table:welcome_direct_messages"`

	ID              int       `bun:"id,pk"`
	SenderAccountID string    `bun:"sender_account_id,notnull"`
	Content         string    `bun:"content,notnull"`
	UpdatedAt       time.Time `bun:"updated_at,notnull"`
}

// InstanceMeta is the singleton row holding the operator-editable description
// text, rules, and contact address this server advertises. ID is always 1.
//
// It exists so an administrator can change what the About page and the
// instance APIs publish without editing the config file and restarting: every
// text field falls back to its [instance] config value while it is empty, and
// Rules (one per line) falls back to [instance].rules until this row exists at
// all, so an administrator who publishes no rules gets none.
type InstanceMeta struct {
	bun.BaseModel `bun:"table:instance_meta"`

	ID                  int       `bun:"id,pk"`
	ShortDescription    string    `bun:"short_description,notnull"`
	Description         string    `bun:"description,notnull"`
	ExtendedDescription string    `bun:"extended_description,notnull"`
	TermsOfService      string    `bun:"terms_of_service,notnull"`
	ContactEmail        string    `bun:"contact_email,notnull"`
	Rules               string    `bun:"rules,notnull"`
	UpdatedAt           time.Time `bun:"updated_at,notnull"`
}

// BubbleInstance represents a trusted remote instance whose posts appear in the Bubble timeline.
type BubbleInstance struct {
	ID        string    `bun:"id,pk,type:text" json:"id"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`
	Domain    string    `bun:"domain,notnull,unique" json:"domain"`
	Note      string    `bun:"note" json:"note"`
}

// Tombstone represents a deleted ActivityPub object. Local deletions are served
// as 410 responses; remote actor tombstones prevent deleted accounts from reappearing.
type Tombstone struct {
	bun.BaseModel `bun:"table:tombstones"`

	ID         string    `bun:"id,pk,type:text" json:"id"`
	URI        string    `bun:"uri,unique,notnull" json:"uri"`
	FormerType string    `bun:"former_type,notnull" json:"former_type"` // "Note", "Person", …
	DeletedAt  time.Time `bun:"deleted_at,notnull" json:"deleted_at"`
	CreatedAt  time.Time `bun:"created_at,notnull" json:"created_at"`
}

// PasswordReset represents a password reset token.
type PasswordReset struct {
	ID        string    `bun:"id,pk,type:text" json:"id"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	ExpiresAt time.Time `bun:"expires_at,notnull" json:"expires_at"`
	AccountID string    `bun:"account_id,notnull" json:"account_id"`
	TokenHash string    `bun:"token_hash,unique,notnull" json:"-"` // SHA-256 hash of the reset token
	Used      bool      `bun:"used,notnull,default:false" json:"-"`
}

// EmailVerification represents a pending email verification token.
type EmailVerification struct {
	bun.BaseModel `bun:"table:email_verifications"`
	ID            string    `bun:"id,pk,type:text" json:"id"`
	CreatedAt     time.Time `bun:"created_at,notnull" json:"created_at"`
	ExpiresAt     time.Time `bun:"expires_at,notnull" json:"expires_at"`
	AccountID     string    `bun:"account_id,notnull" json:"account_id"`
	Email         string    `bun:"email,notnull" json:"email"`
	TokenHash     string    `bun:"token_hash,notnull" json:"-"`
	Used          bool      `bun:"used,notnull,default:false" json:"-"`
}

// EmailDigestState tracks when the last notification digest was sent to an account.
type EmailDigestState struct {
	bun.BaseModel `bun:"table:email_digest_state"`
	AccountID     string    `bun:"account_id,pk,type:text" json:"account_id"`
	LastSentAt    time.Time `bun:"last_sent_at,notnull" json:"last_sent_at"`
}

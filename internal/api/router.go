// Package api implements the Mastodon-compatible REST API.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/app"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/federation"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mail"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media/objectstore"
	accountmigration "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/migration"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/oauth"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/statusfilter"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// Config holds configuration for the API handler.
type Config struct {
	Domain                      string
	LocalDomain                 string
	WebDomain                   string
	Secure                      bool
	UserAgent                   string
	MediaPath                   string
	MaxMediaSize                int64
	MaxImagePixels              int64
	MaxMedia                    int
	RenameMedia                 bool
	StripEXIF                   bool
	MigrationConfigured         bool
	MigrationEnabled            bool
	MigrationMaxFollowsImport   int
	MigrationImportRate         time.Duration
	MigrationCooldownAfterMove  time.Duration
	MigrationRequireTargetAlias bool
	MediaProxy                  *media.Proxy
	MediaStore                  objectstore.Store
	// InstanceName is the human-readable name advertised to clients.
	InstanceName string
	// MaxStatusChars is the maximum status length advertised to clients.
	MaxStatusChars int
	// InstanceRules are returned consistently by the v1/v2 instance APIs.
	InstanceRules []string
	// InstanceShortDescription through InstanceContactEmail seed the instance
	// profile; the admin-edited database row overrides each of them. See
	// instancemeta.Load.
	InstanceShortDescription    string
	InstanceDescription         string
	InstanceExtendedDescription string
	InstanceTermsOfService      string
	InstanceContactEmail        string
	// DefaultLocale is the fallback UI/email/push language for a new
	// registration whose requested locale isn't supported.
	DefaultLocale string
	// BlogAllAccounts mirrors [blog] all_accounts: every local account may
	// publish to the static blog, with no per-account grant.
	BlogAllAccounts                         bool
	BlockSensitiveRelationshipNotifications bool
}

// Handler is the main API handler, organizing all endpoint groups.
type Handler struct {
	// vapidPublicKey is the Web Push application-server key, returned to
	// clients as a subscription's server_key.
	vapidPublicKey string

	store          storage.Storage
	federator      *federation.Federator
	threadResolver *federation.ThreadResolver
	resolveStatus  func(context.Context, string) (*models.Status, error)
	interactions   *federation.InteractionResolver
	app            *app.Service
	domain         string
	localDomain    string
	mediaProxy     *media.Proxy
	config         Config
	// Runtime config populated from main.go
	Registrations     bool
	InviteRequired    bool
	RequireApproval   bool
	RequireEmail      bool
	RequireAdminMFA   bool
	MinPasswordLength int
	ContactUsername   string

	// mailer sends email notifications (nil if SMTP not configured)
	mailer *mail.Mailer

	// viewerTouch reports an authenticated request to the optional cache
	// warmer (nil when warming is disabled).
	viewerTouch func(accountID, path string)
}

// SetInboxProcessor wires on-demand remote status resolution.
func (h *Handler) SetInboxProcessor(p *federation.InboxProcessor) {
	if p != nil {
		h.resolveStatus = p.ResolveRemoteStatusByURL
	}
}

// SetViewerTouch wires the optional cache warmer's activity tracking; called
// with the account ID and request path of every authenticated request. Nil
// disables tracking.
func (h *Handler) SetViewerTouch(touch func(accountID, path string)) {
	h.viewerTouch = touch
}

// SetMailer wires the email sender (nil to disable email notifications).
func (h *Handler) SetMailer(m *mail.Mailer) {
	h.mailer = m
}

// SetVAPIDPublicKey sets the Web Push key advertised to API clients.
func (h *Handler) SetVAPIDPublicKey(key string) {
	h.vapidPublicKey = key
}

// NewHandler creates a new API handler.
func NewHandler(store storage.Storage, federator *federation.Federator, config Config, resolvers ...*federation.ThreadResolver) *Handler {
	if config.MediaStore == nil {
		config.MediaStore = objectstore.NewLocal(config.MediaPath, "/media/")
	}
	localDomain := config.LocalDomain
	if localDomain == "" {
		localDomain = config.Domain
	}
	webDomain := config.WebDomain
	if webDomain == "" {
		webDomain = localDomain
	}
	h := &Handler{
		store:        store,
		federator:    federator,
		interactions: federation.NewInteractionResolver(store, federator),
		app:          app.NewWithDomainsAndMediaPath(store, federator, localDomain, webDomain, config.MediaPath),
		domain:       webDomain,
		localDomain:  localDomain,
		mediaProxy:   config.MediaProxy,
		config:       config,
	}
	h.app.SetMediaStore(config.MediaStore)
	h.app.SetBlogAllAccounts(config.BlogAllAccounts)
	h.app.SetBlockSensitiveRelationshipNotifications(config.BlockSensitiveRelationshipNotifications)
	if len(resolvers) > 0 {
		h.threadResolver = resolvers[0]
	}
	return h
}

// RegisterRoutes registers all Mastodon API routes on the given router.
func (h *Handler) RegisterRoutes(r chi.Router, oauthHandler *oauth.Handler) {
	// OAuth2 routes (no auth required)
	oauthHandler.RegisterRoutes(r)

	// Instance routes (no auth required for public info)
	r.Get("/api/v1/instance", h.InstanceV1)
	r.Get("/api/v2/instance", h.InstanceV2)
	r.Get("/api/v1/instance/peers", h.InstancePeers)
	r.Get("/api/v1/instance/domain_blocks", h.InstanceDomainBlocks)
	r.Get("/api/v1/instance/rules", h.InstanceRules)
	r.Get("/api/v1/instance/languages", h.InstanceLanguages)
	r.Get("/api/v1/instance/translation_languages", h.InstanceTranslationLanguages)
	r.Get("/api/v1/instance/extended_description", h.InstanceExtendedDescription)
	r.Get("/api/v1/instance/terms_of_service", h.InstanceTermsOfService)

	// Registration endpoint (no auth required)
	r.Post("/api/v1/accounts", h.AccountCreate)

	// Public timeline (no auth required)
	r.With(h.OptionalOAuthMiddleware).Get("/api/v1/timelines/public", h.TimelinePublic)

	// Bubble timeline (no auth required - shows local + bubble instance posts)
	r.With(h.OptionalOAuthMiddleware).Get("/api/v1/timelines/bubble", h.TimelineBubble)

	// Hashtag timeline (no auth required - public tag timeline)
	r.With(h.OptionalOAuthMiddleware).Get("/api/v1/timelines/tag/{tag}", h.TimelineTag)
	r.With(h.OptionalOAuthMiddleware).Get("/api/v1/polls/{id}", h.PollGet)
	r.With(h.OptionalOAuthMiddleware).Get("/api/v1/statuses", h.StatusesGet)
	r.With(h.OptionalOAuthMiddleware).Get("/api/v1/announcements", h.Announcements)
	// Trends answer with an empty array on purpose. There is no instance-wide
	// popularity feature and none is planned; clients treat an empty list as
	// "nothing trending" rather than as an error.
	r.Get("/api/v1/trends", h.EmptyArray)
	r.Get("/api/v1/trends/statuses", h.EmptyArray)
	r.Get("/api/v1/trends/tags", h.EmptyArray)
	r.Get("/api/v1/trends/links", h.EmptyArray)
	r.Get("/api/v1/custom_emojis", h.CustomEmojis)

	// Authenticated routes
	r.Group(func(r chi.Router) {
		r.Use(h.OAuthMiddleware)

		// Accounts
		r.Get("/api/v1/accounts/verify_credentials", h.AccountVerifyCredentials)
		r.Patch("/api/v1/accounts/update_credentials", h.AccountUpdateCredentials)
		r.Get("/api/v1/accounts/{id}", h.AccountGet)
		r.Get("/api/v1/accounts/{id}/statuses", h.AccountStatuses)
		r.Get("/api/v1/accounts/{id}/followers", h.AccountFollowers)
		r.Get("/api/v1/accounts/{id}/following", h.AccountFollowing)
		r.Post("/api/v1/accounts/{id}/follow", h.AccountFollow)
		r.Post("/api/v1/accounts/{id}/unfollow", h.AccountUnfollow)
		r.Post("/api/v1/accounts/{id}/block", h.AccountBlock)
		r.Post("/api/v1/accounts/{id}/unblock", h.AccountUnblock)
		r.Post("/api/v1/accounts/{id}/mute", h.AccountMute)
		r.Post("/api/v1/accounts/{id}/unmute", h.AccountUnmute)
		r.Get("/api/v1/accounts/relationships", h.AccountRelationships)
		r.Get("/api/v1/accounts/{id}/lists", h.AccountLists)

		// Statuses
		r.Post("/api/v1/statuses", h.StatusCreate)
		r.Get("/api/v1/statuses/{id}", h.StatusGet)
		r.Put("/api/v1/statuses/{id}", h.StatusEdit)
		r.Delete("/api/v1/statuses/{id}", h.StatusDelete)
		r.Get("/api/v1/statuses/{id}/source", h.StatusSource)
		r.Get("/api/v1/statuses/{id}/context", h.StatusContext)
		r.Get("/api/v1/statuses/{id}/reblogged_by", h.StatusRebloggedBy)
		r.Get("/api/v1/statuses/{id}/favourited_by", h.StatusFavouritedBy)
		r.Post("/api/v1/statuses/{id}/favourite", h.StatusFavourite)
		r.Post("/api/v1/statuses/{id}/unfavourite", h.StatusUnfavourite)
		r.Put("/api/v1/pleroma/statuses/{id}/reactions/{emoji}", h.StatusEmojiReactionAdd)
		r.Delete("/api/v1/pleroma/statuses/{id}/reactions/{emoji}", h.StatusEmojiReactionRemove)
		r.Get("/api/v1/pleroma/statuses/{id}/reactions", h.StatusEmojiReactions)
		r.Get("/api/v1/pleroma/statuses/{id}/reactions/{emoji}", h.StatusEmojiReactions)
		r.Put("/api/v1/statuses/{id}/emoji_reactions/{emoji}", h.StatusEmojiReactionAdd)
		r.Delete("/api/v1/statuses/{id}/emoji_reactions/{emoji}", h.StatusEmojiReactionRemove)
		r.Post("/api/v1/statuses/{id}/reblog", h.StatusReblog)
		r.Post("/api/v1/statuses/{id}/unreblog", h.StatusUnreblog)
		r.Post("/api/v1/statuses/{id}/bookmark", h.StatusBookmark)
		r.Post("/api/v1/statuses/{id}/unbookmark", h.StatusUnbookmark)
		r.Post("/api/v1/statuses/{id}/pin", h.StatusPin)
		r.Post("/api/v1/statuses/{id}/unpin", h.StatusUnpin)
		r.Post("/api/v1/polls/{id}/votes", h.PollVote)

		// Timelines
		r.Get("/api/v1/timelines/home", h.TimelineHome)
		r.Get("/api/v1/timelines/list/{id}", h.TimelineList)

		// Notifications
		r.Get("/api/v1/notifications", h.NotificationsList)
		r.Get("/api/v1/notifications/unread_count", h.NotificationsUnreadCount)
		r.Get("/api/v1/notifications/{id}", h.NotificationGet)
		r.Post("/api/v1/notifications/clear", h.NotificationsClear)
		r.Post("/api/v1/notifications/{id}/dismiss", h.NotificationDismiss)
		r.Get("/api/v2/notifications", h.GroupedNotificationsList)
		r.Get("/api/v2/notifications/policy", h.NotificationPolicyGet)

		// Push subscriptions
		r.Get("/api/v1/push/subscription", h.PushSubscriptionGet)
		r.Post("/api/v1/push/subscription", h.PushSubscriptionCreate)
		r.Put("/api/v1/push/subscription", h.PushSubscriptionUpdate)
		r.Delete("/api/v1/push/subscription", h.PushSubscriptionDelete)

		// Markers
		r.Get("/api/v1/markers", h.MarkersGet)
		r.Post("/api/v1/markers", h.MarkersSave)

		// Favourites (liked statuses)
		r.Get("/api/v1/favourites", h.FavouritesList)

		// Bookmarks
		r.Get("/api/v1/bookmarks", h.BookmarksList)

		// Account migration
		r.Post("/api/v1/accounts/{id}/move", h.AccountMove)
		r.Post("/api/v1/accounts/{id}/import_follows", h.AccountImportFollows)
		r.Get("/api/v1/accounts/{id}/following.csv", h.AccountExportFollowsCSV)
		r.Post("/api/v1/accounts/{id}/import_follows_csv", h.AccountImportFollowsCSV)
		r.Get("/api/v1/accounts/{id}/import_follows_csv/{import_id}", h.AccountFollowImportGet)

		// Follow requests
		r.Get("/api/v1/follow_requests", h.FollowRequestsList)
		r.Post("/api/v1/follow_requests/{id}/authorize", h.FollowRequestAuthorize)
		r.Post("/api/v1/follow_requests/{id}/reject", h.FollowRequestReject)

		// Blocks/Mutes
		r.Get("/api/v1/blocks", h.BlocksList)
		r.Get("/api/v1/mutes", h.MutesList)

		// Per-user domain blocks
		r.Get("/api/v1/domain_blocks", h.DomainBlocksList)
		r.Post("/api/v1/domain_blocks", h.DomainBlockCreate)
		r.Delete("/api/v1/domain_blocks", h.DomainBlockDelete)

		// Reports
		r.Post("/api/v1/reports", h.ReportCreate)

		// Media
		r.Post("/api/v1/media", h.MediaUpload)
		r.Post("/api/v2/media", h.MediaUploadV2)
		r.Put("/api/v1/media/{id}", h.MediaUpdate)
		r.Get("/api/v1/media/{id}", h.MediaGet)

		// Search
		r.Get("/api/v2/search", h.Search)
		r.Get("/api/v1/search", h.SearchV1)

		// Lists
		r.Get("/api/v1/lists", h.ListsList)
		r.Post("/api/v1/lists", h.ListCreate)
		r.Get("/api/v1/lists/{id}", h.ListGet)
		r.Put("/api/v1/lists/{id}", h.ListUpdate)
		r.Delete("/api/v1/lists/{id}", h.ListDelete)
		r.Get("/api/v1/lists/{id}/accounts", h.ListAccounts)
		r.Post("/api/v1/lists/{id}/accounts", h.ListAccountsAdd)
		r.Delete("/api/v1/lists/{id}/accounts", h.ListAccountsRemove)

		// Preferences
		r.Get("/api/v1/preferences", h.Preferences)
		r.Get("/api/v1/conversations", h.Conversations)
		// Suggestions and endorsements are empty for the same reason as trends:
		// the instance does not rank accounts for anyone.
		r.Get("/api/v1/suggestions", h.EmptyArray)
		r.Get("/api/v2/suggestions", h.EmptyArray)
		r.Delete("/api/v1/suggestions/{id}", h.EmptyArray)
		r.Get("/api/v1/endorsements", h.EmptyArray)
		r.Get("/api/v1/scheduled_statuses", h.ScheduledStatuses)
		r.Get("/api/v1/scheduled_statuses/{id}", h.ScheduledStatusGet)
		r.Put("/api/v1/scheduled_statuses/{id}", h.ScheduledStatusUpdate)
		r.Delete("/api/v1/scheduled_statuses/{id}", h.ScheduledStatusDelete)
		r.Get("/api/v1/filters", h.FiltersList)
		r.Get("/api/v2/filters", h.FiltersList)
		r.Post("/api/v2/filters", h.FilterCreate)
		r.Get("/api/v2/filters/{id}", h.FilterGet)
		r.Put("/api/v2/filters/{id}", h.FilterUpdate)
		r.Delete("/api/v2/filters/{id}", h.FilterDelete)

		// Featured tags
		r.Get("/api/v1/featured_tags", h.FeaturedTags)
		r.Post("/api/v1/featured_tags", h.FeaturedTagCreate)
		r.Delete("/api/v1/featured_tags/{id}", h.FeaturedTagDelete)
		r.Get("/api/v1/accounts/{id}/featured_tags", h.AccountFeaturedTags)

		// Hashtags (view + follow/unfollow + followed list)
		r.Get("/api/v1/followed_tags", h.FollowedTagsList)
		r.Get("/api/v1/tags/{id}", h.TagGet)
		r.Post("/api/v1/tags/{id}/follow", h.TagFollow)
		r.Post("/api/v1/tags/{id}/unfollow", h.TagUnfollow)
	})

	// Admin API routes (require authentication + admin privileges)
	r.Group(func(r chi.Router) {
		r.Use(h.OAuthMiddleware)
		r.Use(h.AdminMiddleware)

		// Account management
		r.Get("/api/v1/admin/accounts", h.AdminAccountsList)
		r.Get("/api/v1/admin/accounts/{id}", h.AdminAccountGet)
		r.Post("/api/v1/admin/accounts/{id}/action", h.AdminAccountAction)
		r.Post("/api/v1/admin/accounts/{id}/approve", h.AdminAccountApprove)
		r.Post("/api/v1/admin/accounts/{id}/reject", h.AdminAccountReject)

		// Reports
		r.Get("/api/v1/admin/reports", h.AdminReportsList)
		r.Get("/api/v1/admin/reports/{id}", h.AdminReportGet)
		r.Post("/api/v1/admin/reports/{id}/resolve", h.AdminReportResolve)

		// Domain blocks
		r.Get("/api/v1/admin/domain_blocks", h.AdminDomainBlocksList)
		r.Post("/api/v1/admin/domain_blocks", h.AdminDomainBlockCreate)
		r.Delete("/api/v1/admin/domain_blocks/{id}", h.AdminDomainBlockDelete)
		r.Get("/api/v1/admin/domain_allows", h.AdminDomainAllowsList)
		r.Get("/api/v1/admin/domain_allows/{id}", h.AdminDomainAllowGet)
		r.Post("/api/v1/admin/domain_allows", h.AdminDomainAllowCreate)
		r.Delete("/api/v1/admin/domain_allows/{id}", h.AdminDomainAllowDelete)

		// Invites
		r.Get("/api/v1/admin/invites", h.AdminInvitesList)
		r.Post("/api/v1/admin/invites", h.AdminInvitesCreate)
		r.Delete("/api/v1/admin/invites/{id}", h.AdminInvitesRevoke)

		// Bubble instances
		r.Get("/api/v1/admin/bubble_instances", h.AdminBubbleInstancesList)
		r.Post("/api/v1/admin/bubble_instances", h.AdminBubbleInstanceCreate)
		r.Delete("/api/v1/admin/bubble_instances/{id}", h.AdminBubbleInstanceDelete)

		// Account migration (admin force-move)
		r.Post("/api/v1/admin/accounts/{id}/move", h.AdminAccountMove)

		// Instance health
		r.Get("/api/v1/admin/instances", h.AdminInstancesList)
		r.Post("/api/v1/admin/instances/{id}/reset", h.AdminInstanceReset)
		r.Post("/api/v1/admin/instances/{id}/suspend", h.AdminInstanceToggleSuspend)
		r.Post("/api/v1/admin/instances/{id}/purge", h.AdminInstancePurge)
	})
}

// API Types

// MastodonAccount is the Mastodon API representation of an Account.
type MastodonAccount struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	Acct        string `json:"acct"`
	DisplayName string `json:"display_name"`
	Locked      bool   `json:"locked"`
	Bot         bool   `json:"bot"`
	// discoverable and last_status_at are documented as always present and
	// nullable. Omitting the keys entirely breaks strictly-typed clients:
	// a Swift/Kotlin decoder raises "key not found" and the whole account
	// fails to decode, which is indistinguishable from a rejected login.
	Discoverable   *bool                  `json:"discoverable"`
	Group          bool                   `json:"group"`
	CreatedAt      string                 `json:"created_at"`
	Note           string                 `json:"note"`
	URL            string                 `json:"url"`
	Avatar         string                 `json:"avatar"`
	AvatarStatic   string                 `json:"avatar_static"`
	Header         string                 `json:"header"`
	HeaderStatic   string                 `json:"header_static"`
	FollowersCount int                    `json:"followers_count"`
	FollowingCount int                    `json:"following_count"`
	StatusesCount  int                    `json:"statuses_count"`
	LastStatusAt   *string                `json:"last_status_at"`
	Emojis         []MastodonEmoji        `json:"emojis"`
	Fields         []map[string]string    `json:"fields"`
	Roles          []MastodonRole         `json:"roles"`
	Moved          *MastodonAccount       `json:"moved,omitempty"`
	AlsoKnownAs    []string               `json:"also_known_as,omitempty"`
	Source         *MastodonAccountSource `json:"source,omitempty"`
}

// MastodonRole is a role badge shown next to an account.
type MastodonRole struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

// MastodonAccountSource is the credential account source info.
type MastodonAccountSource struct {
	Note                string   `json:"note"`
	Fields              []string `json:"fields"`
	Privacy             string   `json:"privacy"`
	Sensitive           bool     `json:"sensitive"`
	Language            string   `json:"language"`
	FollowRequestsCount int      `json:"follow_requests_count"`
}

// MastodonStatus is the Mastodon API representation of a Status.
type MastodonStatus struct {
	ID                 string                  `json:"id"`
	URI                string                  `json:"uri"`
	CreatedAt          string                  `json:"created_at"`
	EditedAt           *string                 `json:"edited_at"`
	Account            *MastodonAccount        `json:"account"`
	Content            string                  `json:"content"`
	Visibility         string                  `json:"visibility"`
	Sensitive          bool                    `json:"sensitive"`
	SpoilerText        string                  `json:"spoiler_text"`
	MediaAttachments   []MastodonAttachment    `json:"media_attachments"`
	Mentions           []MastodonMention       `json:"mentions"`
	Tags               []MastodonTag           `json:"tags"`
	Emojis             []MastodonEmoji         `json:"emojis"`
	ReblogsCount       int                     `json:"reblogs_count"`
	FavouritesCount    int                     `json:"favourites_count"`
	RepliesCount       int                     `json:"replies_count"`
	URL                string                  `json:"url"`
	InReplyToID        *string                 `json:"in_reply_to_id"`
	InReplyToAccountID *string                 `json:"in_reply_to_account_id"`
	Reblog             *MastodonStatus         `json:"reblog"`
	Pinned             bool                    `json:"pinned"`
	Bookmarked         bool                    `json:"bookmarked"`
	Favourited         bool                    `json:"favourited"`
	Reblogged          bool                    `json:"reblogged"`
	Muted              bool                    `json:"muted"`
	Card               *MastodonCard           `json:"card"`
	Application        map[string]string       `json:"application"`
	Language           *string                 `json:"language"`
	Quote              *MastodonQuote          `json:"quote"`
	Poll               *models.Poll            `json:"poll"`
	Filtered           []models.FilterMatch    `json:"filtered"`
	EmojiReactions     []MastodonEmojiReaction `json:"emoji_reactions"`
	Pleroma            map[string]interface{}  `json:"pleroma"`
	// ExpiresAt is a littleFedi extension (not part of the Mastodon API):
	// when set, the instance will auto-delete this status around this time.
	ExpiresAt *string `json:"expires_at,omitempty"`
}

type MastodonEmojiReaction struct {
	Name       string   `json:"name"`
	Count      int      `json:"count"`
	Me         bool     `json:"me"`
	AccountIDs []string `json:"account_ids"`
	URL        string   `json:"url,omitempty"`
	StaticURL  string   `json:"static_url,omitempty"`
}

// MastodonQuote is the Mastodon 4.4 Quote entity embedded in a status that
// quotes another. State mirrors the FEP-044f approval state.
type MastodonQuote struct {
	State        string          `json:"state"`
	QuotedStatus *MastodonStatus `json:"quoted_status"`
}

// MastodonAttachment represents a media attachment in the API.
type MastodonAttachment struct {
	ID          string      `json:"id"`
	Type        string      `json:"type"`
	URL         string      `json:"url"`
	PreviewURL  *string     `json:"preview_url"`
	RemoteURL   *string     `json:"remote_url,omitempty"`
	TextURL     *string     `json:"text_url,omitempty"`
	Meta        interface{} `json:"meta,omitempty"`
	Description *string     `json:"description"`
	Blurhash    string      `json:"blurhash,omitempty"`
}

// MastodonList represents a list in the Mastodon API.
type MastodonList struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	RepliesPolicy string `json:"replies_policy"`
	Exclusive     bool   `json:"exclusive"`
}

// MastodonMention represents a mention in the API.
type MastodonMention struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	URL      string `json:"url"`
	Acct     string `json:"acct"`
}

// MastodonEmoji is the API representation of a custom emoji. Clients render the
// `:shortcode:` occurrences in content/display names using these URLs. The URLs
// are proxy/cache-aware (rewritten at serialization time).
type MastodonEmoji struct {
	Shortcode       string `json:"shortcode"`
	URL             string `json:"url"`
	StaticURL       string `json:"static_url"`
	VisibleInPicker bool   `json:"visible_in_picker"`
}

// MastodonTag represents a hashtag in the API. Mastodon uses the tag name as
// its id. Following is populated only when a viewer context is available.
type MastodonTag struct {
	Name      string               `json:"name"`
	URL       string               `json:"url"`
	History   []MastodonTagHistory `json:"history"`
	Following *bool                `json:"following,omitempty"`
}

// MastodonTagHistory is a daily usage bucket. We don't track per-day usage, so
// it is always returned empty for API-shape compatibility.
type MastodonTagHistory struct {
	Day      string `json:"day"`
	Uses     string `json:"uses"`
	Accounts string `json:"accounts"`
}

// MastodonFeaturedTag is a hashtag pinned to an account's profile.
type MastodonFeaturedTag struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	URL           string  `json:"url"`
	StatusesCount string  `json:"statuses_count"`
	LastStatusAt  *string `json:"last_status_at"`
}

// MastodonNotification represents a notification in the API.
type MastodonNotification struct {
	ID        string           `json:"id"`
	Type      string           `json:"type"`
	CreatedAt string           `json:"created_at"`
	Account   *MastodonAccount `json:"account"`
	Status    *MastodonStatus  `json:"status,omitempty"`
	Read      bool             `json:"read"`
	Emoji     string           `json:"emoji,omitempty"`
	URL       string           `json:"url,omitempty"`
}

// MastodonRelationship represents a relationship between accounts.
type MastodonRelationship struct {
	ID                  string `json:"id"`
	Following           bool   `json:"following"`
	FollowedBy          bool   `json:"followed_by"`
	Blocking            bool   `json:"blocking"`
	BlockedBy           bool   `json:"blocked_by"`
	Muting              bool   `json:"muting"`
	MutingNotifications bool   `json:"muting_notifications"`
	Requested           bool   `json:"requested"`
	DomainBlocking      bool   `json:"domain_blocking"`
	ShowingReblogs      bool   `json:"showing_reblogs"`
	Notifying           bool   `json:"notifying"`
	Endorsed            bool   `json:"endorsed"`
	Note                string `json:"note"`
}

// MastodonInstanceV1 represents the /api/v1/instance response.
type MastodonInstanceV1 struct {
	URI              string            `json:"uri"`
	Title            string            `json:"title"`
	ShortDescription string            `json:"short_description"`
	Description      string            `json:"description"`
	Email            string            `json:"email"`
	Version          string            `json:"version"`
	URLs             map[string]string `json:"urls"`
	Stats            map[string]int    `json:"stats"`
	Thumbnail        *string           `json:"thumbnail,omitempty"`
	Languages        []string          `json:"languages"`
	Registrations    bool              `json:"registrations"`
	ApprovalRequired bool              `json:"approval_required"`
	InvitesEnabled   bool              `json:"invites_enabled"`
	ContactAccount   *MastodonAccount  `json:"contact_account,omitempty"`
	Rules            []MastodonRule    `json:"rules"`
	// Configuration mirrors the v2 instance configuration block; clients read
	// statuses.max_characters from here. MaxTootChars is the legacy top-level
	// field still used by some older clients.
	Configuration map[string]interface{} `json:"configuration"`
	MaxTootChars  int                    `json:"max_toot_chars"`
}

// PaginationParams holds pagination parameters from API queries.
type PaginationParams struct {
	MaxID   *string
	MinID   *string
	SinceID *string
	Limit   int
}

// Pagination

func parsePagination(r *http.Request) PaginationParams {
	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 80 {
			limit = n
		}
	}

	p := PaginationParams{Limit: limit}

	if v := r.URL.Query().Get("max_id"); v != "" {
		p.MaxID = &v
	}
	if v := r.URL.Query().Get("min_id"); v != "" {
		p.MinID = &v
	}
	if v := r.URL.Query().Get("since_id"); v != "" {
		p.SinceID = &v
	}

	return p
}

func setLinkHeader(w http.ResponseWriter, path string, params PaginationParams, statuses []*models.Status) {
	if len(statuses) == 0 {
		return
	}
	setLinkHeaderIDs(w, path, params, statuses[0].ID, statuses[len(statuses)-1].ID, len(statuses))
}

// setLinkHeaderIDs emits the pagination Link header from the first/last status
// IDs, so a cached serialized page can set headers without the status slice.
func setLinkHeaderIDs(w http.ResponseWriter, path string, params PaginationParams, firstID, lastID string, count int) {
	if count == 0 {
		return
	}

	links := []string{}
	if lastID != "" {
		links = append(links, fmt.Sprintf(`<%s?max_id=%s&limit=%d>; rel="next"`, path, lastID, params.Limit))
	}
	if firstID != "" {
		links = append(links, fmt.Sprintf(`<%s?min_id=%s&limit=%d>; rel="prev"`, path, firstID, params.Limit))
	}
	if len(links) > 0 {
		w.Header().Set("Link", strings.Join(links, ", "))
	}
}

// Auth Middleware

type contextKey string

const (
	authAccountKey contextKey = "auth_account"
	authScopesKey  contextKey = "auth_scopes"
	authTokenIDKey contextKey = "auth_token_id"
	authAppIDKey   contextKey = "auth_app_id"
)

// OAuthMiddleware extracts and validates the bearer token, setting the auth
// account and scopes on the request context. GET/HEAD requests need a read
// scope and mutating requests need a write scope.
func (h *Handler) OAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			writeAPIError(w, http.StatusUnauthorized, "missing authorization header")
			return
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == authHeader {
			// Also accept "token " prefix (some client libs)
			token = strings.TrimPrefix(authHeader, "token ")
			if token == authHeader {
				writeAPIError(w, http.StatusUnauthorized, "invalid authorization header format")
				return
			}
		}

		oauthToken, err := h.store.GetOAuthTokenByAccess(r.Context(), token)
		if err != nil {
			slog.Warn("api: invalid token", "error", err)
			writeAPIError(w, http.StatusUnauthorized, "invalid access token")
			return
		}
		if oauthToken.AuthorizationCode {
			writeAPIError(w, http.StatusUnauthorized, "invalid access token")
			return
		}

		// Check token expiry
		if oauthToken.ExpiresAt != nil && time.Now().After(*oauthToken.ExpiresAt) {
			slog.Warn("api: token expired", "account_id", oauthToken.AccountID)
			writeAPIError(w, http.StatusUnauthorized, "token expired")
			return
		}

		account, err := h.store.GetAccount(r.Context(), oauthToken.AccountID)
		if err != nil {
			slog.Error("api: token account not found", "error", err)
			writeAPIError(w, http.StatusUnauthorized, "account not found")
			return
		}
		if !account.CanAuthenticate(h.RequireEmail, h.RequireAdminMFA) {
			writeAPIError(w, http.StatusForbidden, "account is not permitted to authenticate")
			return
		}

		tokenScopes := parseScopes(oauthToken.Scopes)
		requiredScopes := scopesForMethod(r.Method, r.URL.Path)
		if !hasAnyScope(tokenScopes, requiredScopes) {
			slog.Warn("api: insufficient scopes",
				"token_scopes", tokenScopes,
				"required", requiredScopes,
				"path", r.URL.Path,
			)
			writeAPIError(w, http.StatusForbidden, "insufficient scopes")
			return
		}

		if h.viewerTouch != nil {
			h.viewerTouch(account.ID, r.URL.Path)
		}
		ctx := context.WithValue(r.Context(), authAccountKey, account)
		ctx = context.WithValue(ctx, authScopesKey, oauthToken.Scopes)
		ctx = context.WithValue(ctx, authTokenIDKey, oauthToken.ID)
		ctx = context.WithValue(ctx, authAppIDKey, oauthToken.ApplicationID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// OptionalOAuthMiddleware authenticates requests that include a bearer token,
// while preserving anonymous access to public endpoints.
func (h *Handler) OptionalOAuthMiddleware(next http.Handler) http.Handler {
	protected := h.OAuthMiddleware(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			next.ServeHTTP(w, r)
			return
		}
		protected.ServeHTTP(w, r)
	})
}

// getAuthAccount retrieves the authenticated account from the request context.
func getAuthAccount(ctx context.Context) *models.Account {
	account, ok := ctx.Value(authAccountKey).(*models.Account)
	if !ok {
		return nil
	}
	return account
}

// getAuthTokenID retrieves the authenticated OAuth token's ID from the
// request context. Used to scope push subscriptions per app login rather
// than per account.
func getAuthTokenID(ctx context.Context) string {
	id, _ := ctx.Value(authTokenIDKey).(string)
	return id
}

// getAuthAppID retrieves the authenticated OAuth token's application ID from
// the request context.
func getAuthAppID(ctx context.Context) string {
	id, _ := ctx.Value(authAppIDKey).(string)
	return id
}

// getAuthScopes retrieves the OAuth scopes from the request context.
func getAuthScopes(ctx context.Context) string {
	scopes, _ := ctx.Value(authScopesKey).(string)
	return scopes
}

// parseScopes splits a space-separated scope string into a set.
func parseScopes(scopes string) map[string]bool {
	set := make(map[string]bool)
	for _, s := range strings.Fields(scopes) {
		set[s] = true
	}
	return set
}

// hasAnyScope returns true if the token has at least one of the required scopes.
func hasAnyScope(tokenScopes map[string]bool, required []string) bool {
	if len(required) == 0 {
		return true // no requirement
	}
	for _, r := range required {
		if tokenScopes[r] {
			return true
		}
	}
	return false
}

// scopesForMethod returns the required scopes for a given HTTP method and path.
// GET/HEAD requests primarily need 'read'. Mutations need 'write'.
// Admin paths additionally need 'admin:read' or 'admin:write'.
func scopesForMethod(method, path string) []string {
	isAdmin := strings.HasPrefix(path, "/api/v1/admin/")
	isRead := method == "GET" || method == "HEAD" || method == "OPTIONS"

	switch {
	case strings.HasPrefix(path, "/api/v1/admin/domain_allows") && isRead:
		return []string{"admin:read:domain_allows", "admin:read"}
	case strings.HasPrefix(path, "/api/v1/admin/domain_allows"):
		return []string{"admin:write:domain_allows", "admin:write"}
	case isAdmin && isRead:
		return []string{"admin:read"}
	case isAdmin && !isRead:
		return []string{"admin:write"}
	}
	if strings.HasPrefix(path, "/api/v1/push/subscription") {
		return []string{"push"}
	}
	if isRead {
		required := []string{"read"}
		switch {
		case strings.Contains(path, "/notifications"):
			return append(required, "read:notifications")
		case strings.Contains(path, "/bookmarks"):
			return append(required, "read:bookmarks")
		case strings.Contains(path, "/favourites"):
			return append(required, "read:favourites")
		case strings.Contains(path, "/filters"):
			return append(required, "read:filters")
		case strings.Contains(path, "/lists"):
			return append(required, "read:lists")
		case strings.Contains(path, "/mutes"):
			return append(required, "read:mutes")
		case strings.Contains(path, "/blocks"):
			return append(required, "read:blocks")
		case strings.Contains(path, "/follow"), strings.Contains(path, "/relationships"), strings.Contains(path, "/featured_tags"), strings.Contains(path, "/tags"):
			return append(required, "read:follows")
		case strings.Contains(path, "/search"):
			return append(required, "read:search")
		case strings.Contains(path, "/statuses"), strings.Contains(path, "/timelines"), strings.Contains(path, "/polls"), strings.Contains(path, "/markers"), strings.Contains(path, "/conversations"):
			return append(required, "read:statuses")
		default:
			return append(required, "read:accounts")
		}
	}

	required := []string{"write"}
	switch {
	case strings.Contains(path, "/reactions/"), strings.Contains(path, "/emoji_reactions/"):
		return append(required, "write:favourites")
	case strings.Contains(path, "/media"):
		return append(required, "write:media")
	case strings.HasSuffix(path, "/favourite"), strings.HasSuffix(path, "/unfavourite"):
		return append(required, "write:favourites")
	case strings.HasSuffix(path, "/bookmark"), strings.HasSuffix(path, "/unbookmark"):
		return append(required, "write:bookmarks")
	case strings.Contains(path, "/follow"), strings.Contains(path, "/import_follows"), strings.Contains(path, "/tags/"):
		return append(required, "write:follows", "follow")
	case strings.Contains(path, "/blocks"), strings.HasSuffix(path, "/block"), strings.HasSuffix(path, "/unblock"):
		return append(required, "write:blocks", "follow")
	case strings.Contains(path, "/mute"):
		return append(required, "write:mutes", "follow")
	case strings.Contains(path, "/lists"):
		return append(required, "write:lists")
	case strings.Contains(path, "/filters"):
		return append(required, "write:filters")
	case strings.Contains(path, "/notifications"):
		return append(required, "write:notifications")
	case strings.Contains(path, "/reports"):
		return append(required, "write:reports")
	case strings.Contains(path, "/statuses"), strings.Contains(path, "/polls"), strings.Contains(path, "/markers"):
		return append(required, "write:statuses")
	case strings.Contains(path, "/accounts"):
		return append(required, "write:accounts")
	default:
		return required
	}
}

// Conversion Helpers

func mastodonTimestamp(t time.Time) string {
	// Ice Cubes requires fractional seconds, including for whole-second dates.
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func (h *Handler) accountToMastodon(account, viewer *models.Account, source bool) *MastodonAccount {
	if account == nil {
		return &MastodonAccount{
			ID:          "0",
			Username:    "unknown",
			Acct:        "unknown",
			DisplayName: "Deleted account",
			CreatedAt:   mastodonTimestamp(time.Now()),
			Emojis:      []MastodonEmoji{},
			Fields:      []map[string]string{},
			Roles:       []MastodonRole{},
		}
	}

	acct := account.Username
	if account.Domain != nil && *account.Domain != "" {
		acct = account.Username + "@" + *account.Domain
	}

	avatar := account.Avatar
	if avatar == "" {
		avatar = account.AvatarRemote
	}
	if avatar == "" {
		avatar = "/avatars/original/missing.png"
	}
	avatar = h.displayMediaURL(avatar)

	header := account.Header
	if header == "" {
		header = account.HeaderRemote
	}
	if header == "" {
		header = "/headers/original/missing.png"
	}
	header = h.displayMediaURL(header)

	url := account.ActorURI
	if account.Domain == nil {
		url = "https://" + h.domain + "/@" + account.Username
	}

	createdAt := mastodonTimestamp(account.CreatedAt)
	followersCount, followingCount, statusesCount := accountCounts(account)
	if app.FollowListHidden(account, viewer, true) {
		followersCount = 0
	}
	if app.FollowListHidden(account, viewer, false) {
		followingCount = 0
	}

	// Custom emojis in the display name / bio. Loaded lazily if enrichment
	// didn't already populate them (nil = not yet loaded; empty = none).
	if account.Emojis == nil {
		h.app.LoadAccountEmojis(context.Background(), account)
	}

	mastodonAcct := &MastodonAccount{
		ID:             account.ID,
		Username:       account.Username,
		Acct:           acct,
		DisplayName:    account.DisplayName,
		Locked:         account.ManuallyApprovesFollowers,
		Bot:            account.IsBot,
		Group:          false,
		CreatedAt:      createdAt,
		Note:           account.Note,
		URL:            url,
		Avatar:         avatar,
		AvatarStatic:   avatar,
		Header:         header,
		HeaderStatic:   header,
		FollowersCount: followersCount,
		FollowingCount: followingCount,
		StatusesCount:  statusesCount,
		Emojis:         h.emojisToMastodon(account.Emojis),
		Fields:         []map[string]string{},
		Roles:          []MastodonRole{},
	}
	// littleFedi has no directory opt-out, so every account it serves is
	// discoverable. last_status_at would cost a query per account in a
	// timeline, so it stays null: the key is what clients decode against.
	discoverable := true
	mastodonAcct.Discoverable = &discoverable

	// Include alsoKnownAs if set (account migrated here from another server)
	if aliases := accountmigration.AccountAliases(account); len(aliases) > 0 {
		mastodonAcct.AlsoKnownAs = aliases
	}

	if source {
		mastodonAcct.Source = &MastodonAccountSource{
			Note:     account.NoteSource,
			Fields:   []string{},
			Privacy:  "public",
			Language: "en",
		}
	}

	// Include movedTo information if this account has moved
	if account.MovedToURI != "" {
		// Build a stub moved account from the URI (we may not have the full account cached)
		movedAcct := h.buildMovedStub(account.MovedToURI)
		mastodonAcct.Moved = movedAcct
	}

	return mastodonAcct
}

// accountCounts returns the follower/following/status counts for API
// serialization. Local accounts use the same denormalized columns as remote
// ones: they are maintained incrementally by the status and follow write
// paths (CreateStatus/DeleteStatus, adjustLocalFollowCountsTx) and repaired
// by RepairLocalAccountCounts, so the three live COUNT(*) queries this used
// to run per serialized account (~45 per timeline rebuild) bought nothing.
func accountCounts(account *models.Account) (int, int, int) {
	if account == nil {
		return 0, 0, 0
	}
	return account.FollowersCount, account.FollowingCount, account.StatusesCount
}

// buildMovedStub creates a minimal MastodonAccount from a movedTo URI.
// This avoids an extra DB lookup on every API call.
func (h *Handler) buildMovedStub(movedToURI string) *MastodonAccount {
	u, err := url.Parse(movedToURI)
	if err != nil {
		return nil
	}

	// Try to extract username from the path
	// Common patterns:
	//   https://domain/users/username
	//   https://domain/@username
	//   https://domain/actor/username
	path := strings.Trim(u.Path, "/")
	parts := strings.Split(path, "/")
	username := ""
	for i, p := range parts {
		if p == "users" && i+1 < len(parts) {
			username = parts[i+1]
			break
		}
		if strings.HasPrefix(p, "@") {
			username = strings.TrimPrefix(p, "@")
			break
		}
	}
	if username == "" && len(parts) > 0 {
		username = parts[len(parts)-1]
	}

	acct := username + "@" + u.Host

	return &MastodonAccount{
		ID:          "0",
		Username:    username,
		Acct:        acct,
		DisplayName: "",
		URL:         movedToURI,
		CreatedAt:   mastodonTimestamp(time.Now()),
		Emojis:      []MastodonEmoji{},
		Fields:      []map[string]string{},
	}
}

// MastodonCard is the link preview of a status, as Mastodon's API serializes
// it. The embed fields stay empty: littleFedi never serves a player, and a
// client that sees html == "" falls back to the plain link.
type MastodonCard struct {
	URL          string  `json:"url"`
	Title        string  `json:"title"`
	Description  string  `json:"description"`
	Type         string  `json:"type"`
	AuthorName   string  `json:"author_name"`
	AuthorURL    string  `json:"author_url"`
	ProviderName string  `json:"provider_name"`
	ProviderURL  string  `json:"provider_url"`
	HTML         string  `json:"html"`
	Width        int     `json:"width"`
	Height       int     `json:"height"`
	Image        *string `json:"image"`
	EmbedURL     string  `json:"embed_url"`
	Blurhash     string  `json:"blurhash"`
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (h *Handler) cardToMastodon(preview *models.StatusPreview) *MastodonCard {
	if preview == nil || preview.URL == "" {
		return nil
	}
	// Prefer the cached copy; missing images must be null for URL decoders.
	var image *string
	if imageURL := h.displayMediaURL(firstNonEmptyString(preview.Image, preview.ImageRemote)); imageURL != "" {
		image = &imageURL
	}
	return &MastodonCard{
		URL:          preview.URL,
		Title:        preview.Title,
		Description:  preview.Description,
		Type:         "link",
		ProviderName: preview.SiteName,
		Image:        image,
	}
}

// mastodonApplication is shared across every serialized status; treat as immutable.
var mastodonApplication = map[string]string{"name": "littlefedi", "website": ""}

func (h *Handler) statusToMastodon(status *models.Status, currentAccount *models.Account) *MastodonStatus {
	createdAt := mastodonTimestamp(status.CreatedAt)
	var editedAt *string
	if status.EditedAt != nil {
		e := mastodonTimestamp(*status.EditedAt)
		editedAt = &e
	}
	var expiresAt *string
	if status.ExpiresAt != nil {
		e := mastodonTimestamp(*status.ExpiresAt)
		expiresAt = &e
	}

	account := h.accountToMastodon(status.Account, currentAccount, false)
	reactions := h.emojiReactionsToMastodon(status.EmojiReactions)

	// Check if this is a reblog (boost)
	if status.BoostOfID != nil {
		// Return the boost wrapper with the original status as reblog field
		reblogStatus := &MastodonStatus{
			ID:                 status.ID,
			URI:                status.URI,
			CreatedAt:          createdAt,
			EditedAt:           editedAt,
			Account:            account,
			Content:            "",
			Visibility:         mastodonVisibility(status.Visibility),
			Sensitive:          false,
			SpoilerText:        "",
			MediaAttachments:   []MastodonAttachment{},
			Mentions:           []MastodonMention{},
			Tags:               []MastodonTag{},
			Emojis:             []MastodonEmoji{},
			ReblogsCount:       status.ReblogsCount,
			FavouritesCount:    status.FavouritesCount,
			RepliesCount:       status.RepliesCount,
			URL:                h.absoluteURL(status.URL),
			InReplyToID:        nil,
			InReplyToAccountID: nil,
			Reblog:             nil,
			Pinned:             status.Pinned,
			Bookmarked:         status.Bookmarked,
			Favourited:         status.Favourited,
			Reblogged:          status.Reblogged,
			Muted:              status.Muted,
			Card:               nil, // the card belongs to the boosted post, carried in Reblog
			Application:        mastodonApplication,
			Language:           mastodonStatusLanguage(status.Language),
			Filtered:           filterResults(status.FilterMatches),
			EmojiReactions:     reactions,
			Pleroma:            map[string]interface{}{"emoji_reactions": reactions},
		}
		// Load the original status if available
		if status.BoostOf != nil {
			reblogStatus.Reblog = h.statusToMastodon(status.BoostOf, currentAccount)
		}
		return reblogStatus
	}

	// Convert media attachments
	attachments := make([]MastodonAttachment, 0, len(status.MediaAttachments))
	for _, m := range status.MediaAttachments {
		attachments = append(attachments, h.mediaAttachmentToMastodon(m))
	}

	// Convert mentions
	mentions := make([]MastodonMention, 0, len(status.Mentions))
	for _, m := range status.Mentions {
		mentions = append(mentions, MastodonMention{
			ID:       m.AccountID,
			Username: m.Username,
			URL:      h.absoluteURL(m.URL),
			Acct:     strings.TrimPrefix(m.Acct, "@"),
		})
	}

	// Convert tags
	tags := make([]MastodonTag, 0, len(status.Tags))
	for _, t := range status.Tags {
		tags = append(tags, h.toMastodonTag(&t, false, false))
	}

	url := status.URL
	if url == "" {
		url = status.URI
	}
	url = h.absoluteURL(url)

	var inReplyToID *string
	var inReplyToAcctID *string
	if status.InReplyToID != nil && *status.InReplyToID != "" {
		inReplyToID = status.InReplyToID
		// Try to get the parent status's account ID
		if status.InReplyTo != nil {
			iID := status.InReplyTo.AccountID
			inReplyToAcctID = &iID
		}
	}

	// Quote post (Mastodon 4.4 Quote entity). The quoted status is embedded only
	// once the quote is approved; other states expose the state without content.
	var quote *MastodonQuote
	if status.QuoteOfURI != nil && *status.QuoteOfURI != "" {
		state := status.QuoteState
		if state == "" {
			state = "accepted"
		}
		quote = &MastodonQuote{State: state}
		if state == "accepted" && status.QuoteOf != nil {
			quote.QuotedStatus = h.statusToMastodon(status.QuoteOf, currentAccount)
		}
	}

	return &MastodonStatus{
		ID:        status.ID,
		URI:       status.URI,
		CreatedAt: createdAt,
		EditedAt:  editedAt,
		Account:   account,
		// content_html is sanitized at write time on every ingest path
		// (local compose, inbox, thread resolver, outbox backfill), so it
		// is served as-is here.
		Content:            status.ContentHTML,
		Visibility:         mastodonVisibility(status.Visibility),
		Sensitive:          status.Sensitive,
		SpoilerText:        status.Summary,
		MediaAttachments:   attachments,
		Mentions:           mentions,
		Tags:               tags,
		Emojis:             h.emojisToMastodon(status.Emojis),
		ReblogsCount:       status.ReblogsCount,
		FavouritesCount:    status.FavouritesCount,
		RepliesCount:       status.RepliesCount,
		URL:                url,
		InReplyToID:        inReplyToID,
		InReplyToAccountID: inReplyToAcctID,
		Pinned:             status.Pinned,
		Bookmarked:         status.Bookmarked,
		Favourited:         status.Favourited,
		Reblogged:          status.Reblogged,
		Muted:              status.Muted,
		Language:           mastodonStatusLanguage(status.Language),
		Application:        mastodonApplication,
		Card:               h.cardToMastodon(status.Preview),
		Quote:              quote,
		Poll:               status.Poll,
		Filtered:           filterResults(status.FilterMatches),
		EmojiReactions:     reactions,
		Pleroma:            map[string]interface{}{"emoji_reactions": reactions},
		ExpiresAt:          expiresAt,
	}
}

func mastodonStatusLanguage(raw string) *string {
	language := statusfilter.PrimaryLanguage(raw)
	if language == "" {
		return nil
	}
	return &language
}

func filterResults(matches []models.FilterMatch) []models.FilterMatch {
	if matches == nil {
		return []models.FilterMatch{}
	}
	return matches
}

func (h *Handler) emojiReactionsToMastodon(reactions []models.EmojiReactionSummary) []MastodonEmojiReaction {
	out := make([]MastodonEmojiReaction, 0, len(reactions))
	for _, reaction := range reactions {
		item := MastodonEmojiReaction{Name: reaction.Name, Count: reaction.Count, Me: reaction.Me, AccountIDs: reaction.AccountIDs}
		if reaction.Emoji != nil {
			item.URL = h.displayMediaURL(reaction.Emoji.URL)
			static := reaction.Emoji.StaticURL
			if static == "" {
				static = reaction.Emoji.URL
			}
			item.StaticURL = h.displayMediaURL(static)
		}
		out = append(out, item)
	}
	return out
}

func writeAPIJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// writeAPIJSONBytes writes a pre-encoded JSON body with the same headers and
// framing as writeAPIJSON (json.Encoder appends a trailing newline; match it
// so cached and live responses are byte-identical).
func writeAPIJSONBytes(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
	if len(body) == 0 || body[len(body)-1] != '\n' {
		_, _ = w.Write([]byte{'\n'})
	}
}

func writeAPIError(w http.ResponseWriter, status int, description string) {
	writeAPIJSON(w, status, map[string]string{"error": description})
}

func writeFederationPolicyError(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, storage.ErrFederationBlocked) {
		return false
	}
	writeAPIError(w, http.StatusForbidden, err.Error())
	return true
}

// Package webui provides the server-rendered web interface with htmx.
package webui

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"html"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/accounts"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/app"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/auth"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/blogtheme"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/cache"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/content"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/federation"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/housekeeping"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/httpneg"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/i18n"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mail"
	medialib "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media/objectstore"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/metrics"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/opengraph"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/statusfilter"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/streaming"
)

const maxPooledRenderBuffer = 256 << 10

var renderBufferPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func getRenderBuffer() *bytes.Buffer {
	b := renderBufferPool.Get().(*bytes.Buffer)
	b.Reset()
	return b
}

func putRenderBuffer(b *bytes.Buffer) {
	if b != nil && b.Cap() <= maxPooledRenderBuffer {
		b.Reset()
		renderBufferPool.Put(b)
	}
}

func escapeHTMLAttribute(value string) string {
	return template.HTMLEscapeString(value)
}

// Handler serves the SSR web UI.
type Handler struct {
	store          storage.Storage
	federator      *federation.Federator
	threadResolver *federation.ThreadResolver
	interactions   *federation.InteractionResolver
	inboxProcessor *federation.InboxProcessor // optional, for remote status and featured collection fetches
	app            *app.Service
	cfg            *config.Config
	domain         string
	localDomain    string
	singleUser     string
	password       string // global password, used as fallback during migration
	mediaPath      string
	mediaStore     objectstore.Store
	maxMediaSize   int64
	stripEXIF      bool
	renameMedia    bool
	templates      *template.Template
	// templatesByLocale holds one Funcs-bound clone of the parsed template
	// tree per supported locale (see loadTemplates), so renderPage can pick
	// the right T/Tn closures per request without re-parsing or re-cloning
	// per request.
	templatesByLocale map[string]*template.Template
	mailer            *mail.Mailer              // for sending emails (nil if SMTP not configured)
	vapidPublicKey    string                    // resolved persistent VAPID public key, empty if push is disabled
	housekeeper       *housekeeping.Housekeeper // optional, for on-demand admin housekeeping
	mediaProxy        *medialib.Proxy           // rewrites remote media URLs to the local proxy (privacy)
	previewFetcher    *opengraph.Fetcher        // safe, bounded Open Graph metadata cache

	// pageCache is the shared optional cache (nil when disabled). The web UI uses
	// it to memoize rendered status-card HTML (see renderStatusCard); it is the
	// same instance app.Service uses for page results, so entries share one budget
	// and a viewer's writes invalidate both. cardTTL bounds card staleness.
	pageCache *cache.MemoryCache
	cardTTL   time.Duration
	rssCache  *cache.MemoryCache

	// viewerTouch reports an authenticated request to the optional cache
	// warmer (nil when warming is disabled).
	viewerTouch func(accountID, path string)

	profileRefreshMu sync.Mutex
	profileRefresh   map[string]time.Time

	// Per-IP rate limiting for auth endpoints
	authRateMu     sync.Mutex
	authRateLogin  map[string][]time.Time
	authRateReset  map[string][]time.Time
	authRateVerify map[string][]time.Time
}

func (h *Handler) SetMediaStore(store objectstore.Store) {
	if store != nil {
		h.mediaStore = store
		h.app.SetMediaStore(store)
	}
}

// SetStreamPublisher wires the streaming fan-out into the web UI's shared
// app.Service so statuses composed in the web UI are streamed live.
func (h *Handler) SetStreamPublisher(p streaming.Publisher) {
	h.app.SetStreamPublisher(p)
}

// SetPageCache wires the optional per-viewer page cache into the shared
// app.Service. A nil cache leaves page caching disabled. cardTTL bounds the
// rendered status-card HTML entries; like components, cards are
// dependency-invalidated so the TTL only bounds count staleness.
func (h *Handler) SetPageCache(c *cache.MemoryCache, pageTTL, componentTTL, cardTTL time.Duration) {
	h.app.SetPageCache(c, pageTTL, componentTTL)
	h.pageCache = c
	h.cardTTL = cardTTL
}

// SetInboxProcessor wires on-demand remote status and featured collection fetches.
func (h *Handler) SetInboxProcessor(p *federation.InboxProcessor) {
	h.inboxProcessor = p
}

// SetServeStale enables stale-while-revalidate for cached pages.
func (h *Handler) SetServeStale(v bool) { h.app.SetServeStale(v) }

// SetViewerTouch wires the optional cache warmer's activity tracking; called
// with the account ID and request path of every authenticated request. Nil
// disables tracking.
func (h *Handler) SetViewerTouch(touch func(accountID, path string)) {
	h.viewerTouch = touch
}

// SetMailer wires the email sender (nil to disable).
func (h *Handler) SetMailer(m *mail.Mailer) {
	h.mailer = m
	if m != nil {
		slog.Info("mail: SMTP configured, email sending enabled")
	}
}

// SetVAPIDPublicKey wires the resolved VAPID public key so Settings can offer the web
// UI's own push subscription using the same key the push sender actually
// signs with. Empty disables the push settings UI.
func (h *Handler) SetVAPIDPublicKey(key string) {
	h.vapidPublicKey = key
}

// NewHandler builds a web UI handler for an instance whose local and web
// domains are the same. NewHandlerWithDomains covers the split-domain case.
func NewHandler(store storage.Storage, federator *federation.Federator, cfg *config.Config, domain, singleUser, password, mediaPath string, maxMediaSize int64, stripEXIF, renameMedia bool, housekeeper *housekeeping.Housekeeper, mediaProxy *medialib.Proxy, resolvers ...*federation.ThreadResolver) *Handler {
	return NewHandlerWithDomains(store, federator, cfg, domain, domain, singleUser, password, mediaPath, maxMediaSize, stripEXIF, renameMedia, housekeeper, mediaProxy, resolvers...)
}

func NewHandlerWithDomains(store storage.Storage, federator *federation.Federator, cfg *config.Config, localDomain, webDomain, singleUser, password, mediaPath string, maxMediaSize int64, stripEXIF, renameMedia bool, housekeeper *housekeeping.Housekeeper, mediaProxy *medialib.Proxy, resolvers ...*federation.ThreadResolver) *Handler {
	mediaCfg := cfg.Media
	if mediaCfg.Path == "" {
		mediaCfg.Path = mediaPath
	}
	mediaStore, _ := objectstore.New(mediaCfg, false)
	if mediaStore == nil {
		mediaStore = objectstore.NewLocal(mediaPath, "/media/")
	}
	previewOptions := []opengraph.Option{opengraph.WithStore(store)}
	if cfg.Media.CacheMode() != config.CacheModeOff {
		previewOptions = append(previewOptions, opengraph.WithImageCache(
			medialib.NewCache(cfg.Media.ResolvedCachePath(), cfg.Media.MaxFileSize, cfg.Media.StripEXIF),
		))
	}
	h := &Handler{
		store:          store,
		federator:      federator,
		app:            app.NewWithDomainsAndMediaPath(store, federator, localDomain, webDomain, mediaPath),
		cfg:            cfg,
		domain:         webDomain,
		localDomain:    localDomain,
		singleUser:     singleUser,
		password:       password,
		mediaPath:      mediaPath,
		mediaStore:     mediaStore,
		maxMediaSize:   maxMediaSize,
		stripEXIF:      stripEXIF,
		renameMedia:    renameMedia,
		housekeeper:    housekeeper,
		mediaProxy:     mediaProxy,
		previewFetcher: opengraph.NewFetcher(previewOptions...),
		rssCache:       cache.New(4 << 20),
		interactions:   federation.NewInteractionResolver(store, federator),
		authRateLogin:  make(map[string][]time.Time),
		authRateReset:  make(map[string][]time.Time),
		authRateVerify: make(map[string][]time.Time),
	}
	h.app.SetMediaStore(mediaStore)
	if cfg != nil {
		h.app.SetBlogAllAccounts(cfg.Blog.AllAccounts)
		h.app.SetBlockSensitiveRelationshipNotifications(cfg.Instance.BlockSensitiveRelationshipNotifications)
	}
	if len(resolvers) > 0 {
		h.threadResolver = resolvers[0]
	}

	h.loadTemplates()
	return h
}

// emojiURLMap builds a shortcode->image-URL map for the given custom emojis,
// rewriting each URL through the privacy proxy (local/relative cache URLs pass
// through unchanged). Returns nil when there are no emojis.
func (h *Handler) emojiURLMap(emojis []models.CustomEmoji) map[string]string {
	if len(emojis) == 0 {
		return nil
	}
	m := make(map[string]string, len(emojis))
	for _, e := range emojis {
		m[e.Shortcode] = h.browserMediaURL(e.URL)
	}
	return m
}

func (h *Handler) browserURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err == nil && u.User == nil && strings.EqualFold(u.Host, h.domain) &&
		(u.Scheme == "http" || u.Scheme == "https") {
		return u.RequestURI()
	}
	return raw
}

func (h *Handler) browserMediaURL(raw string) string {
	if h.mediaProxy != nil {
		raw = h.mediaProxy.URL(raw)
	}
	return h.browserURL(raw)
}

// localeOption is one entry in the settings-page language picker.
type localeOption struct {
	Code string
	Name string
}

// contentLanguageOptions is the composer's post-language list: the
// Mastodon-compatible `language` tag on a status, not the UI locale. Native
// names are not translated - a language picker conventionally shows each
// name in its own language regardless of viewer locale.
var contentLanguageOptions = []localeOption{
	{Code: "en", Name: "English"},
	{Code: "ar", Name: "العربية"},
	{Code: "bg", Name: "Български"},
	{Code: "cs", Name: "Čeština"},
	{Code: "da", Name: "Dansk"},
	{Code: "de", Name: "Deutsch"},
	{Code: "el", Name: "Ελληνικά"},
	{Code: "eo", Name: "Esperanto"},
	{Code: "es", Name: "Español"},
	{Code: "et", Name: "Eesti"},
	{Code: "fi", Name: "Suomi"},
	{Code: "fr", Name: "Français"},
	{Code: "ga", Name: "Gaeilge"},
	{Code: "he", Name: "עברית"},
	{Code: "hi", Name: "हिन्दी"},
	{Code: "hr", Name: "Hrvatski"},
	{Code: "hu", Name: "Magyar"},
	{Code: "id", Name: "Bahasa Indonesia"},
	{Code: "is", Name: "Íslenska"},
	{Code: "it", Name: "Italiano"},
	{Code: "ja", Name: "日本語"},
	{Code: "ko", Name: "한국어"},
	{Code: "lt", Name: "Lietuvių"},
	{Code: "lv", Name: "Latviešu"},
	{Code: "nl", Name: "Nederlands"},
	{Code: "no", Name: "Norsk"},
	{Code: "pl", Name: "Polski"},
	{Code: "pt", Name: "Português"},
	{Code: "ro", Name: "Română"},
	{Code: "ru", Name: "Русский"},
	{Code: "sk", Name: "Slovenčina"},
	{Code: "sl", Name: "Slovenščina"},
	{Code: "sr", Name: "Српски"},
	{Code: "sv", Name: "Svenska"},
	{Code: "th", Name: "ไทย"},
	{Code: "tr", Name: "Türkçe"},
	{Code: "uk", Name: "Українська"},
	{Code: "vi", Name: "Tiếng Việt"},
	{Code: "zh-CN", Name: "中文 (中国)"},
	{Code: "zh-TW", Name: "中文 (台灣)"},
}

var filterLanguageOptions = func() []localeOption {
	seen := make(map[string]bool, len(contentLanguageOptions))
	options := make([]localeOption, 0, len(contentLanguageOptions))
	for _, option := range contentLanguageOptions {
		option.Code = statusfilter.PrimaryLanguage(option.Code)
		if option.Code != "" && !seen[option.Code] {
			seen[option.Code] = true
			options = append(options, option)
		}
	}
	return options
}()

// isValidContentLanguage reports whether code is one of contentLanguageOptions.
func isValidContentLanguage(code string) bool {
	for _, opt := range contentLanguageOptions {
		if opt.Code == code {
			return true
		}
	}
	return false
}

func (h *Handler) loadTemplates() {
	// Embed all templates
	tmpl := template.New("").Funcs(template.FuncMap{
		"formatTime":         formatTime,
		"formatTimeISO":      formatTimeISO,
		"formatTimeRelative": formatTimeRelative,
		"formatDuration":     formatDuration,
		"formatTimePtr":      formatTimePtr,
		"formatTimeISOPtr":   formatTimeISOPtr,
		"visibilityIcon":     visibilityIcon,
		"visibilityLabel":    func(v models.Visibility) string { return visibilityLabel(v, i18n.DefaultLocale) },
		"truncate":           truncateText,
		"unescapeHTML":       html.UnescapeString,
		"list":               func(values ...string) []string { return values },
		"excerpt": func(html string, n int) string {
			return truncateText(content.PlainText(html), n)
		},
		"acctPath":               accountPath,
		"profilePath":            profilePath,
		"replyPath":              replyPath,
		"statusPath":             statusPath,
		"interactionPath":        interactionPath,
		"statusInteractionsPath": statusInteractionsPath,
		"statusShareURL":         statusShareURL,
		"blogPostPath":           blogPostPath,
		"userAgent": func() string {
			if h.cfg != nil {
				return h.cfg.Federation.UserAgent
			}
			return ""
		},
		// catchupEnabled gates the catch-up navigation entries without
		// plumbing the flag through every page's data struct.
		"catchupEnabled": h.catchupEnabled,
		// jobPoolLabel names the worker pool a queue job kind actually runs
		// on right now, mirroring cmd/littlefedi/main.go's wiring: only when
		// light_workers > 0 do the light kinds move off the main pool.
		"jobPoolLabel": func(kind string) string {
			if h.cfg != nil && h.cfg.Queue.LightWorkers > 0 && app.IsLightJobKind(kind) {
				return "light"
			}
			return "main"
		},
		"trustedTemplateHTML": func(s interface{}) template.HTML {
			switch v := s.(type) {
			case template.HTML:
				return v
			default:
				// Fallback: escape via fmt.Sprintf then re-escape with HTMLEscapeString.
				// This prevents XSS if a non-string accidentally reaches this path.
				return template.HTML(template.HTMLEscapeString(fmt.Sprintf("%v", v)))
			}
		},
		"sanitizeHTML": func(s string) template.HTML {
			// Sanitize first, then point hashtag links at this instance's local
			// tag pages so clicking #tag stays on littleFedi instead of jumping
			// to the remote author's server.
			s = content.LocalizeHashtagLinks(content.SanitizeHTMLStr(s))
			return template.HTML(content.LocalizeInstanceLinks(s, h.domain))
		},
		"localizeStatusHTML": func(s string) template.HTML {
			s = content.LocalizeHashtagLinks(content.SanitizeHTMLStr(s))
			return template.HTML(content.LocalizeInstanceLinks(s, h.domain))
		},
		// emojifyHTML renders post content/spoiler HTML and replaces custom-emoji
		// `:shortcode:` references with inline <img> tags. Emoji images are
		// injected after sanitization (the sanitizer strips <img> from federated
		// content) and their URLs go through the privacy proxy / cache.
		"emojifyHTML": func(s string, emojis []models.CustomEmoji) template.HTML {
			sanitized := content.LocalizeHashtagLinks(content.SanitizeHTMLStr(s))
			sanitized = content.LocalizeInstanceLinks(sanitized, h.domain)
			return template.HTML(content.EmojifyHTML(sanitized, h.emojiURLMap(emojis)))
		},
		// emojifyStatusHTML additionally hides Mastodon's quote-inline fallback
		// when a native quote card is available, avoiding duplicate quote links.
		"emojifyStatusHTML": func(s string, emojis []models.CustomEmoji, quote *models.Status) template.HTML {
			s = content.SanitizeHTMLStr(s)
			if quote != nil {
				s = content.RemoveQuoteInlineFallback(s)
			}
			s = content.LocalizeHashtagLinks(s)
			s = content.LocalizeInstanceLinks(s, h.domain)
			return template.HTML(content.EmojifyHTML(s, h.emojiURLMap(emojis)))
		},
		// emojifyName renders a plain-text display name with custom emoji.
		"emojifyName": func(name string, emojis []models.CustomEmoji) template.HTML {
			return template.HTML(content.EmojifyText(name, h.emojiURLMap(emojis)))
		},
		"emojifyReaction": func(label, reaction string, emoji *models.CustomEmoji) template.HTML {
			escaped := template.HTMLEscapeString(label)
			if emoji == nil || reaction == "" {
				return template.HTML(escaped)
			}
			img := fmt.Sprintf(`<img class="emoji" src="%s" alt="%s" draggable="false">`,
				escapeHTMLAttribute(h.browserMediaURL(emoji.URL)), escapeHTMLAttribute(reaction))
			return template.HTML(strings.Replace(escaped, template.HTMLEscapeString(reaction), img, 1))
		},
		"urlSafe": safeTemplateURL,
		// isLongStatus decides whether a card's body renders behind a
		// "show more" disclosure. See the constants for what counts as long.
		"isLongStatus": isLongStatus,
		// proxyMedia rewrites a remote media URL to the local privacy proxy when
		// enabled; local/relative URLs pass through unchanged.
		"proxyMedia": h.browserMediaURL,
		// A media box needs its ratio before the file lands or the page jumps
		// as each picture arrives. Both return zero for an attachment nobody
		// measured, and the templates then omit the dimensions.
		"mediaWidth":  func(m models.MediaAttachment) int { w, _ := m.Dimensions(); return w },
		"mediaHeight": func(m models.MediaAttachment) int { _, h := m.Dimensions(); return h },
		// The picture's own average tone, to paint the box with while it
		// loads. Empty when there is no usable hash, and the box then keeps
		// whatever the stylesheet gave it.
		"blurhashColour": func(hash string) string {
			colour, ok := medialib.AverageColour(hash)
			if !ok {
				return ""
			}
			return colour
		},
		"localURL":                   h.browserURL,
		"previewURL":                 opengraph.URLFromHTML,
		"assetURL":                   staticAssetURL,
		"jsValue":                    jsValue,
		"replyMention":               replyMention,
		"replyMentions":              replyMentions,
		"maxMedia":                   h.maxMedia,
		"maxStatusChars":             h.maxStatusChars,
		"blogEnabled":                h.blogAllowed,
		"publicLocalTimelineEnabled": h.publicLocalTimelineEnabled,
		"publicBlogDirectoryEnabled": h.publicBlogDirectoryEnabled,
		"blogDirectoryEnabled": func() bool {
			return h.blogCapable() && h.cfg != nil && h.cfg.Blog.DirectoryEnabled
		},
		// Whether this instance lets authors pick their own permalink
		// template, and what the instance-wide default looks like.
		"blogSlugFormatEditable": func() bool { return h.blogCapable() && h.cfg.Blog.AllowUserSlugFormat },
		"blogSlugFormatDefault":  func() string { return h.cfg.Blog.ResolvedSlugFormat("") },
		"add":                    func(a, b int) int { return a + b },
		"sub":                    func(a, b int) int { return a - b },
		"containsInt": func(values []int, want int) bool {
			for _, value := range values {
				if value == want {
					return true
				}
			}
			return false
		},
		"containsString": func(values []string, want string) bool {
			for _, value := range values {
				if value == want {
					return true
				}
			}
			return false
		},
		"filterExpiryDate": func(value *time.Time) string {
			if value == nil {
				return ""
			}
			return value.In(time.Local).Format("2006-01-02T15:04")
		},
		"seq": func(start, end int) []int {
			n := end - start + 1
			if n <= 0 {
				return nil
			}
			result := make([]int, n)
			for i := range result {
				result[i] = start + i
			}
			return result
		},
		"hasPrefix": strings.HasPrefix,
		// T/Tn are bound to DefaultLocale here only so ParseFS's function-
		// existence check passes; loadTemplates below overrides both per
		// locale on each cloned tree via Funcs before that clone is ever used.
		"T":    func(key string, args ...any) string { return i18n.T(i18n.DefaultLocale, key, args...) },
		"Tn":   func(n int, key string, args ...any) string { return i18n.Tn(i18n.DefaultLocale, n, key, args...) },
		"rawT": func(key string) string { return i18n.Raw(i18n.DefaultLocale, key) },
		// supportedLocales lists every wired-in locale for the settings-page
		// language picker, native-name labelled (LocaleNames), in display order.
		"supportedLocales": func() []localeOption {
			codes := i18n.SupportedLocales()
			out := make([]localeOption, len(codes))
			for i, code := range codes {
				out[i] = localeOption{Code: code, Name: i18n.LocaleNames[code]}
			}
			return out
		},
		// availableThemes/availableAppearances feed the settings-page
		// appearance picker. Labels come from T via each theme's NameKey,
		// so a theme is translatable like any other string.
		"availableThemes":      Themes,
		"availableAppearances": Appearances,
		// availableBlogThemes feeds the same picker for the generated static
		// blog. It comes from a package with no build tag, so this compiles in
		// builds without the blog too; the section around it is gated on
		// blogEnabled instead.
		"availableBlogThemes": blogtheme.Themes,
		// contentLanguages lists the composer's post-language choices (ISO
		// 639-1-ish codes, native-script names) - the Mastodon `language` field
		// on a status, unrelated to the UI locale. Shared by the composer's
		// per-post select and the settings-page default-post-language picker.
		"contentLanguages": func() []localeOption { return contentLanguageOptions },
		"filterLanguages":  func() []localeOption { return filterLanguageOptions },
		// statusCard renders one status card, caching plain cards so the render is
		// reused across every timeline/profile (see renderStatusCard). It is the
		// caching replacement for `{{template "components/status.html" ...}}`.
		// Bound to DefaultLocale here only so ParseFS's function-existence
		// check passes; overridden per locale below, same as T/Tn.
		"statusCard": func(s *models.Status, viewer *models.Account, csrf string) template.HTML {
			return h.renderStatusCard(s, viewer, csrf, i18n.DefaultLocale)
		},
		"dict": func(values ...interface{}) (map[string]interface{}, error) {
			if len(values)%2 != 0 {
				return nil, fmt.Errorf("dict: odd number of arguments")
			}
			d := make(map[string]interface{}, len(values)/2)
			for i := 0; i < len(values); i += 2 {
				key, ok := values[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict: key must be string")
				}
				d[key] = values[i+1]
			}
			return d, nil
		},
	})

	// Parse all templates from embedded FS
	tmpl = template.Must(tmpl.ParseFS(embeddedFS, "templates/*.html", "templates/components/*.html", "templates/admin/*.html"))

	// Clone the parsed tree once per supported locale, each clone binding
	// locale-specific T/Tn closures. Clone() copies the associated-template
	// namespace, not text, so this is a handful of cheap startup-time clones
	// (not a per-request cost) - the Pi-Zero-class low-power target can't
	// afford re-parsing or re-cloning the ~3000-line template set per request.
	locales := i18n.SupportedLocales()
	h.templatesByLocale = make(map[string]*template.Template, len(locales))
	for _, loc := range locales {
		loc := loc
		clone := template.Must(tmpl.Clone())
		h.templatesByLocale[loc] = clone.Funcs(template.FuncMap{
			"T":  func(key string, args ...any) string { return i18n.T(loc, key, args...) },
			"Tn": func(n int, key string, args ...any) string { return i18n.Tn(loc, n, key, args...) },
			// rawT exposes the unformatted catalog string (with its literal
			// "%s") for JS to substitute a client-timezone date into, so a
			// server-rendered timestamp can be upgraded without duplicating
			// this locale's word order/translation in app.js.
			"rawT": func(key string) string { return i18n.Raw(loc, key) },
			"statusCard": func(s *models.Status, viewer *models.Account, csrf string) template.HTML {
				return h.renderStatusCard(s, viewer, csrf, loc)
			},
			"visibilityLabel": func(v models.Visibility) string { return visibilityLabel(v, loc) },
		})
	}
	h.templates = h.templatesByLocale[i18n.DefaultLocale]
}

// localeCookieName is the anonymous-session locale preference, set from the
// settings form and read back on every request before a session exists.
const localeCookieName = "locale"

// theme/appearance cookies mirror the account preference so a signed-out
// visitor keeps the look they picked, exactly as localeCookieName does.
const (
	themeCookieName      = "theme"
	appearanceCookieName = "appearance"
)

// blogAllowed reports whether account may publish to the static blog on this
// server: the feature has to be compiled in and enabled for the instance, and
// the account either granted individually by an admin or covered by
// [blog] all_accounts. Every gate in the web UI (composer, settings sections,
// profile tab, notification toggles) goes through here, so the two halves of
// the answer can never drift apart.
func (h *Handler) blogAllowed(account *models.Account) bool {
	return h.blogCapable() && account != nil && h.cfg.Blog.AccountAllowed(account.BlogAllowed)
}

func (h *Handler) blogSitemapURL() string {
	if h.blogCapable() && h.cfg != nil && h.cfg.Blog.DirectoryEnabled && h.publicBlogDirectoryEnabled() {
		return "https://" + h.domain + "/sitemap.xml"
	}
	return ""
}

func (h *Handler) publicLocalTimelineEnabled() bool {
	return h.cfg == nil || h.cfg.Web.PublicLocalTimelineEnabled()
}

func (h *Handler) publicBlogDirectoryEnabled() bool {
	return h.cfg == nil || h.cfg.Web.PublicBlogDirectoryEnabled()
}

func redirectGuest(w http.ResponseWriter, r *http.Request, target string) {
	w.Header().Set("Cache-Control", "private, no-store")
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// pageTitle resolves a catalog key to the request's locale, for use as the
// title argument to renderPage.
func (h *Handler) pageTitle(r *http.Request, key string, args ...any) string {
	return i18n.T(h.resolveLocale(r, getAuthAccount(r.Context())), key, args...)
}

// localeTemplates returns the template set bound to this request's locale.
// Fragment handlers that call ExecuteTemplate directly (rather than going
// through renderPage) must use this, or their {{T}} calls fall back to the
// default-locale clone and the fragment lands in English inside a translated
// page.
func (h *Handler) localeTemplates(r *http.Request) *template.Template {
	if tmpl := h.templatesByLocale[h.resolveLocale(r, getAuthAccount(r.Context()))]; tmpl != nil {
		return tmpl
	}
	return h.templates
}

// resolveLocale picks the locale for one request: a logged-in account's
// saved preference, then the anonymous locale cookie, then a best-effort
// Accept-Language match, then the configured site default, then "en".
func (h *Handler) resolveLocale(r *http.Request, account *models.Account) string {
	if account != nil && i18n.IsSupported(account.Locale) {
		return account.Locale
	}
	if cookie, err := r.Cookie(localeCookieName); err == nil && i18n.IsSupported(cookie.Value) {
		return cookie.Value
	}
	if loc := i18n.MatchAcceptLanguage(r.Header.Get("Accept-Language")); loc != "" {
		return loc
	}
	if h.cfg != nil && i18n.IsSupported(h.cfg.Web.DefaultLocale) {
		return h.cfg.Web.DefaultLocale
	}
	return i18n.DefaultLocale
}

// customCSSFor returns the viewer's own stylesheet, ready to inline. The value
// is re-validated rather than trusted from the column: the row may predate a
// change to what ValidateCustomCSS rejects, and template.CSS opts out of the
// contextual escaping that would otherwise catch it. A value that fails is
// dropped, not reported: the page still renders, just without the skin.
func customCSSFor(account *models.Account) template.CSS {
	if account == nil || account.CustomCSS == "" {
		return ""
	}
	css, err := content.ValidateCustomCSS(account.CustomCSS)
	if err != nil {
		return ""
	}
	return template.CSS(css)
}

// resolveTheme picks the skin for one request: a logged-in account's saved
// preference, then the anonymous theme cookie, then the instance default,
// then littleFedi's own look. Unknown values are skipped rather than
// rejected, so a stale cookie or a config typo degrades to the default.
func (h *Handler) resolveTheme(r *http.Request, account *models.Account) Theme {
	if account != nil {
		if t, ok := ThemeByID(account.Theme); ok {
			return t
		}
	}
	if cookie, err := r.Cookie(themeCookieName); err == nil {
		if t, ok := ThemeByID(cookie.Value); ok {
			return t
		}
	}
	if h.cfg != nil {
		if t, ok := ThemeByID(h.cfg.Web.DefaultTheme); ok {
			return t
		}
	}
	t, _ := ThemeByID(ThemeDefault)
	return t
}

// resolveAppearance picks light/dark mode for one request, with the same
// precedence as resolveTheme.
func (h *Handler) resolveAppearance(r *http.Request, account *models.Account) string {
	if account != nil && IsSupportedAppearance(account.Appearance) {
		return account.Appearance
	}
	if cookie, err := r.Cookie(appearanceCookieName); err == nil && IsSupportedAppearance(cookie.Value) {
		return cookie.Value
	}
	if h.cfg != nil && IsSupportedAppearance(h.cfg.Web.DefaultAppearance) {
		return h.cfg.Web.DefaultAppearance
	}
	return AppearanceAuto
}

// renderStatusCard renders and caches one presentation variant of a status.
// The presentation hash covers boost aggregation, filters and poll state; the
// cache key also carries locale (status.html contains {{T ...}} calls), so a
// viewer in one language can never be served another language's cached card.
// Cache dependencies provide targeted invalidation when the underlying
// component or viewer state changes.
func (h *Handler) renderStatusCard(s *models.Status, viewer *models.Account, csrf string, locale string) template.HTML {
	if s == nil {
		return ""
	}
	tmpl := h.templatesByLocale[locale]
	if tmpl == nil {
		tmpl = h.templates
	}
	cacheable := h.pageCache != nil
	var key string
	var cacheVersion uint64
	if cacheable {
		viewerID := viewerIDOf(viewer)
		key = app.ViewerCacheKeyPrefix(viewerID) + "card:" + locale + ":" + s.ID + ":" + cardPresentationHash(s, csrf, viewer)
		v, ok, lookup := h.pageCache.GetWithResult(key)
		if ok {
			slog.Debug("cache: status card hit", "status", s.ID)
			return v.(template.HTML)
		}
		cacheVersion = h.pageCache.Version()
		if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
			stats := h.pageCache.Stats()
			slog.Debug("cache: status card rebuild", "status", s.ID, "reason", lookup, "ttl", h.cardTTL, "entries", stats.Entries, "evictions", stats.Evictions, "expirations", stats.Expirations)
		}
	}
	buf := getRenderBuffer()
	renderStart := time.Now()
	err := tmpl.ExecuteTemplate(buf, "components/status.html", map[string]interface{}{
		"Status":    s,
		"Account":   viewer,
		"CSRFToken": csrf,
	})
	metrics.ObserveTimelineStage("card_render", time.Since(renderStart))
	if err != nil {
		putRenderBuffer(buf)
		slog.Error("webui: render status card", "status", s.ID, "error", err)
		return ""
	}
	// The cached/returned card must not retain storage owned by the pool.
	// bytes.Buffer.String returns an owned string copy, so the pool can safely
	// reclaim the backing byte slice.
	out := template.HTML(buf.String())
	cardBytes := buf.Len()
	putRenderBuffer(buf)
	if cacheable {
		viewerID := viewerIDOf(viewer)
		deps := []string{"status:" + s.ID, "viewer:" + viewerID, "viewer-status:" + viewerID + ":" + s.ID}
		if s.BoostOfID != nil && *s.BoostOfID != "" {
			deps = append(deps, "status:"+*s.BoostOfID)
		}
		if s.AccountID != "" {
			deps = append(deps, "account:"+s.AccountID)
		}
		stored := h.pageCache.SetIfVersionWithDependencies(cacheVersion, key, out, cardBytes+96, h.cardTTL, deps)
		slog.Debug("cache: status card rebuilt", "status", s.ID, "bytes", cardBytes, "stored", stored)
	}
	return out
}

func cardPresentationHash(s *models.Status, csrf string, viewer *models.Account) string {
	var b strings.Builder
	b.WriteString(csrf)
	b.WriteString(s.UpdatedAt.Format(time.RFC3339Nano))
	b.WriteString(fmt.Sprintf("|%t|%t|%t", s.Favourited, s.Bookmarked, s.Reblogged))
	if viewer != nil {
		b.WriteString(fmt.Sprintf("|%t|%t|%t", viewer.DataSaver, viewer.HideFavouriteCounts, viewer.HideBoostCounts))
	}
	for _, a := range s.BoostedBy {
		if a != nil {
			b.WriteString("|b:" + a.ID)
		}
	}
	for _, m := range s.FilterMatches {
		if m.Filter != nil {
			b.WriteString("|f:" + m.Filter.ID + ":" + m.Filter.Title)
		}
		for _, keyword := range m.KeywordMatches {
			b.WriteString(":" + keyword)
		}
		for _, language := range m.LanguageMatches {
			b.WriteString(":" + language)
		}
	}
	if s.Poll != nil {
		b.WriteString(fmt.Sprintf("|p:%s:%d:%t:%s", s.Poll.ID, s.Poll.VotesCount, s.Poll.Voted, s.ContentHTML))
		for _, v := range s.Poll.OwnVotes {
			b.WriteString(fmt.Sprintf(":%d", v))
		}
	}
	// Cache-key discrimination only, not a security boundary: FNV-1a keeps
	// the per-card cost trivial where SHA-256 ran once per card per request.
	h := fnv.New64a()
	_, _ = h.Write([]byte(b.String()))
	return strconv.FormatUint(h.Sum64(), 16)
}

// RegisterRoutes registers web UI routes on the given router.
func (h *Handler) RegisterRoutes(r chi.Router) {
	// Session auth routes
	r.Get("/auth/sign_in", h.LoginPage)
	r.With(h.SameOriginFormMiddleware).Post("/auth/sign_in", h.Login)
	r.With(h.SessionMiddleware, h.CSRFMiddleware).Post("/auth/sign_out", h.Logout)

	// Registration routes
	r.Get("/auth/sign_up", h.SignUpPage)
	r.With(h.SameOriginFormMiddleware).Post("/auth/sign_up", h.SignUp)

	// Password reset routes
	r.Get("/auth/password/new", h.PasswordResetRequestPage)
	r.With(h.SameOriginFormMiddleware).Post("/auth/password/new", h.PasswordResetRequest)
	r.Get("/auth/password/edit", h.PasswordResetEditPage)
	r.With(h.SameOriginFormMiddleware).Post("/auth/password/edit", h.PasswordResetEdit)

	// Email verification
	r.Get("/auth/verify_email", h.VerifyEmail)
	r.Get("/auth/verify_email/resend", h.ResendVerificationPage)
	r.With(h.SameOriginFormMiddleware).Post("/auth/verify_email/resend", h.ResendVerification)

	// Public pages with optional auth
	r.With(h.OptionalAuthMiddleware).Get("/", h.HomePage)
	r.With(h.OptionalAuthMiddleware).Get("/public", h.PublicTimelinePage)
	r.With(h.OptionalAuthMiddleware).Get("/public/local", h.LocalTimelinePage)
	r.With(h.OptionalAuthMiddleware).Get("/public/bubble", h.BubbleTimelinePage)
	r.With(h.OptionalAuthMiddleware).Get("/about", h.AboutPage)
	r.With(h.OptionalAuthMiddleware).Get("/terms", h.TermsPage)

	// Profile pages
	r.Get("/@{username}.rss", h.RSSFeed)
	r.With(h.OptionalAuthMiddleware).Get("/@{username}", h.ProfilePage)
	r.With(h.OptionalAuthMiddleware).Get("/@{username}/followers", h.FollowersPage)
	r.With(h.OptionalAuthMiddleware).Get("/@{username}/following", h.FollowingPage)
	r.With(h.OptionalAuthMiddleware).Get("/@{username}/{id}", h.ThreadPage)
	h.registerBlogRoutes(r)

	// Resolve remote account by handle
	r.With(h.OptionalAuthMiddleware).Get("/resolve", h.ResolveAccount)

	// Search (accounts, statuses, hashtags) and hashtag timelines
	r.With(h.OptionalAuthMiddleware).Get("/search", h.SearchPage)
	r.With(h.OptionalAuthMiddleware).Get("/tags/{tag}", h.TagTimelinePage)

	// Protected routes
	r.Group(func(r chi.Router) {
		r.Use(h.SessionMiddleware)
		r.Use(h.CSRFMiddleware)

		// Notifications
		r.Get("/notifications", h.NotificationsPage)
		r.Get("/partial/notifications", h.PartialNotifications)
		r.Get("/partial/notifications/new", h.PartialNotificationsNew)
		r.Post("/notifications/clear", h.NotificationsClear)
		r.Post("/notifications/read", h.NotificationsRead)
		r.Get("/web/notification-count", h.NotificationCount)

		// Catch-up (ranked, explained, opt-in by visiting it)
		r.Get("/catchup", h.CatchupPage)
		r.Post("/catchup/seen", h.CatchupSeen)

		// Bookmarks
		r.Get("/bookmarks", h.BookmarksPage)

		// Favourites
		r.Get("/favourites", h.FavouritesPage)

		// Direct messages
		r.Get("/direct", h.DirectMessagesPage)

		// Settings
		r.Get("/settings", h.SettingsPage)
		r.Post("/settings/profile", h.SettingsProfile)

		// Password change
		r.Get("/settings/password", h.SettingsPasswordPage)
		r.Post("/settings/password", h.SettingsPasswordChange)
		r.Get("/settings/mfa", h.SettingsMFAPage)
		r.Post("/settings/mfa/enroll", h.SettingsMFAEnroll)
		r.Post("/settings/mfa/disable", h.SettingsMFADisable)
		r.Post("/settings/mfa/recovery", h.SettingsMFARegenerateRecovery)
		r.Get("/settings/export", h.SettingsExportPage)
		r.Post("/settings/export", h.SettingsExportRequest)
		r.Post("/settings/export/{id}/download", h.SettingsExportDownload)
		r.Get("/settings/import", h.SettingsImportPage)
		r.Post("/settings/import", h.SettingsImportUpload)
		r.Get("/settings/delete", h.SettingsDeletePage)
		r.Post("/settings/delete", h.SettingsDeleteSubmit)

		// Session management
		r.Get("/settings/sessions", h.SettingsSessionsPage)
		r.Post("/settings/sessions/{id}/revoke", h.SettingsSessionRevoke)

		// Muted & blocked accounts and domains
		r.Get("/settings/moderation", h.SettingsModerationPage)
		r.Post("/settings/domain_blocks", h.SettingsDomainBlockCreate)
		r.Post("/settings/domain_blocks/delete", h.SettingsDomainBlockDelete)

		// Privacy preferences
		r.Post("/settings/privacy", h.SettingsPrivacyUpdate)
		r.Post("/settings/locale", h.SettingsLocaleUpdate)
		r.Post("/settings/appearance", h.SettingsAppearanceUpdate)
		r.Post("/settings/custom-css", h.SettingsCustomCSSUpdate)
		r.Post("/settings/post_language", h.SettingsPostLanguageUpdate)
		r.Post("/settings/timeline", h.SettingsTimelineUpdate)
		r.Post("/settings/wellness", h.SettingsWellnessUpdate)
		r.Post("/settings/filters", h.SettingsFilterCreate)
		r.Post("/settings/filters/{id}", h.SettingsFilterUpdate)
		r.Post("/settings/filters/{id}/delete", h.SettingsFilterDelete)
		r.Post("/settings/blog", h.SettingsBlogUpdate)
		r.Post("/settings/blog-appearance", h.SettingsBlogAppearanceUpdate)
		r.Post("/settings/blog-rebuild", h.SettingsBlogRebuild)
		r.Post("/settings/notifications", h.SettingsNotificationsUpdate)
		r.Post("/settings/email_notifications", h.SettingsEmailNotificationsUpdate)
		r.Post("/settings/email", h.SettingsEmailUpdate)

		// Web push (PWA) subscription
		r.Post("/settings/push/subscribe", h.SettingsPushSubscribe)
		r.Post("/settings/push/unsubscribe", h.SettingsPushUnsubscribe)
		r.Post("/settings/push/preferences", h.SettingsPushPreferencesUpdate)
		r.Post("/settings/push/revoke/{id}", h.SettingsPushRevoke)

		// Pruning preferences
		r.Get("/settings/pruning", h.SettingsPruningPage)
		r.Post("/settings/pruning", h.SettingsPruningUpdate)

		// Account migration
		r.Get("/settings/move", h.SettingsMovePage)
		r.Post("/settings/move", h.SettingsMoveSubmit)
		r.Post("/settings/migration_alias", h.SettingsSetMigrationAlias)
		r.Post("/settings/import_follows", h.SettingsImportFollows)
		r.Get("/settings/export_follows", h.SettingsExportFollows)
		r.Post("/settings/import_follows_csv", h.SettingsImportFollowsCSV)

		// Composer actions (htmx post)
		r.Post("/statuses", h.ComposeStatus)
		r.Post("/partial/composer/preview", h.ComposerPreview)

		// Interaction actions (htmx post)
		r.Post("/statuses/{id}/favourite", h.FavouriteStatus)
		r.Post("/statuses/{id}/reactions", h.EmojiReactionStatus)
		r.Post("/statuses/{id}/reblog", h.ReblogStatus)
		r.Post("/statuses/{id}/bookmark", h.BookmarkStatus)
		r.Post("/polls/{id}/vote", h.VotePoll)
		r.Get("/statuses/{id}/edit", h.EditStatusPage)
		r.Post("/statuses/{id}/edit", h.EditStatusSubmit)
		r.Get("/statuses/{id}/redraft", h.RedraftStatusPage)
		r.Post("/statuses/{id}/redraft", h.RedraftStatusRedirect)
		r.Get("/statuses/{id}/redraft-error", h.RedraftErrorPage)
		r.Post("/statuses/{id}/delete", h.DeleteStatus)
		r.Post("/statuses/{id}/pin", h.PinStatus)
		r.Post("/statuses/{id}/unpin", h.UnpinStatus)

		// Follow actions
		r.Post("/accounts/{id}/follow", h.FollowAccount)
		r.Post("/accounts/{id}/unfollow", h.UnfollowAccount)
		r.Get("/follow_requests", h.FollowRequestsPage)
		r.Post("/follow_requests/{id}/authorize", h.AuthorizeFollowRequest)
		r.Post("/follow_requests/{id}/reject", h.RejectFollowRequest)
		r.Post("/accounts/{id}/notify", h.NotifyAccount)
		r.Post("/accounts/{id}/block", h.BlockAccount)
		r.Post("/accounts/{id}/unblock", h.UnblockAccount)
		r.Post("/accounts/{id}/mute", h.MuteAccount)
		r.Post("/accounts/{id}/unmute", h.UnmuteAccount)

		// Reporting
		r.Get("/report", h.ReportFormPage)
		r.Post("/report", h.ReportSubmit)

		// Followed hashtags
		r.Get("/tags", h.FollowedTagsPage)
		r.Post("/tags/{tag}/follow", h.FollowTagWeb)
		r.Post("/tags/{tag}/unfollow", h.UnfollowTagWeb)

		// Lists
		r.Get("/lists", h.ListsPage)
		r.Post("/lists", h.ListCreateWeb)
		r.Get("/lists/{id}", h.ListTimelinePage)
		r.Post("/lists/{id}", h.ListUpdateWeb)
		r.Post("/lists/{id}/delete", h.ListDeleteWeb)
		r.Post("/lists/{id}/accounts/add", h.ListMemberAddWeb)
		r.Post("/lists/{id}/accounts/{accountID}/remove", h.ListMemberRemoveWeb)

		// Scheduled posts
		r.Get("/scheduled", h.ScheduledPostsPage)
		r.Post("/scheduled/{id}/delete", h.ScheduledPostCancelWeb)
		r.Post("/scheduled/{id}", h.ScheduledPostRescheduleWeb)
		// List timelines are private, so the partial lives in the protected group.
		r.Get("/partial/list/{id}", h.PartialListTimeline)
	})

	// Admin routes (protected by admin middleware)
	r.Group(func(r chi.Router) {
		r.Use(h.SessionMiddleware)
		r.Use(h.CSRFMiddleware)
		r.Use(h.AdminMiddleware)

		// Admin dashboard
		r.Get("/admin", h.AdminDashboard)
		r.Post("/admin/broadcast", h.AdminBroadcastDirectMessage)
		r.Post("/admin/welcome-message", h.AdminWelcomeDirectMessage)
		r.Get("/admin/queue", h.AdminQueue)
		r.Post("/admin/queue/{id}/delete", h.AdminQueueDeleteProblem)

		// Account management
		r.Get("/admin/accounts", h.AdminAccountsList)
		r.Post("/admin/accounts/{id}/admin", h.AdminToggleAdmin)
		r.Post("/admin/accounts/{id}/blog", h.AdminToggleBlogAllowed)
		r.Post("/admin/accounts/{id}/moderate", h.AdminModerateAccount)
		r.Get("/admin/accounts/{id}/delete", h.AdminDeleteAccountConfirm)
		r.Post("/admin/accounts/{id}/delete", h.AdminDeleteAccount)
		r.Post("/admin/accounts/{id}/approve", h.AdminApproveAccount)
		r.Post("/admin/accounts/{id}/reject", h.AdminRejectAccount)

		// Reports
		r.Get("/admin/reports", h.AdminReports)
		r.Post("/admin/reports/{id}/resolve", h.AdminReportResolveWeb)

		// Moderation audit log
		r.Get("/admin/audit", h.AdminAuditLog)

		// Invite management
		r.Get("/admin/invites", h.AdminInvitesList)
		r.Post("/admin/invites", h.AdminInvitesCreate)
		r.Post("/admin/invites/{id}/revoke", h.AdminInvitesRevoke)

		// Pending approvals
		r.Get("/admin/pending", h.AdminPendingApprovals)

		// Federation policy
		r.Get("/admin/domain_blocks", h.AdminDomainBlocks)
		r.Post("/admin/domain_blocks", h.AdminDomainBlockCreate)
		r.Post("/admin/domain_blocks/{id}/delete", h.AdminDomainBlockDelete)
		r.Post("/admin/domain_blocks/mode", h.AdminFederationMode)
		r.Post("/admin/domain_blocks/allows", h.AdminDomainAllowCreate)
		r.Post("/admin/domain_blocks/allows/{id}/delete", h.AdminDomainAllowDelete)
		r.Post("/admin/domain_blocks/import", h.AdminFederationImport)
		r.Post("/admin/domain_blocks/commercial-bigtech", h.AdminCommercialBigTech)

		// Bubble instances
		r.Get("/admin/bubbles", h.AdminBubbles)
		r.Post("/admin/bubbles", h.AdminBubbleCreate)
		r.Post("/admin/bubbles/{id}/delete", h.AdminBubbleDelete)

		// Instance health
		r.Get("/admin/instances", h.AdminInstancesList)
		r.Post("/admin/instances/{id}/reset", h.AdminInstanceReset)
		r.Post("/admin/instances/{id}/suspend", h.AdminInstanceToggleSuspend)
		r.Get("/admin/instances/{id}/purge", h.AdminInstancePurgeConfirm)
		r.Post("/admin/instances/{id}/purge", h.AdminInstancePurge)

		// Housekeeping (on-demand maintenance tasks)
		r.Get("/admin/housekeeping", h.AdminHousekeeping)
		r.Post("/admin/housekeeping/prune_remote", h.AdminHousekeepingPruneRemote)
		r.Post("/admin/housekeeping/prune_own", h.AdminHousekeepingPruneOwn)
		r.Post("/admin/housekeeping/prune_stale_media", h.AdminHousekeepingPruneStaleMedia)
		r.Post("/admin/housekeeping/remove_orphaned_media", h.AdminHousekeepingRemoveOrphanedMedia)
		r.Post("/admin/housekeeping/remove_unreferenced_media", h.AdminHousekeepingRemoveUnreferencedMedia)
		r.Post("/admin/housekeeping/regenerate_all_blogs", h.AdminRegenerateAllBlogs)

		// Backup (on-demand manual backup)
		r.Get("/admin/backup", h.AdminBackup)
		r.Post("/admin/backup/create", h.AdminBackupCreate)

		// Instance settings
		r.Get("/admin/settings", h.AdminSettings)
		r.Post("/admin/settings/smtp-test", h.AdminSettings)
		r.Get("/admin/instance", h.AdminInstanceMeta)
		r.Post("/admin/instance", h.AdminInstanceMetaSave)
		h.registerMediaStorageRoutes(r)
	})

	// Static files (embedded htmx, alpine)
	r.Get("/static/*", StaticHandler(h.instanceName()).ServeHTTP)

	// Service worker: served at the root path (not /static/) so its default
	// scope covers the whole origin, with no-cache so updates roll out
	// promptly instead of sitting behind the immutable static-asset cache.
	r.Get("/sw.js", ServiceWorkerHandler(h.instanceName()).ServeHTTP)
	r.Get("/robots.txt", RobotsHandler(h.blogSitemapURL()).ServeHTTP)

	// htmx partial routes with optional auth
	r.With(h.OptionalAuthMiddleware).Get("/partial/timeline/{kind}", h.PartialTimeline)
	r.With(h.OptionalAuthMiddleware).Get("/partial/tag/{tag}", h.PartialTagTimeline)
	r.With(h.OptionalAuthMiddleware).Get("/partial/profile/{username}", h.PartialProfileTimeline)
	r.With(h.OptionalAuthMiddleware).Get("/partial/profile/{username}/live", h.ProfileLivePartial)
	r.With(h.OptionalAuthMiddleware).Get("/partial/status/{id}/reply", h.ReplyComposerPartial)
	r.With(h.OptionalAuthMiddleware).Get("/partial/status/{id}", h.PartialStatus)
	r.With(h.OptionalAuthMiddleware).Get("/partial/status/{id}/preview", h.StatusPreview)
	r.With(h.OptionalAuthMiddleware).Get("/partial/thread/{id}", h.ThreadPartial)
	r.With(h.OptionalAuthMiddleware).Get("/partial/thread/{id}/interactions", h.InteractionPartial)

	// Autocomplete endpoints (return JSON for composer autocomplete)
	r.With(h.OptionalAuthMiddleware).Get("/api/accounts/autocomplete", h.AutocompleteAccounts)
	r.With(h.OptionalAuthMiddleware).Get("/api/tags/autocomplete", h.AutocompleteTags)
	r.With(h.OptionalAuthMiddleware).Get("/api/emojis/autocomplete", h.AutocompleteEmojis)
}

// Context Keys

type contextKey string

const sessionAccountKey contextKey = "webui_account"
const sessionTokenKey contextKey = "webui_token"
const sessionIDKey contextKey = "webui_session_id"

// Session Middleware

func generateSessionToken() (string, string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		token := uuid.New().String()
		return token, sha256Hex(token)
	}
	token := hex.EncodeToString(b)
	return token, sha256Hex(token)
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// allowLogin checks whether a password is acceptable for a given account.
// If the account has a bcrypt password hash, use that (per-user auth).
// Otherwise, fall back to the global shared-secret password (migration path).
// If neither is set and we're on localhost, accept any non-empty password (dev mode).
func (h *Handler) allowLogin(r *http.Request, account *models.Account, submittedPassword string) bool {
	if !account.CanAuthenticate(h.cfg != nil && h.cfg.Auth.RequireEmail, h.cfg != nil && h.cfg.Auth.RequireAdminMFA) {
		slog.Warn("webui: blocked account login attempt", "username", account.Username, "state", account.ModerationState)
		return false
	}

	// Per-user bcrypt password (primary auth path)
	if account.PasswordHash != "" {
		ok := auth.CheckPassword(submittedPassword, account.PasswordHash)
		if !ok {
			slog.Warn("webui: per-user password mismatch", "username", account.Username)
		}
		return ok
	}

	// In multi-user mode, every account must have a per-user password set
	if h.cfg != nil && h.cfg.IsMultiUser() {
		slog.Warn("webui: multi-user mode but account has no password hash", "username", account.Username)
		return false
	}

	// Fallback: global shared-secret password (migration/deprecation path)
	if h.password != "" {
		if subtle.ConstantTimeCompare([]byte(submittedPassword), []byte(h.password)) != 1 {
			slog.Warn("webui: global password mismatch", "username", account.Username)
			return false
		}
		slog.Warn("webui: using deprecated global-password fallback for", "username", account.Username)
		// Auto-migrate: hash the global password into the account
		hash, err := auth.HashPassword(h.password)
		if err == nil {
			account.PasswordHash = hash
			if err := h.store.UpdateAccount(r.Context(), account); err != nil {
				slog.Warn("webui: auto-migration hash write failed", "error", err)
			} else {
				slog.Info("webui: auto-migrated global password to per-account hash", "username", account.Username)
			}
		}
		return true
	}

	// Dev mode: if explicitly enabled via config, allow any non-empty password.
	// never gate this on the Host header - that is attacker-controlled.
	if h.cfg != nil && h.cfg.Auth.DevMode {
		slog.Warn("webui: dev_mode is enabled - allowing any non-empty password for localhost development")
		return submittedPassword != ""
	}
	return false
}

// SessionMiddleware extracts and validates the session cookie.
func (h *Handler) SessionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account, token, session := h.getSessionAccount(r)
		if account == nil {
			// no-store prevents the browser from caching this redirect and
			// replaying it after the session becomes valid (e.g. after a
			// server restart). Without this, hx-boost navigations can serve
			// a stale 302 from cache, bouncing the user to the login page.
			w.Header().Set("Cache-Control", "private, no-store")
			http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
			return
		}
		h.renewSession(w, r, token, session)
		if h.viewerTouch != nil {
			h.viewerTouch(account.ID, r.URL.Path)
		}
		ctx := context.WithValue(r.Context(), sessionAccountKey, account)
		ctx = context.WithValue(ctx, sessionTokenKey, token)
		ctx = context.WithValue(ctx, sessionIDKey, session.ID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (h *Handler) getSessionAccount(r *http.Request) (*models.Account, string, *models.Session) {
	cookie, err := r.Cookie("session")
	if err != nil {
		return nil, "", nil
	}

	tokenHash := sha256Hex(cookie.Value)
	session, err := h.store.GetSessionByTokenHash(r.Context(), tokenHash)
	if err != nil {
		return nil, "", nil
	}

	account, err := h.store.GetAccount(r.Context(), session.AccountID)
	if err != nil || !account.CanAuthenticate(h.cfg != nil && h.cfg.Auth.RequireEmail, h.cfg != nil && h.cfg.Auth.RequireAdminMFA) {
		return nil, "", nil
	}

	return account, cookie.Value, session
}

func (h *Handler) renewSession(w http.ResponseWriter, r *http.Request, token string, session *models.Session) {
	if session == nil || time.Until(session.ExpiresAt) > sessionRenewalWindow {
		return
	}
	expiresAt := time.Now().Add(sessionDuration)
	if err := h.store.UpdateSessionExpiresAt(r.Context(), session.ID, expiresAt); err != nil {
		slog.Warn("webui: renew session", "error", err)
		return
	}
	h.setSessionCookie(w, r, token)
	session.ExpiresAt = expiresAt
}

// OptionalAuthMiddleware adds the account to context if logged in, but doesn't block.
func (h *Handler) OptionalAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account, token, session := h.getSessionAccount(r)
		if account != nil {
			h.renewSession(w, r, token, session)
			if h.viewerTouch != nil {
				h.viewerTouch(account.ID, r.URL.Path)
			}
			ctx := context.WithValue(r.Context(), sessionAccountKey, account)
			ctx = context.WithValue(ctx, sessionTokenKey, token)
			ctx = context.WithValue(ctx, sessionIDKey, session.ID)
			next.ServeHTTP(w, r.WithContext(ctx))
		} else {
			next.ServeHTTP(w, r)
		}
	})
}

func (h *Handler) CSRFMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		token, _ := r.Context().Value(sessionTokenKey).(string)
		submitted := r.Header.Get("X-CSRF-Token")
		if submitted == "" {
			submitted = r.FormValue("csrf_token")
		}
		if token == "" || submitted == "" || subtle.ConstantTimeCompare([]byte(submitted), []byte(csrfToken(token))) != 1 {
			http.Error(w, "Invalid CSRF token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SameOriginFormMiddleware blocks browser-driven login, registration and
// password-reset CSRF. Non-browser clients without Fetch Metadata/Origin
// headers remain compatible.
func (h *Handler) SameOriginFormMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
			http.Error(w, "Cross-site form submission rejected", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host == "" || (!strings.EqualFold(u.Host, r.Host) && !strings.EqualFold(u.Host, h.domain)) {
				http.Error(w, "Cross-site form submission rejected", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func safeLocalPath(raw string) bool {
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.ContainsAny(raw, "\\\r\n") {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && !u.IsAbs() && u.Host == ""
}

// safeReturnPath accepts relative local paths and absolute same-host Referer
// values, converting the latter to a local request URI. It is intentionally
// stricter than safeTemplateURL because it protects post-action redirects.
func safeReturnPath(r *http.Request, raw, fallback string) string {
	if raw == "" {
		return fallback
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return fallback
	}
	if u.IsAbs() {
		if !strings.EqualFold(u.Host, r.Host) {
			return fallback
		}
		raw = u.RequestURI()
	}
	if !safeLocalPath(raw) {
		return fallback
	}
	return raw
}

// safeHTTPRedirectTarget permits only local paths or ordinary HTTP(S) URLs.
// Remote profile and status redirects are a deliberate federation feature,
// but must never emit script, data, credential-bearing, or malformed URLs.
func safeHTTPRedirectTarget(raw, fallback string) string {
	raw = strings.TrimSpace(raw)
	if safeLocalPath(raw) {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fallback
	}
	return u.String()
}

func safeTemplateURL(raw string) template.URL {
	if safeLocalPath(raw) {
		return template.URL(raw)
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return template.URL(u.String())
}

func (h *Handler) secureCookie(r *http.Request) bool {
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return true
	}
	if h.cfg != nil && h.cfg.HTTP.ProxyURI != "" {
		u, err := url.Parse(h.cfg.HTTP.ProxyURI)
		return err == nil && strings.EqualFold(u.Scheme, "https")
	}
	return false
}

func csrfToken(sessionToken string) string {
	return sha256Hex("csrf:" + sessionToken)
}

func getAuthAccount(ctx context.Context) *models.Account {
	account, ok := ctx.Value(sessionAccountKey).(*models.Account)
	if !ok {
		return nil
	}
	return account
}

func getSessionID(ctx context.Context) string {
	id, _ := ctx.Value(sessionIDKey).(string)
	return id
}

func getCSRFToken(ctx context.Context) string {
	sessionToken, _ := ctx.Value(sessionTokenKey).(string)
	if sessionToken == "" {
		return ""
	}
	return csrfToken(sessionToken)
}

// Page Data Types

// PageData is the base data for most pages.
type PageData struct {
	Title          string
	Account        *models.Account
	Statuses       []*models.Status
	Timeline       string // "home", "public", "local", "bubble"
	HideReplies    bool
	HideBoosts     bool
	TimelineCursor string
	CSRFToken      string
	// QuotedStatus, when set, pre-loads the composer to quote this status.
	QuotedStatus *models.Status

	// Composer prefill. EditingID switches the composer into edit mode (posting
	// to /statuses/{id}/edit with an immutable visibility); the Draft* fields
	// seed the textarea, content warning, visibility and language for both edit
	// and "delete & re-draft". ExistingMedia lists the post's current
	// attachments so they can be kept or removed on edit.
	EditingID        string
	RedraftStatusID  string
	RedraftReplyURI  string
	ReplyTo          *models.Status
	DraftContent     string
	DraftHTML        string
	DraftMarkdown    bool
	DraftMarkdownSet bool
	DraftSummary     string
	DraftSensitive   bool
	DraftVisibility  models.Visibility
	DraftLanguage    string
	DraftIsBlogPost  bool
	DraftBlogSlug    string
	CanEditBlogPost  bool
	// DraftExpiresAt is a pre-formatted display string ("" if the post has no
	// expiry), shown next to the expiry control on edit; the control itself
	// always submits an explicit choice rather than prefilling a preset.
	DraftExpiresAt string
	ExistingMedia  []*models.MediaAttachment
}

// ProfileData is the data for the profile page.
type ProfileData struct {
	Title          string
	Account        *models.Account
	Profile        *models.Account
	MovedToURL     string
	MovedToLabel   string
	Lists          []*models.List
	ListAdded      bool
	PinnedStatuses []*models.Status
	Statuses       []*models.Status
	StatusesCount  int
	FollowersCount int
	FollowingCount int
	HideFollowers  bool
	HideFollowing  bool
	Relationship   map[string]interface{}
	Timeline       string
	TimelineCursor string
	CSRFToken      string
	// Handle is the bare "user" or "user@domain" form (no leading "/@"),
	// computed in Go rather than via template printf: Domain is *string, and
	// printf's %s verb doesn't indirect through pointers to basic types the
	// way the template engine's own {{.}} print does - printf "%s@%s" with a
	// *string produced literal garbage like "user@%!s(*string=0x...)" in the
	// URL, which the HTTP server then 400'd on.
	Handle string
	// Fetching is true when this remote account still shows nothing (no
	// posts, no counts) right after we queued its background outbox/actor
	// refresh. The profile-live fragment self-polls while true.
	Fetching  bool
	PollTries int
	// OnlyMedia is the profile's "Media" tab: the same timeline restricted to
	// posts carrying attachments.
	OnlyMedia bool
	// ShowReplies and ShowBoosts are the optional filters on the Posts tab.
	ShowReplies bool
	ShowBoosts  bool
	// OnlyBlog is the profile's "Blog" tab: the account's blog posts only.
	// BlogURL is the separately linked, generated static site.
	OnlyBlog   bool
	HasBlog    bool
	BlogURL    string
	RSSFeedURL string
}

// ThreadData is the data for the thread page.
type ThreadData struct {
	Title        string
	Account      *models.Account
	Status       *models.Status
	Ancestors    []*models.Status
	Descendants  []ThreadReplyData
	FavouritedBy []*models.Account
	RebloggedBy  []*models.Account
	Quotes       []*models.Status
	Reactions    []models.EmojiReactionSummary
	CSRFToken    string
	// Fetching reports that a background crawl is completing this thread, so the
	// template arms HTMX polling to swap in replies/ancestors as they arrive.
	Fetching bool
	Replying bool
}

// ThreadReplyData preserves the reply tree in the flat sequence expected by
// the server-rendered template. Depth is relative to the focused status.
type ThreadReplyData struct {
	Status *models.Status
	Depth  int
}

// buildThreadReplies renders each branch together in chronological sibling
// order. API descendants are globally chronological, which is not sufficient
// for displaying a readable tree when replies span multiple branches.
func buildThreadReplies(focusID string, descendants []*models.Status) []ThreadReplyData {
	children := make(map[string][]*models.Status, len(descendants))
	for _, status := range descendants {
		if status == nil || status.InReplyToID == nil {
			continue
		}
		children[*status.InReplyToID] = append(children[*status.InReplyToID], status)
	}
	for parentID := range children {
		sort.SliceStable(children[parentID], func(i, j int) bool {
			left, right := children[parentID][i], children[parentID][j]
			if left.CreatedAt.Equal(right.CreatedAt) {
				return left.ID < right.ID
			}
			return left.CreatedAt.Before(right.CreatedAt)
		})
	}

	replies := make([]ThreadReplyData, 0, len(descendants))
	seen := map[string]bool{focusID: true}
	var appendBranch func(string, int)
	appendBranch = func(parentID string, depth int) {
		for _, child := range children[parentID] {
			if seen[child.ID] {
				continue
			}
			seen[child.ID] = true
			replies = append(replies, ThreadReplyData{Status: child, Depth: depth})
			appendBranch(child.ID, depth+1)
		}
	}
	appendBranch(focusID, 1)

	// Context data should form a tree rooted at focusID. Keep malformed or
	// truncated input visible at the top reply level rather than dropping it.
	for _, status := range descendants {
		if status == nil || seen[status.ID] {
			continue
		}
		seen[status.ID] = true
		replies = append(replies, ThreadReplyData{Status: status, Depth: 1})
		appendBranch(status.ID, 2)
	}
	return replies
}

// NotificationsData is the data for the notifications page.
type NotificationsData struct {
	Title          string
	Account        *models.Account
	Notifications  interface{}
	UnreadCount    int
	Timeline       string
	CSRFToken      string
	TimelineCursor string
	HasMore        bool
	Filter         string
}

// FollowListData is the data for the followers/following list page.
type FollowListData struct {
	Title       string
	Account     *models.Account
	Profile     *models.Account
	Accounts    []*models.Account
	FollowState map[string]string
	Count       int
	ListKind    string // "followers" or "following"
	NextCursor  string
	Timeline    string
	CSRFToken   string
}

// SettingsData is the data for the settings page.
type SettingsData struct {
	SensitiveRelationshipNotificationsBlocked bool

	Title               string
	Account             *models.Account
	UnreadCount         int
	Timeline            string
	CSRFToken           string
	Domain              string
	Import              *models.AccountImport
	ImportResult        *models.AccountImportResult
	ImportAlias         string
	ImportError         string
	Error               string
	MinPasswordLength   int
	NotificationOptions []notificationPreferenceOption
	SMTPConfigured      bool
	VAPIDPublicKey      string
	WebPushEndpoint     string
	CurrentPushID       string
	WebPushSubscription *models.PushSubscription
	PushSubscriptions   []*models.PushSubscription
	CSPNonce            string
	// Theme/Appearance are the *effective* values for this request, not the
	// stored ones: an account that never picked a theme has an empty column
	// but is looking at the instance default, and the picker must show what
	// is on screen rather than nothing.
	Theme      string
	Appearance string
	// CustomCSS is the stored value, unlike Theme/Appearance above: the
	// textarea has to show what the account actually saved so an edit does not
	// silently start from someone else's default.
	CustomCSS      string
	CustomCSSError string
	// Blog* mirror the pair above for the generated static site. They are
	// always the stored values: nothing else can be looking at them.
	BlogTheme       string
	BlogCustomCSS   string
	BlogCSSError    string
	BlogRebuildSent bool
	Filters         []*models.Filter
	// FilterNotice is the confirmation shown after a filter was created,
	// saved or deleted, and FilterFlash is the id of the card that changed so
	// it can be highlighted. A save round-trips through a redirect, so
	// without these the page comes back looking exactly as it left.
	FilterNotice string
	FilterFlash  string
}

// Remote profile refresh

// OutboxFetchJobKind queues the first-view outbox import off the request
// path (see ProfilePage) so a slow or unreachable remote server can't stall
// the page load.
const OutboxFetchJobKind = "outbox_fetch"

type outboxFetchPayload struct {
	AccountID    string `json:"account_id"`
	MaxItems     int    `json:"max_items"`
	SyncFeatured bool   `json:"sync_featured,omitempty"`
}

// HandleOutboxFetchJob processes the background work requested by a remote
// profile view.
func HandleOutboxFetchJob(store storage.Storage, federator *federation.Federator, inboxProcessor *federation.InboxProcessor) func(ctx context.Context, payload string) error {
	h := &Handler{store: store, federator: federator}
	return func(ctx context.Context, payload string) error {
		var p outboxFetchPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return err
		}
		account, err := store.GetAccount(ctx, p.AccountID)
		if err != nil {
			return err
		}
		var refreshErr error
		if p.MaxItems > 0 {
			refreshErr = h.fetchAndStoreOutbox(ctx, account, p.MaxItems)
		}
		if p.SyncFeatured && inboxProcessor != nil {
			if err := inboxProcessor.SyncFeaturedCollection(ctx, account); refreshErr == nil {
				refreshErr = err
			}
		}
		return refreshErr
	}
}

// fetchAndStoreOutbox fetches the first page of a remote account's outbox
// and stores any new statuses locally. Runs as a background job (see
// HandleOutboxFetchJob) queued when viewing a remote profile for the first
// time. Uses a short timeout so a slow remote server can't hold a worker
// indefinitely.
//
// A returned error propagates ctx cancellation up to the worker pool, which
// on a single-worker deployment cancels jobCtx every time a foreground HTTP
// request comes in (see queue.ForegroundGate) - including the profile-live
// poll requests this job's own results feed. Swallowing that cancellation
// here would make the job report "completed" with nothing actually
// stored; propagating it lets the pool recognise the preemption and
// yield the job back onto the queue to retry moments later, instead of
// losing the fetch.
func (h *Handler) fetchAndStoreOutbox(ctx context.Context, account *models.Account, maxItems int) error {
	if h.federator == nil || account.OutboxURI == "" {
		return nil
	}

	// Check if we already have recent posts for this account
	// (skip outbox fetch if we already have more than 2 cached posts)
	existing, err := h.store.GetAccountStatuses(ctx, "", account.ID, nil, nil, nil, 3, false, false, false, false)
	if err == nil && len(existing) >= 3 {
		return nil
	}

	// Use a short timeout so we don't block the page load
	fetchCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	slog.Info("webui: fetching remote outbox", "account", account.Username, "outbox", account.OutboxURI)

	items, err := h.federator.FetchOutboxFirst(fetchCtx, account.OutboxURI, maxItems)
	if err != nil {
		slog.Warn("webui: failed to fetch remote outbox", "outbox", account.OutboxURI, "error", err)
		return err
	}

	stored := 0
	for _, item := range items {
		// A canceled ctx (preemption, see doc comment above) fails every
		// remaining store call identically; stop instead of burning through
		// the rest of the page just to log the same error repeatedly.
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Outbox items are activities (Create, Announce, etc.)
		// We're interested in Create activities with a Note object
		activityType, _ := item["type"].(string)

		switch activityType {
		case "Create":
			obj, ok := item["object"].(map[string]interface{})
			if !ok {
				continue
			}
			objType, _ := obj["type"].(string)
			if objType != "Note" && objType != "Article" && objType != "Page" {
				continue
			}
			status, err := h.parseOutboxNote(ctx, obj, account)
			if err != nil {
				slog.Warn("webui: failed to parse outbox note", "error", err)
				continue
			}
			// Check for duplicate
			existing, _ := h.store.GetStatusByURI(ctx, status.URI)
			if existing != nil {
				continue
			}
			if err := h.store.CreateStatus(ctx, status); err != nil {
				slog.Warn("webui: failed to store outbox status", "uri", status.URI, "error", err)
				continue
			}
			if err := h.createRemoteStatusMedia(ctx, status, account.ID); err != nil {
				slog.Warn("webui: failed to store outbox media", "uri", status.URI, "error", err)
			}
			stored++

		case "Announce":
			// Backfill only imports Create'd posts; boosts are
			// dropped; fetching+resolving the boosted object needs the same
			// remote-fetch path as live inbox processing. Upgrade if remote
			// profile backfill is expected to show boosts in history.
			continue
		}
	}

	if stored > 0 {
		slog.Info("webui: stored outbox posts", "account", account.Username, "count", stored)
	}
	return nil
}

// parseOutboxNote parses a Note/Article object from an outbox into a Status.
func (h *Handler) parseOutboxNote(ctx context.Context, obj map[string]interface{}, account *models.Account) (*models.Status, error) {
	objectID, _ := obj["id"].(string)
	rawContent, _ := obj["content"].(string)
	summary, _ := obj["summary"].(string)
	sensitive, _ := obj["sensitive"].(bool)
	published, _ := obj["published"].(string)
	objectURL, _ := obj["url"].(string)

	if objectID == "" {
		return nil, fmt.Errorf("note missing id")
	}

	pubTime := time.Now()
	if published != "" {
		if t, err := time.Parse(time.RFC3339, published); err == nil {
			pubTime = t
		} else if t, err := time.Parse("2006-01-02T15:04:05Z", published); err == nil {
			pubTime = t
		}
	}

	// Determine visibility from addressing
	visibility := models.VisibilityPublic // default for outbox public posts
	toStrs := toStringSlice(obj["to"])
	ccStrs := toStringSlice(obj["cc"])

	toPublic := false
	ccPublic := false
	for _, s := range toStrs {
		if s == "https://www.w3.org/ns/activitystreams#Public" {
			toPublic = true
		}
	}
	for _, s := range ccStrs {
		if s == "https://www.w3.org/ns/activitystreams#Public" {
			ccPublic = true
		}
	}

	if !toPublic && !ccPublic {
		// Not a public post - skip or mark as unlisted
		visibility = models.VisibilityUnlisted
	}

	// Parse inReplyTo
	var inReplyToURI *string
	var inReplyToID *string
	if irt, ok := obj["inReplyTo"].(string); ok && irt != "" {
		inReplyToURI = &irt
		// Try to find parent locally
		parent, err := h.store.GetStatusByURI(ctx, irt)
		if err == nil {
			inReplyToID = &parent.ID
		}
	}

	// Parse media attachments
	var mediaAttachments []*models.MediaAttachment
	if attachments, ok := obj["attachment"].([]interface{}); ok {
		for _, a := range attachments {
			att, ok := a.(map[string]interface{})
			if !ok {
				continue
			}
			if media, ok := federation.ParseMediaAttachment(att, account.ID); ok {
				mediaAttachments = append(mediaAttachments, media)
			}
		}
	}

	// Parse mentions and tags
	var mentions []*models.Mention
	var tags []*models.Tag
	if tagList, ok := obj["tag"].([]interface{}); ok {
		for _, t := range tagList {
			tag, ok := t.(map[string]interface{})
			if !ok {
				continue
			}
			tagType, _ := tag["type"].(string)
			switch tagType {
			case "Mention":
				href, _ := tag["href"].(string)
				name, _ := tag["name"].(string)
				if href != "" {
					// Try to find the mentioned account (might be remote)
					mentionedAccount, err := h.store.GetAccountByURI(ctx, href)
					if err == nil {
						mentions = append(mentions, &models.Mention{
							AccountID: mentionedAccount.ID,
							Username:  mentionedAccount.Username,
							URL:       href,
							Acct:      name,
						})
					}
				}
			case "Hashtag":
				tagName, _ := tag["name"].(string)
				tagName = strings.TrimPrefix(tagName, "#")
				if tagName != "" {
					tagObj, err := h.store.GetOrCreateTag(ctx, tagName)
					if err == nil {
						tags = append(tags, tagObj)
					}
				}
			}
		}
	}

	url := objectURL
	if url == "" {
		url = objectID
	}

	status := &models.Status{
		ID:            storage.NewStatusID(pubTime),
		URI:           objectID,
		URL:           url,
		AccountID:     account.ID,
		ContentHTML:   content.SanitizeHTMLStr(rawContent),
		ContentSource: rawContent,
		Summary:       summary,
		Sensitive:     sensitive,
		Visibility:    visibility,
		InReplyToID:   inReplyToID,
		InReplyToURI:  inReplyToURI,
		CreatedAt:     pubTime,
		UpdatedAt:     time.Now(),
	}

	// Convert pointer slices to value slices for storage
	if len(mediaAttachments) > 0 {
		status.MediaAttachments = make([]models.MediaAttachment, len(mediaAttachments))
		for i, m := range mediaAttachments {
			if m != nil {
				status.MediaAttachments[i] = *m
			}
		}
	}
	if len(mentions) > 0 {
		status.Mentions = make([]models.Mention, len(mentions))
		for i, m := range mentions {
			if m != nil {
				status.Mentions[i] = *m
			}
		}
	}
	if len(tags) > 0 {
		status.Tags = make([]models.Tag, len(tags))
		for i, t := range tags {
			if t != nil {
				status.Tags[i] = *t
			}
		}
	}

	return status, nil
}

func (h *Handler) createRemoteStatusMedia(ctx context.Context, status *models.Status, accountID string) error {
	for i := range status.MediaAttachments {
		media := status.MediaAttachments[i]
		if media.URL == "" {
			continue
		}
		media.StatusID = &status.ID
		media.AccountID = accountID
		if media.PreviewURL == "" && media.Type != "video" {
			media.PreviewURL = media.URL
		}
		if err := h.store.CreateMediaAttachment(ctx, &media); err != nil {
			return err
		}
	}
	return nil
}

// toStringSlice converts a JSON field that might be a string or []interface{} to []string.
func toStringSlice(v interface{}) []string {
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case []interface{}:
		out := make([]string, 0, len(val))
		for _, item := range val {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{val}
	default:
		return nil
	}
}

// Page Rendering

// Page wraps page content for rendering inside base.html.
type Page struct {
	Title                                                          string
	InstanceName                                                   string
	Account                                                        *models.Account
	NotificationCount                                              int
	CSRFToken                                                      string
	ContentHTML                                                    template.HTML
	IsAdmin                                                        bool
	AdminSection                                                   string
	AdminReports, AdminPending, AdminInstances, AdminQueueProblems int
	// BackURL is a same-origin fallback target for the back control, derived
	// from the Referer. JS browsers use history.back(); text/no-JS browsers
	// follow this href. Empty when there's nowhere sensible to go back to.
	BackURL     string
	CurrentPath string
	CSPNonce    string
	Locale      string
	RSSFeedURL  string
	// Theme/Appearance become data-theme/data-appearance on <html>; ThemeCSS
	// is the extra stylesheet to load after app.css (empty for the default
	// theme). ThemeColor* fill the <meta name="theme-color"> pair.
	Theme           string
	Appearance      string
	ThemeCSS        string
	ThemeColorLight string
	ThemeColorDark  string
	// CustomCSS is the viewer's own stylesheet, inlined after ThemeCSS so it
	// wins on source order. Empty unless a logged-in account stored one.
	CustomCSS template.CSS
}

func adminSection(path string) string {
	switch {
	case path == "/admin":
		return "overview"
	case strings.HasPrefix(path, "/admin/reports"):
		return "reports"
	case strings.HasPrefix(path, "/admin/domain_blocks"):
		return "domain-blocks"
	case strings.HasPrefix(path, "/admin/audit"):
		return "audit"
	case strings.HasPrefix(path, "/admin/accounts"):
		return "accounts"
	case strings.HasPrefix(path, "/admin/pending"):
		return "pending"
	case strings.HasPrefix(path, "/admin/invites"):
		return "invites"
	case strings.HasPrefix(path, "/admin/instances"):
		return "instances"
	// Checked after /admin/instances, which shares this prefix.
	case strings.HasPrefix(path, "/admin/instance"):
		return "instance-profile"
	case strings.HasPrefix(path, "/admin/bubbles"):
		return "bubbles"
	case strings.HasPrefix(path, "/admin/queue"):
		return "queue"
	case strings.HasPrefix(path, "/admin/housekeeping"):
		return "housekeeping"
	case strings.HasPrefix(path, "/admin/backup"):
		return "backup"
	case strings.HasPrefix(path, "/admin/media-storage"):
		return "media-storage"
	case strings.HasPrefix(path, "/admin/settings"):
		return "configuration"
	default:
		return "overview"
	}
}

// backURL returns a same-origin path to navigate "back" to, taken from the
// request Referer. It returns "" when there is no usable referer or it points
// at the current page (e.g. on a reload), so the back control stays hidden.
func backURL(r *http.Request) string {
	if ref := r.Header.Get("Referer"); ref != "" {
		if u, err := url.Parse(ref); err == nil {
			// Only follow same-origin referers.
			if u.Host == "" || u.Host == r.Host {
				target := u.Path
				if u.RawQuery != "" {
					target += "?" + u.RawQuery
				}
				if target != "" && target != r.URL.Path && target != r.URL.RequestURI() {
					return target
				}
			}
		}
	}
	// No usable referer (or it points back at this same page, as happens after a
	// POST->redirect that lands on the page that submitted the form). Fall back to
	// the parent path so the Back control stays put instead of vanishing -
	// e.g. /admin/reports -> /admin, /settings/moderation -> /settings.
	return parentPath(r.URL.Path)
}

// parentPath returns the path one level up, or "/" for top-level paths.
func parentPath(p string) string {
	p = strings.TrimRight(p, "/")
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

// OAuthAuthorizePage is the view model for the themed OAuth consent screen.
type OAuthAuthorizePage struct {
	AppName, AppWebsite, Username                     string
	ClientID, RedirectURI, ResponseType, Scope, State string
	CodeChallenge, CodeChallengeMethod, CSRFToken     string
	Scopes                                            []string

	// The consent links carry the client's own URL scheme, which
	// html/template would otherwise replace with #ZgotmplZ. They are built by
	// internal/oauth from a callback already matched against the
	// registration, so they are not attacker-controlled.
	AllowURL, DenyURL template.URL
}

// RenderOAuthAuthorize renders OAuth consent with the same layout and theme as
// the rest of littleFedi while preserving the authenticated session context.
func (h *Handler) RenderOAuthAuthorize(w http.ResponseWriter, r *http.Request, data OAuthAuthorizePage) {
	account, token, session := h.getSessionAccount(r)
	if account == nil || token == "" {
		w.Header().Set("Cache-Control", "private, no-store")
		http.Redirect(w, r, "/auth/sign_in", http.StatusFound)
		return
	}
	h.renewSession(w, r, token, session)
	ctx := context.WithValue(r.Context(), sessionAccountKey, account)
	ctx = context.WithValue(ctx, sessionTokenKey, token)
	ctx = context.WithValue(ctx, sessionIDKey, session.ID)
	h.renderPage(w, r.WithContext(ctx), "Authorize - littleFedi", data, "oauth_authorize.html")
}

func (h *Handler) renderPage(w http.ResponseWriter, r *http.Request, title string, data interface{}, templateName string) {
	account := getAuthAccount(r.Context())
	sessionToken, _ := r.Context().Value(sessionTokenKey).(string)
	csrf := ""
	if sessionToken != "" {
		csrf = csrfToken(sessionToken)
	}
	locale := h.resolveLocale(r, account)
	tmpl := h.templatesByLocale[locale]
	if tmpl == nil {
		tmpl = h.templates
	}

	// For direct htmx interactions (like, boost, infinite scroll - not boosted links),
	// return just the page fragment without the base layout.
	// Boosted links (HX-Boosted) need the full page so htmx can extract body content.
	// htmx's history-cache-miss reload (hx-history="false" means every back
	// navigation takes this path) sends HX-Request without HX-Boosted, so it
	// needs the same full-page treatment or the layout chrome disappears.
	isHtmx := r.Header.Get("HX-Request") == "true"
	isBoosted := r.Header.Get("HX-Boosted") == "true" || r.Header.Get("HX-History-Restore-Request") == "true"

	if isHtmx && !isBoosted {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "private, no-store, no-transform")
		if err := tmpl.ExecuteTemplate(w, templateName, data); err != nil {
			slog.Error("webui: htmx render error", "template", templateName, "error", err)
			http.Error(w, "Render error", http.StatusInternalServerError)
		}
		return
	}

	// Render the page content to a pooled buffer.
	contentBuf := getRenderBuffer()
	if err := tmpl.ExecuteTemplate(contentBuf, templateName, data); err != nil {
		putRenderBuffer(contentBuf)
		slog.Error("webui: content render error", "template", templateName, "error", err)
		http.Error(w, "Render error", http.StatusInternalServerError)
		return
	}

	// Wrap in base layout
	theme := h.resolveTheme(r, account)
	p := &Page{
		Title:           title,
		InstanceName:    h.instanceName(),
		Account:         account,
		CSRFToken:       csrf,
		ContentHTML:     template.HTML(contentBuf.String()),
		BackURL:         backURL(r),
		CurrentPath:     r.URL.Path,
		IsAdmin:         strings.HasPrefix(r.URL.Path, "/admin"),
		AdminSection:    adminSection(r.URL.Path),
		CSPNonce:        r.Header.Get("X-littleFedi-CSP-Nonce"),
		Locale:          locale,
		Theme:           theme.ID,
		Appearance:      h.resolveAppearance(r, account),
		ThemeCSS:        theme.Stylesheet,
		ThemeColorLight: theme.ThemeColorLight,
		ThemeColorDark:  theme.ThemeColorDark,
	}
	p.CustomCSS = customCSSFor(account)
	if profile, ok := data.(*ProfileData); ok {
		p.RSSFeedURL = profile.RSSFeedURL
	}
	putRenderBuffer(contentBuf)
	if account != nil {
		if n, err := h.store.CountUnreadNotificationsFiltered(r.Context(), account.ID, notificationExcludedTypes(account)); err == nil {
			p.NotificationCount = n
		}
	}
	if p.IsAdmin {
		if n, err := h.store.CountOpenReports(r.Context()); err == nil {
			p.AdminReports = n
		}
		if pending, err := h.store.GetPendingApprovals(r.Context()); err == nil {
			p.AdminPending = len(pending)
		}
		if n, err := h.store.CountUnreachableInstances(r.Context()); err == nil {
			p.AdminInstances = n
		}
		now := time.Now()
		if stats, err := h.store.GetQueueStats(r.Context(), now, now.Add(-queueStaleTimeout(h.cfg))); err == nil {
			_, _, p.AdminQueueProblems = assessQueue(stats, metrics.Snapshot(), now)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Prevent CDN caching of user-specific pages and disable content
	// rewriting (e.g. Cloudflare email obfuscation on @-handles).
	// no-store (not just no-cache) ensures the browser never serves a
	// stale authenticated page from cache after a server restart -
	// critical because the per-request CSP nonce and CSRF token make
	// cached copies unusable anyway.
	w.Header().Set("Cache-Control", "private, no-store, no-transform")
	pageBuf := getRenderBuffer()
	if err := tmpl.ExecuteTemplate(pageBuf, "base", p); err != nil {
		putRenderBuffer(pageBuf)
		slog.Error("webui: base render error", "error", err)
		http.Error(w, "Render error", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(pageBuf.Bytes())
	putRenderBuffer(pageBuf)
}

func (h *Handler) instanceName() string {
	if h.cfg != nil && strings.TrimSpace(h.cfg.Instance.Name) != "" {
		return strings.TrimSpace(h.cfg.Instance.Name)
	}
	return "littleFedi"
}

func (h *Handler) renderError(w http.ResponseWriter, r *http.Request, message string) {
	w.WriteHeader(http.StatusInternalServerError)
	h.renderPage(w, r, h.pageTitle(r, "page.error_title"), map[string]interface{}{"Message": message}, "error.html")
}

// Template Format Helpers

func formatTime(t time.Time) string {
	return t.UTC().Format("Jan 2, 2006, 15:04 UTC")
}

func formatTimeISO(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func formatTimeRelative(t time.Time) string {
	diff := time.Since(t)

	switch {
	case diff < time.Minute:
		return "just now"
	case diff < time.Hour:
		mins := int(diff.Minutes())
		if mins == 1 {
			return "1m"
		}
		return fmt.Sprintf("%dm", mins)
	case diff < 24*time.Hour:
		hours := int(diff.Hours())
		if hours == 1 {
			return "1h"
		}
		return fmt.Sprintf("%dh", hours)
	case diff < 7*24*time.Hour:
		days := int(diff.Hours() / 24)
		if days == 1 {
			return "1d"
		}
		return fmt.Sprintf("%dd", days)
	default:
		return t.Format("Jan 2")
	}
}

// formatTimePtr is formatTime for the common *time.Time case (e.g.
// Status.ExpiresAt), since Go templates don't auto-dereference function
// arguments.
func formatTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return formatTime(*t)
}

// formatTimeISOPtr renders an RFC3339 UTC timestamp for JS to read off a
// data attribute and convert to the viewer's local timezone client-side.
// The server-rendered text (formatTimePtr, labelled "UTC") stays as the
// no-JS fallback, so this is additive progressive enhancement only - it
// doesn't vary the HTML per viewer, so page caching is unaffected.
func formatTimeISOPtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return formatTimeISO(*t)
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
}

func visibilityIcon(v models.Visibility) string {
	switch v {
	case models.VisibilityPublic:
		return "🌍"
	case models.VisibilityUnlisted:
		return "🔗"
	case models.VisibilityPrivate:
		return "🔒"
	case models.VisibilityDirect:
		return "✉️"
	default:
		return "🌍"
	}
}

func visibilityLabel(v models.Visibility, locale string) string {
	switch v {
	case models.VisibilityPublic:
		return i18n.T(locale, "visibility.public")
	case models.VisibilityUnlisted:
		return i18n.T(locale, "visibility.unlisted")
	case models.VisibilityPrivate:
		return i18n.T(locale, "visibility.private")
	case models.VisibilityDirect:
		return i18n.T(locale, "visibility.direct")
	case models.VisibilityLocal:
		return i18n.T(locale, "visibility.local")
	case models.VisibilityLocalUnlisted:
		return i18n.T(locale, "visibility.local_unlisted")
	default:
		return i18n.T(locale, "visibility.public")
	}
}

func truncateText(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

func jsValue(v interface{}) template.JS {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return template.JS(b)
}

// acceptsActivityPub reports whether the client prefers an ActivityPub
// representation of a web permalink (snac and others dereference pasted
// /@user/{id} URLs this way).
func acceptsActivityPub(r *http.Request) bool {
	return httpneg.ActivityPubPreferred(strings.Join(r.Header.Values("Accept"), ","))
}

// accountPath returns the local web path for an account's profile, using the
// full @user@domain handle for remote accounts and @user for local ones.
func accountPath(account *models.Account) string {
	if account == nil {
		return "#"
	}
	if account.Domain != nil && *account.Domain != "" {
		return "/@" + account.Username + "@" + *account.Domain
	}
	return "/@" + account.Username
}

// profilePath sends signed-out visitors straight to remote accounts while
// keeping the local profile view available to signed-in users.
func profilePath(account, viewer *models.Account) string {
	if viewer == nil && account != nil && account.Domain != nil && account.ActorURI != "" {
		return account.ActorURI
	}
	return accountPath(account)
}

// statusPath returns the local permalink path for a status.
func statusPath(status *models.Status) string {
	if status == nil || status.Account == nil {
		return "#"
	}
	// The fragment makes the selected post, rather than the first ancestor,
	// the browser's navigation target before any enhancement JavaScript runs.
	return accountPath(status.Account) + "/" + status.ID + "#status-" + status.ID
}

func interactionPath(status *models.Status) string {
	if status == nil || status.Account == nil {
		return "#"
	}
	return accountPath(status.Account) + "/" + status.ID + "?refresh_interactions=1#status-interactions-" + status.ID
}

func statusInteractionsPath(status *models.Status) string {
	if status == nil || status.Account == nil {
		return "#"
	}
	return accountPath(status.Account) + "/" + status.ID + "#status-interactions-" + status.ID
}

func replyPath(status *models.Status) string {
	if status == nil || status.Account == nil {
		return "#"
	}
	return accountPath(status.Account) + "/" + status.ID + "?reply=1"
}

// statusShareURL returns a remote post's public URL on its home instance.
// Local posts keep the local permalink, which app.js makes absolute before
// copying it.
func statusShareURL(status *models.Status) string {
	if status != nil && status.Account != nil && status.Account.Domain != nil {
		for _, raw := range []string{status.URL, status.URI} {
			u, err := url.Parse(strings.TrimSpace(raw))
			if err == nil && u.User == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") {
				return u.String()
			}
		}
	}
	return statusPath(status)
}

// blogPostPath returns the public blog permalink for a status that has been
// published as a blog post, or "" if it hasn't (blog posts are always local).
func blogPostPath(status *models.Status) string {
	if status == nil || status.Account == nil || !status.IsBlogPost || status.BlogSlug == "" {
		return ""
	}
	return accountPath(status.Account) + "/blog/" + status.BlogSlug + "/"
}

func replyMention(status *models.Status) string {
	if status == nil || status.Account == nil {
		return ""
	}
	return accountMention(status.Account)
}

// A status body past either threshold renders collapsed behind a "show more"
// disclosure, so one essay cannot push a whole timeline off the screen - the
// small phone-sized PWA viewport being the case that hurts. Both numbers are
// measured on the stored HTML, tags included: markup inflates the character
// count a little, which is fine, the threshold only has to tell a paragraph
// apart from an essay. The block count catches the other shape of long post -
// a poem, a list, a code block - which is many short lines and few characters.
const (
	longStatusChars  = 1000
	longStatusBlocks = 12
)

// isLongStatus reports whether contentHTML should be collapsed in a card. The
// answer depends on the content alone, never on the viewer, so the collapsed
// markup is safe to keep in the shared status-card cache.
func isLongStatus(contentHTML string) bool {
	if len(contentHTML) > longStatusChars {
		return true
	}
	blocks := strings.Count(contentHTML, "</p>") +
		strings.Count(contentHTML, "<br") +
		strings.Count(contentHTML, "</li>")
	return blocks > longStatusBlocks
}

func replyMentions(status *models.Status, viewer *models.Account) string {
	if status == nil {
		return ""
	}
	mentions := []string{replyMention(status)}
	seen := make(map[string]bool, len(status.Mentions)+1)
	if viewer != nil {
		seen[viewer.ID] = true
	}
	if status.Account != nil {
		seen[status.Account.ID] = true
	}
	for _, mention := range status.Mentions {
		if mention.Acct == "" || seen[mention.AccountID] {
			continue
		}
		seen[mention.AccountID] = true
		mentions = append(mentions, "@"+strings.TrimPrefix(mention.Acct, "@"))
	}
	return strings.TrimSpace(strings.Join(mentions, " "))
}

func accountMention(account *models.Account) string {
	if account == nil {
		return ""
	}
	if account.Domain != nil && *account.Domain != "" {
		return "@" + account.Username + "@" + *account.Domain
	}
	return "@" + account.Username
}

// NotificationCount serves the unread notification badge.
func (h *Handler) NotificationCount(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		w.Write([]byte("0"))
		return
	}

	count, err := h.store.CountUnreadNotificationsFiltered(r.Context(), account.ID, notificationExcludedTypes(account))
	if err != nil {
		count = 0
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(strconv.Itoa(count)))
}

// Multi-User Helpers

// minPasswordLen returns the minimum password length from config.
func minPasswordLen(cfg *config.Config) int {
	if cfg == nil {
		return accounts.DefaultMinPasswordLength
	}
	if cfg.Auth.MinPasswordLength > 0 {
		return cfg.Auth.MinPasswordLength
	}
	return accounts.DefaultMinPasswordLength
}

// buildRegConfig builds a RegistrationConfig from the handler's config.
func buildRegConfig(cfg *config.Config) *accounts.ConfigAdapter {
	if cfg == nil {
		return &accounts.ConfigAdapter{
			Registrations:     false,
			MinPasswordLength: accounts.DefaultMinPasswordLength,
		}
	}
	return &accounts.ConfigAdapter{
		MultiUser:         cfg.IsMultiUser(),
		Registrations:     cfg.Auth.Registrations,
		InviteRequired:    cfg.Auth.InviteRequired,
		MinPasswordLength: cfg.Auth.MinPasswordLength,
		RequireApproval:   cfg.Auth.RequireApproval,
		RequireEmail:      cfg.Auth.RequireEmail,
	}
}

// Rate Limit Helpers

// checkAuthRate implements a simple sliding-window rate limiter for auth endpoints.
// Returns true if the request is allowed, false if rate limit exceeded.
func (h *Handler) checkAuthRate(bucket *map[string][]time.Time, key string, maxRequests int, window time.Duration) bool {
	h.authRateMu.Lock()
	defer h.authRateMu.Unlock()

	now := time.Now()
	cutoff := now.Add(-window)

	// Clean up stale entries periodically
	if len(*bucket) > 1000 {
		for k, attempts := range *bucket {
			if len(attempts) == 0 || attempts[len(attempts)-1].Before(cutoff) {
				delete(*bucket, k)
			}
		}
	}

	attempts := (*bucket)[key]
	kept := attempts[:0]
	for _, attempt := range attempts {
		if !attempt.Before(cutoff) {
			kept = append(kept, attempt)
		}
	}
	if len(kept) >= maxRequests {
		(*bucket)[key] = kept
		return false
	}
	(*bucket)[key] = append(kept, now)
	return true
}

// clientIP reads the address normalized by the server's trusted-proxy
// middleware. It ignores forwarding headers here so auth endpoint
// limits cannot be bypassed by a directly connected client.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	if ip := net.ParseIP(strings.Trim(r.RemoteAddr, "[]")); ip != nil {
		return ip.String()
	}
	return r.RemoteAddr
}

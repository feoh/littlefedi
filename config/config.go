// Package config loads littleFedi's TOML configuration and environment overrides.
package config

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/blogslug"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/i18n"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/keycrypt"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mesh"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/version"
)

// Config is the top-level configuration structure.
type Config struct {
	// Domain is the legacy name for LocalDomain. It remains supported so
	// existing configurations and callers keep working.
	Domain      string `toml:"domain"`       // deprecated: use local_domain
	LocalDomain string `toml:"local_domain"` // account handle domain, e.g. "example.com"
	WebDomain   string `toml:"web_domain"`   // public HTTP/AP host; defaults to LocalDomain
	SingleUser  string `toml:"single_user"`  // local account handle (without domain)
	// SelfDestruct freezes the instance and delivers Delete(Person) for every
	// local identity to the remote inboxes already known to this instance.
	SelfDestruct bool `toml:"self_destruct"`
	// LowPower applies a defaults layer tuned for Pi-Zero-class hardware
	// (single core, <=512 MB RAM, SD storage) before the config file and env
	// are read, so any value set explicitly still wins. Env:
	// LITTLEFEDI_LOW_POWER=1 (env decides even over the file's low_power).
	LowPower              bool          `toml:"low_power"`
	BackgroundMaintenance bool          `toml:"background_maintenance"`
	Admins                []string      `toml:"admins"`           // list of admin usernames (multi-user mode)
	ContactUsername       string        `toml:"contact_username"` // instance contact account (defaults to first admin)
	DB                    DB            `toml:"db"`
	Streaming             Streaming     `toml:"streaming"`
	Queue                 Queue         `toml:"queue"`
	Media                 Media         `toml:"media"`
	Federation            Fed           `toml:"federation"`
	RateLimits            RateLimits    `toml:"rate_limits"`
	HTTP                  HTTP          `toml:"http"`
	Web                   Web           `toml:"web"`
	Auth                  Auth          `toml:"auth"`
	Housekeeping          Housekeeping  `toml:"housekeeping"`
	Observability         Obs           `toml:"observability"`
	SMTP                  SMTP          `toml:"smtp"`
	Push                  Push          `toml:"push"`
	Migration             Migration     `toml:"migration"`
	Instance              Instance      `toml:"instance"`
	Backup                Backup        `toml:"backup"`
	Cache                 Cache         `toml:"cache"`
	Blog                  Blog          `toml:"blog"`
	Mesh                  Mesh          `toml:"mesh"`
	Lighthouse            Lighthouse    `toml:"lighthouse"`
	Directory             MeshDirectory `toml:"directory"`
}

// Mesh configures littleMesh, the transport that federates without a public
// IP address, a DNS name, or an inbound port. A node is named by its own
// public key, so enabling this on an instance that has no domain of its own
// also decides that instance's ActivityPub domain.
type Mesh struct {
	Enabled bool `toml:"enabled"`
	// Exposure controls routes reachable over inbound mesh circuits. Allowlist
	// grants full access only to the source node IDs in FullAccess.
	Exposure   mesh.GatewayMode `toml:"exposure"`
	FullAccess []string         `toml:"full_access"`
	// Suffix is the mesh suffix of this node's domain, without the node ID,
	// e.g. "mesh.faro.example". Empty derives it from the first lighthouse,
	// which is the normal case.
	Suffix string `toml:"suffix"`
	// Lighthouses are the relays to register with, as "host[:port]" or
	// "host[:port]@node-id" to pin the relay's identity. Pinning protects
	// circuit setup from a man-in-the-middle; the payload is protected either
	// way by the end-to-end session inside the circuit.
	Lighthouses []string `toml:"lighthouses"`
	// DiscoverLighthouses permits mesh URLs with foreign suffixes to select an
	// unconfigured relay derived from DNS. Disabled by default to keep egress
	// limited to relays explicitly trusted by the operator.
	DiscoverLighthouses bool `toml:"discover_lighthouses"`
	// DirectListen accepts unrelayed connections, for a node that does have a
	// reachable address. Optional and independent of the lighthouses.
	DirectListen string `toml:"direct_listen"`
	// DirectAdvertise overrides the addresses peers are told to try. Empty
	// advertises whatever DirectListen bound to, unless that was a wildcard.
	DirectAdvertise []string `toml:"direct_advertise"`
	// AllowPrivateDirect permits direct connections to private addresses,
	// which is what lets two nodes on one LAN talk without the relay. It
	// relaxes the same guard that stops a remote peer from steering
	// connections into the operator's network, so leave it off unless the
	// mesh really is local.
	AllowPrivateDirect bool `toml:"allow_private_direct"`
	// MaxCircuits bounds concurrent circuits in both directions.
	MaxCircuits int `toml:"max_circuits"`
	// DialTimeout bounds one outbound circuit setup.
	DialTimeout time.Duration `toml:"dial_timeout"`
	// DirectoryURL resolves stable node IDs and readable aliases to signed,
	// replaceable lighthouse lists.
	DirectoryURL string `toml:"directory_url"`
	// DirectoryState persists verified cache entries and rollback high-water
	// marks across restarts.
	DirectoryState string `toml:"directory_state"`
	// Alias is the readable local-domain name assigned to this node.
	Alias string `toml:"alias"`
	// RecordTTL and PublishInterval control the signed reachability record.
	RecordTTL       time.Duration `toml:"record_ttl"`
	PublishInterval time.Duration `toml:"publish_interval"`
}

// MeshDirectory configures the persistent community registry served by
// `littlefedi directory`.
type MeshDirectory struct {
	Listen           string `toml:"listen"`
	StateFile        string `toml:"state_file"`
	OpenRegistration bool   `toml:"open_registration"`
	MaxRecords       int    `toml:"max_records"`
}

// Lighthouse configures the relay served by `littlefedi lighthouse`. It is
// read only by that subcommand; an ordinary instance ignores it.
type Lighthouse struct {
	// Listen is the control and relay address.
	Listen string `toml:"listen"`
	// GatewayListen, when set, serves the HTTPS front door that makes relayed
	// nodes visible to the ordinary fediverse. Put it behind a reverse proxy
	// holding a wildcard certificate for "*.mesh.<this-host>".
	GatewayListen string `toml:"gateway_listen"`
	// GatewayMode controls whether the HTTPS gateway exposes only federation
	// routes, the whole instance, or full access for selected node IDs.
	GatewayMode mesh.GatewayMode `toml:"gateway_mode"`
	// Exposure accepts the earlier shorthand used by initial littleMesh
	// configs. GatewayMode is the canonical name.
	Exposure mesh.GatewayMode `toml:"exposure"`
	// GatewayFullAccess lists nodes receiving full access in allowlist mode.
	GatewayFullAccess []string `toml:"gateway_full_access"`
	// Allowlist, when non-empty, restricts the relay to these node IDs.
	Allowlist []string `toml:"allowlist"`
	// MaxBytesPerNodePerHour caps relayed traffic charged to one node.
	// Zero means unlimited, which is only appropriate for a private relay.
	MaxBytesPerNodePerHour int64 `toml:"max_bytes_per_node_per_hour"`
	// MaxCircuitsPerNode caps concurrent circuits one node may hold.
	MaxCircuitsPerNode int `toml:"max_circuits_per_node"`
	// MaxConnections caps all accepted relay connections.
	MaxConnections int `toml:"max_connections"`
	// MaxConnectionsPerIP limits one source before TLS identity, preventing
	// node-key rotation from bypassing connection limits.
	MaxConnectionsPerIP int `toml:"max_connections_per_ip"`
}

// Cache configures the in-RAM cache that speeds up timeline/profile
// navigation on low-power devices without an external service like Redis. When
// Enabled, a byte-bounded LRU (split between a viewer-neutral entity cache behind
// the storage layer and a short-TTL page cache) is kept in memory; it is fully
// transparent and enabled by default (set enabled = false to opt out).
type Cache struct {
	Enabled bool `toml:"enabled"`
	// MaxMemoryMB is the total RAM budget for the cache, in megabytes. It is
	// split internally between the entity and page layers and enforced by LRU
	// eviction.
	MaxMemoryMB int `toml:"max_memory_mb"`
	// EntityTTL bounds how long a cached status/account may be served before a
	// re-read, as a backstop for any invalidation path not covered explicitly.
	EntityTTL string `toml:"entity_ttl"`
	// PageTTL bounds how long a rendered timeline/profile page is reused. It is
	// the window within which another user's new posts become visible.
	PageTTL string `toml:"page_ttl"`
	// ComponentTTL bounds per-viewer enriched status components. Components are
	// invalidated precisely on edits/deletes/viewer interactions, so this only
	// bounds how stale embedded favourite/boost counts may get; a long TTL keeps
	// enrichment warm across sparse visits on low-power devices.
	ComponentTTL string `toml:"component_ttl"`
	// CardTTL bounds rendered status-card HTML in the web UI. Like components,
	// cards are dependency-invalidated; the TTL only bounds count staleness.
	CardTTL string `toml:"card_ttl"`
	// ServeStale serves a generation-stale cached page immediately and rebuilds
	// it in the background, so the first page open after new posts arrive never
	// waits on a rebuild. Content may lag by one refresh cycle.
	ServeStale bool `toml:"serve_stale"`
	// TimelineTTL is the hard validation window for cached timeline membership.
	TimelineTTL string `toml:"timeline_ttl"`
	// TimelineMaxEntries bounds the number of status IDs retained per cached
	// feed window. API requests are currently capped at 40, but a larger default
	// leaves room for future canonical-window caching without a config change.
	TimelineMaxEntries int `toml:"timeline_max_entries"`
	// FederationTTL bounds cached ActivityPub objects and collections.
	FederationTTL string `toml:"federation_ttl"`
	// DiscoveryTTL bounds stable WebFinger, NodeInfo, host-meta and actor discovery.
	DiscoveryTTL string `toml:"discovery_ttl"`
	// NegativeTTL briefly suppresses repeated fetches for definitive missing resources.
	NegativeTTL string `toml:"negative_ttl"`
	// MaxFederationObjectKB prevents a single remote response consuming the cache.
	MaxFederationObjectKB int `toml:"max_federation_object_kb"`
	// WarmTimelines proactively refreshes recently-active viewers' home
	// timeline and notification caches in the background when new statuses or
	// notifications arrive, so the next page open is a cache hit instead of a
	// rebuild. Only effective when Enabled.
	WarmTimelines bool `toml:"warm_timelines"`
	// ActiveViewerTTL is how long after their last request a viewer keeps
	// being refreshed by the warmer.
	ActiveViewerTTL string `toml:"active_viewer_ttl"`
	// WarmMaxViewers caps how many viewers a single warm pass refreshes, which
	// bounds background work on single-core devices.
	WarmMaxViewers int `toml:"warm_max_viewers"`
	// WarmDebounce coalesces bursts of arriving statuses (federation delivers
	// in clumps) into one warm pass.
	WarmDebounce string `toml:"warm_debounce"`
}

// EntityTTLDuration parses EntityTTL (Go duration; empty/"0" = no expiry).
func (c Cache) EntityTTLDuration() (time.Duration, error) {
	return parseDurationWithDays(c.EntityTTL)
}

// PageTTLDuration parses PageTTL (Go duration; empty/"0" = no expiry).
func (c Cache) PageTTLDuration() (time.Duration, error) {
	return parseDurationWithDays(c.PageTTL)
}

// ComponentTTLDuration parses ComponentTTL; empty falls back to PageTTL for
// deployments predating the split.
func (c Cache) ComponentTTLDuration() (time.Duration, error) {
	if c.ComponentTTL == "" {
		return parseDurationWithDays(c.PageTTL)
	}
	return parseDurationWithDays(c.ComponentTTL)
}

// CardTTLDuration parses CardTTL; empty falls back to ComponentTTL.
func (c Cache) CardTTLDuration() (time.Duration, error) {
	if c.CardTTL == "" {
		return c.ComponentTTLDuration()
	}
	return parseDurationWithDays(c.CardTTL)
}

func (c Cache) TimelineTTLDuration() (time.Duration, error) {
	return parseDurationWithDays(c.TimelineTTL)
}

func (c Cache) FederationTTLDuration() (time.Duration, error) {
	return parseDurationWithDays(c.FederationTTL)
}
func (c Cache) DiscoveryTTLDuration() (time.Duration, error) {
	return parseDurationWithDays(c.DiscoveryTTL)
}
func (c Cache) NegativeTTLDuration() (time.Duration, error) {
	return parseDurationWithDays(c.NegativeTTL)
}

// ActiveViewerTTLDuration parses ActiveViewerTTL (Go duration; empty/"0" = no expiry).
func (c Cache) ActiveViewerTTLDuration() (time.Duration, error) {
	return parseDurationWithDays(c.ActiveViewerTTL)
}

// WarmDebounceDuration parses WarmDebounce (Go duration).
func (c Cache) WarmDebounceDuration() (time.Duration, error) {
	return parseDurationWithDays(c.WarmDebounce)
}

// Instance holds instance-level metadata advertised to clients.
type Instance struct {
	BlockSensitiveRelationshipNotifications bool `toml:"block_sensitive_relationship_notifications"`
	// Name is the human-readable instance name shown to clients and in the PWA.
	Name string `toml:"name"`
	// MaxStatusChars is the maximum status length (in characters) advertised to
	// Mastodon clients via the instance API. Defaults to 5000.
	MaxStatusChars int `toml:"max_status_chars"`
	// Rules are public instance rules shown during registration and exposed by
	// both Mastodon-compatible instance APIs.
	Rules []string `toml:"rules"`
	// The description fields below seed the instance profile shown on /about
	// and returned by the instance APIs. An administrator can override every
	// one of them from /admin/instance without restarting, at which point the
	// stored value wins and these become the fallback.
	//
	// ShortDescription is the one-line summary; Description the paragraph.
	ShortDescription string `toml:"short_description"`
	Description      string `toml:"description"`
	// ExtendedDescription and TermsOfService are Markdown, rendered as full
	// sections on /about and /terms.
	ExtendedDescription string `toml:"extended_description"`
	TermsOfService      string `toml:"terms_of_service"`
	// ContactEmail is the moderation contact published to clients. Empty
	// publishes no address.
	ContactEmail string `toml:"contact_email"`
}

// Backup controls periodic database and media backups.
type Backup struct {
	Enabled      bool   `toml:"enabled"`       // enable periodic server-side backups
	Path         string `toml:"path"`          // output directory, defaults to "backups"
	Interval     string `toml:"interval"`      // how often to run backups, e.g. "24h" or "7d"
	IncludeMedia bool   `toml:"include_media"` // include owned media (cache is always excluded)
	Retain       int    `toml:"retain"`        // number of backups to keep (0 = unlimited)
}

// Blog controls the optional per-account static blog site (requires
// littleFedi to be built with -tags blog; a plain build ignores this section
// entirely, since no route or job handler is registered without the tag).
type Blog struct {
	Enabled          bool   `toml:"enabled"`           // opt in per instance; still per-account which posts render
	OutputDir        string `toml:"output_dir"`        // generated site root, defaults to "blogs"
	DirectoryEnabled bool   `toml:"directory_enabled"` // publish /blogs; authors may opt out through Discovery
	// SlugFormat is the permalink template for new posts, e.g.
	// "{year}/{month}/{day}/{title}". Placeholders: {year} {month} {day}
	// {title} {id} {shortid}. Empty falls back to blogslug.DefaultFormat.
	SlugFormat string `toml:"slug_format"`
	// AllowUserSlugFormat lets each account override SlugFormat from its
	// settings page. Set false to impose one permalink style instance-wide;
	// slugs already assigned keep working either way, since a slug is frozen
	// once written.
	AllowUserSlugFormat bool `toml:"allow_user_slug_format"`
	// AllAccounts grants blog publishing to every local account instead of
	// asking an administrator to opt each one in. It is a policy for the whole
	// instance, so while it is on the per-account permission is ignored rather
	// than merged: turn it off and the instance is back to whatever each
	// account was individually granted.
	AllAccounts bool `toml:"all_accounts"`
}

// ResolvedOutputDir returns the blog output directory, defaulting to "blogs".
func (b Blog) ResolvedOutputDir() string {
	if strings.TrimSpace(b.OutputDir) != "" {
		return b.OutputDir
	}
	return "blogs"
}

// AccountAllowed resolves one account's blog permission against the instance
// policy. It answers only "may this account publish", not "can this server
// publish at all": callers keep their own check that the feature is compiled
// in and enabled.
func (b Blog) AccountAllowed(accountAllowed bool) bool {
	return b.AllAccounts || accountAllowed
}

// ResolvedSlugFormat returns the permalink template for accountFormat,
// honouring the per-account override only when this instance allows it.
func (b Blog) ResolvedSlugFormat(accountFormat string) string {
	return blogslug.ResolveFormat(b.SlugFormat, accountFormat, b.AllowUserSlugFormat)
}

// ResolvedPath returns the backup directory, defaulting to "backups".
func (b Backup) ResolvedPath() string {
	if strings.TrimSpace(b.Path) != "" {
		return b.Path
	}
	return "backups"
}

// IntervalDuration parses the Interval string into a time.Duration.
// Supports "d" suffix for days. Defaults to 24h if empty or invalid.
func (b Backup) IntervalDuration() time.Duration {
	s := strings.TrimSpace(b.Interval)
	if s == "" {
		return 24 * time.Hour
	}
	if strings.HasSuffix(s, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil {
			return 24 * time.Hour
		}
		return time.Duration(days * 24 * float64(time.Hour))
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 24 * time.Hour
	}
	return d
}

type DB struct {
	DSN          string `toml:"dsn"` // sqlite: file path; postgres: connection URI
	MaxOpenConns int    `toml:"max_open_conns"`
	MaxIdleConns int    `toml:"max_idle_conns"`
	LogQueries   bool   `toml:"log_queries"`
}

// Streaming selects the live-event broker. "local" is zero-dependency and is
// appropriate for a single process; "postgres" enables cross-process fan-out.
type Streaming struct {
	Backend string `toml:"backend"`
}

// Queue controls the bounded database-backed background worker pool.
type Queue struct {
	Workers              int           `toml:"workers"`
	TerminalJobRetention time.Duration `toml:"terminal_job_retention"`
	StaleRunningTimeout  time.Duration `toml:"stale_running_timeout"`
	MaintenanceInterval  time.Duration `toml:"maintenance_interval"`
	// PollInterval is the workers' base queue-poll cadence. Local enqueues wake
	// a worker immediately, so the poll is only a safety net for retry-scheduled
	// jobs and (in postgres multi-process mode) jobs enqueued by other processes.
	PollInterval time.Duration `toml:"poll_interval"`
	// PollBackoffMax caps the exponential idle backoff of the queue poll.
	// 0 = auto: 5m in single-process mode (local enqueues and retries both
	// self-wake a worker, so the idle poll is pure crash recovery), 60s in
	// postgres multi-process mode (the poll also picks up jobs enqueued by
	// other processes).
	PollBackoffMax time.Duration `toml:"poll_backoff_max"`
	// LightWorkers, when > 0, runs the light, mostly network-bound outbound
	// job kinds (federation delivery, its fan-out expander, push
	// notifications, and email) on a second, dedicated worker pool with its
	// own concurrency instead of the main pool. These jobs are short and
	// often latency-sensitive (a remote instance or a user expects timely
	// delivery), but the main pool's workers block synchronously on the
	// heavier kinds (inbox signature verification/actor fetch/home feed
	// fan-out, media_cache image decode) - priority ordering alone can't help
	// because it only orders queued rows, it can't preempt a job already
	// running. A separate pool gives the light kinds their own goroutine so a
	// blocked main worker never delays them. 0 (default) keeps all four kinds
	// on the main pool, unchanged behavior - this is opt-in so low_power
	// installs (main pool workers=1) are unaffected unless light_workers is
	// set explicitly; the extra idle goroutine(s) cost is negligible even on
	// constrained hardware. Both pools yield to interactive requests and share
	// a single recovery slot, claimed after three seconds of uninterrupted
	// foreground load here and ten on the main pool, so light jobs win it when
	// the two compete. They also share the background limiter sized from
	// DB.MaxOpenConns, so worker counts above the connection count buy nothing:
	// the database is the serial resource, and extra workers only deepen the
	// queue in front of it.
	LightWorkers int `toml:"light_workers"`
}

type Media struct {
	Backend       string `toml:"backend"`         // local or s3 (s3 requires -tags s3)
	CacheBackend  string `toml:"cache_backend"`   // empty inherits Backend
	PublicBaseURL string `toml:"public_base_url"` // optional CDN/public object origin
	Path          string `toml:"path"`            // filesystem path for local media storage
	MaxFileSize   int64  `toml:"max_file_size"`   // in bytes
	MaxMedia      int    `toml:"max_media"`       // maximum attachments allowed on one status
	ProxyRemote   bool   `toml:"proxy_remote"`    // stream remote media through our domain so local users never hit the remote instance (privacy)
	ProxySecret   string `toml:"proxy_secret"`    // HMAC secret for signing proxy URLs; auto-generated per-process if empty
	// CacheRemote selects the remote media caching strategy:
	//   "off"   - never persist remote media (proxy/stream only)
	//   "eager" - proactively fetch, process and store all remote media (like Mastodon)
	//   "lazy"  - store remote media in the background the first time it is accessed via the proxy
	CacheRemote string `toml:"cache_remote"`
	// CacheMaxAge bounds how long cached remote media is kept on disk before being
	// pruned. Accepts Go durations plus a day suffix, e.g. "720h" or "30d".
	// Empty or "0" keeps cached media forever.
	CacheMaxAge string `toml:"cache_max_age"`
	// CacheMaxSizeMB bounds the cache directory, in megabytes. Housekeeping
	// evicts the least recently cached entries until it fits. 0 leaves
	// CacheMaxAge as the only bound, and so does CacheRemote being off, since
	// then there is nothing being cached for a budget to bound.
	//
	// Two passes enforce it. Every housekeeping run sums the cached media
	// attachments, the part that grows without limit because it follows every
	// image in every post that reaches a timeline, and trims those. Once a day
	// the object-store walk totals the directory itself and evicts the oldest of
	// anything, which is the only way cached avatars, headers, custom emoji and
	// link-preview images are seen at all: none of them record a size a query
	// could sum. Between walks the frequent pass leaves room for whatever the
	// last walk found, so media cannot quietly take the whole allowance.
	//
	// Only the cache is counted and only the cache is evicted. Owned uploads sit
	// outside both, which is what makes any positive limit reachable: there is
	// always cache left to give back. Eviction order is the time an entry was
	// written, not last read, because atime is off on the hosts this runs on.
	CacheMaxSizeMB int `toml:"cache_max_size_mb"`
	// CachePath is the directory where cached remote media is stored. Empty
	// defaults to "<path>/cache". It may point at a separate disk/volume; it is
	// always served at the /media/cache/ URL prefix.
	CachePath   string `toml:"cache_path"`
	RenameMedia bool   `toml:"rename_media"` // rename uploaded files to generic names (strips original filename info)
	StripEXIF   bool   `toml:"strip_exif"`   // strip EXIF metadata from uploaded images (preserves orientation)
	// MaxImagePixels and MaxImageDimension bound decoded image memory, which can
	// otherwise be much larger than the compressed upload size. Zero explicitly
	// disables a bound; the default pixel limit also applies to remote images.
	MaxImagePixels    int64 `toml:"max_image_pixels"`
	MaxImageDimension int   `toml:"max_image_dimension"`
	// MaxStoredDimension downscales accepted JPEG/PNG uploads so the longest
	// side is at most this many pixels before they are stored. It keeps the
	// media directory (and every later read: federation fetches, cache warming,
	// backups) small on modest hardware. 0 stores uploads at their original
	// resolution. This is destructive: no full-resolution copy is kept.
	// Animated GIFs, WebP (no encoder) and video/audio are never touched.
	MaxStoredDimension    int `toml:"max_stored_dimension"`
	ProcessingConcurrency int `toml:"processing_concurrency"`
	// ResampleFilter selects the thumbnail resampling filter: "lanczos"
	// (default, best quality), "catmullrom", "linear" or "box" (fastest).
	// The low_power preset switches to "linear".
	ResampleFilter string `toml:"resample_filter"`
	S3             S3     `toml:"s3"`
}

// S3 configures AWS S3 and compatible object stores (SeaweedFS, MinIO, Ceph,
// B2, Wasabi, Spaces). Endpoint and PathStyle are the compatibility switches.
type S3 struct {
	Bucket                string `toml:"bucket"`
	PrivateBucket         string `toml:"private_bucket"`
	Region                string `toml:"region"`
	Endpoint              string `toml:"endpoint"`
	Prefix                string `toml:"prefix"`
	PathStyle             bool   `toml:"path_style"`
	AccessKey             string `toml:"access_key"`
	SecretKey             string `toml:"secret_key"`
	SessionToken          string `toml:"session_token"`
	UseDefaultCredentials bool   `toml:"use_default_credentials"`
	Insecure              bool   `toml:"insecure"`
}

func (m Media) OwnedBackend() string {
	b := strings.ToLower(strings.TrimSpace(m.Backend))
	if b == "" {
		return "local"
	}
	return b
}

func (m Media) ResolvedCacheBackend() string {
	b := strings.ToLower(strings.TrimSpace(m.CacheBackend))
	if b == "" {
		return m.OwnedBackend()
	}
	return b
}

// Media cache modes.
const (
	CacheModeOff   = "off"
	CacheModeEager = "eager"
	CacheModeLazy  = "lazy"
)

// CacheMode returns the normalized remote media cache mode (off/eager/lazy).
func (m Media) CacheMode() string {
	switch strings.ToLower(strings.TrimSpace(m.CacheRemote)) {
	case CacheModeEager:
		return CacheModeEager
	case CacheModeLazy:
		return CacheModeLazy
	default:
		return CacheModeOff
	}
}

// CacheMaxAgeDuration parses CacheMaxAge into a duration. A zero duration means
// "keep forever". Supports a day suffix (e.g. "30d") in addition to Go durations.
func (m Media) CacheMaxAgeDuration() (time.Duration, error) {
	return parseDurationWithDays(m.CacheMaxAge)
}

// CacheMaxSizeBytes converts CacheMaxSizeMB into bytes. Zero means no size
// bound; a negative setting is read the same way rather than as a limit no
// cache can meet.
//
// A budget with caching off is zero too. Nothing is being stored to bound, so
// honouring the number would only buy a recurring sum over media_attachments
// and a walk of an empty directory.
func (m Media) CacheMaxSizeBytes() int64 {
	if m.CacheMaxSizeMB <= 0 || m.CacheMode() == CacheModeOff {
		return 0
	}
	return int64(m.CacheMaxSizeMB) * 1024 * 1024
}

// ResolvedCachePath returns the directory where cached remote media is stored,
// defaulting to "<Path>/cache" when CachePath is unset.
func (m Media) ResolvedCachePath() string {
	if strings.TrimSpace(m.CachePath) != "" {
		return m.CachePath
	}
	return filepath.Join(m.Path, "cache")
}

// parseDurationWithDays parses a duration string that may use a "d" (days)
// suffix in addition to the units understood by time.ParseDuration. An empty
// string or "0" parses to a zero duration.
func parseDurationWithDays(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	if strings.HasSuffix(s, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid day duration %q: %w", s, err)
		}
		return time.Duration(days * 24 * float64(time.Hour)), nil
	}
	return time.ParseDuration(s)
}

type Fed struct {
	SecureMode               bool   `toml:"secure_mode"` // authorized fetch
	UserAgent                string `toml:"user_agent"`
	CommercialBigTechListURL string `toml:"commercial_bigtech_list_url"`
	// SignWithOIP attaches an FEP-8b32 Object Integrity Proof (eddsa-jcs-2022,
	// Ed25519) to outbound activities in addition to the HTTP Signature.
	// Defaults to off so instances can validate OIP verification in the wild
	// before their peers start depending on it.
	SignWithOIP bool `toml:"sign_with_oip"`
	// KeyEncryptionKey seals the ActivityPub private keys of local accounts
	// at rest. Base64 of 32 raw bytes, environment only: a key sitting in the
	// same file the operator backs up next to the database would protect
	// nothing. Losing it makes every sealed key unreadable, so it belongs
	// wherever the TLS material lives, not in the database backup.
	KeyEncryptionKey               string        `toml:"-"`
	ActorRefreshInterval           time.Duration `toml:"actor_refresh_interval"` // how often to refresh remote actors (0 = disabled)
	ActorStaleAge                  time.Duration `toml:"actor_stale_age"`        // successful background refresh age
	ActorRefreshOnDemandStaleAge   time.Duration `toml:"actor_refresh_on_demand_stale_age"`
	ActorRefreshConcurrency        int           `toml:"actor_refresh_concurrency"`
	ActorRefreshBatchSize          int           `toml:"actor_refresh_batch_size"`
	ActorRefreshLease              time.Duration `toml:"actor_refresh_lease"`
	ActorRefreshQueueHighWatermark int           `toml:"actor_refresh_queue_high_watermark"`
	ThreadFetchEnabled             bool          `toml:"thread_fetch_enabled"`
	ThreadFetchStaleAfter          time.Duration `toml:"thread_fetch_stale_after"`
	ThreadFetchTimeout             time.Duration `toml:"thread_fetch_timeout"`
	ThreadFetchMaxReplies          int           `toml:"thread_fetch_max_replies"`
	ThreadFetchMaxAncestorDepth    int           `toml:"thread_fetch_max_ancestor_depth"`
	ThreadFetchMaxPages            int           `toml:"thread_fetch_max_pages"`
	// Background completion crawls the full conversation tree (including
	// cross-instance replies) off the request path.
	ThreadFetchBackgroundEnabled     bool          `toml:"thread_fetch_background_enabled"`
	ThreadFetchBackgroundTimeout     time.Duration `toml:"thread_fetch_background_timeout"`
	ThreadFetchBackgroundMaxReplies  int           `toml:"thread_fetch_background_max_replies"`
	ThreadFetchBackgroundMaxPages    int           `toml:"thread_fetch_background_max_pages"`
	ThreadFetchBackgroundConcurrency int           `toml:"thread_fetch_background_concurrency"`
	// Outbound HTTP connection pooling. Fan-out concentrates on a few big
	// shared inboxes; too few idle connections per host forces repeated TLS
	// handshakes. The low_power preset keeps these small.
	HTTPMaxIdleConns        int `toml:"http_max_idle_conns"`
	HTTPMaxIdleConnsPerHost int `toml:"http_max_idle_conns_per_host"`
}

type RateLimits struct {
	API   int `toml:"api"`   // requests per minute per IP
	Inbox int `toml:"inbox"` // requests per minute per IP (inbox)
}

type HTTP struct {
	Listen   string   `toml:"listen"`    // e.g. ":8080"
	ProxyURI string   `toml:"proxy_uri"` // public URI scheme, for reverse proxy
	Trusted  []string `toml:"trusted_proxies"`
	// Gzip enables on-the-fly compression of dynamic HTML/JSON responses.
	// Unset defaults to on; the low_power preset turns it off (compression
	// trades CPU for bandwidth, the wrong trade on a single slow core).
	// Static assets are pre-compressed at startup and unaffected.
	Gzip *bool `toml:"gzip"`
}

// GzipEnabled reports whether dynamic-response compression is on (default true).
func (h HTTP) GzipEnabled() bool {
	if h.Gzip == nil {
		return true
	}
	return *h.Gzip
}

type Web struct {
	Password      string `toml:"password"`       // global password for the web UI (deprecated; use per-user auth)
	DefaultLocale string `toml:"default_locale"` // fallback UI/email/push language when no account/cookie/Accept-Language match
	// DefaultTheme and DefaultAppearance style the web UI for signed-out
	// visitors and for accounts that never picked their own. Unknown values
	// fall back to the built-in defaults rather than failing startup, so a
	// typo here degrades to littleFedi's own look instead of a broken page.
	DefaultTheme      string `toml:"default_theme"`      // any id from webui.Themes(): "default", "roost", "herd", ...
	DefaultAppearance string `toml:"default_appearance"` // "auto", "light", "dark" or "black"
	// Catchup offers the per-account catch-up view at /catchup: a bounded,
	// explained ranking of what the account's home feed received while it was
	// away. Unset defaults to on; false removes the page and its navigation
	// entry without touching the chronological timelines.
	Catchup *bool `toml:"catchup"`
	// PublicLocalTimeline lets signed-out visitors browse the local timeline.
	// Unset defaults to on; false requires a session, including for pagination.
	PublicLocalTimeline *bool `toml:"public_local_timeline"`
	// PublicBlogDirectory controls guest access to /blogs and its sitemap index.
	// Individual blogs remain public. Unset defaults to on.
	PublicBlogDirectory *bool `toml:"public_blog_directory"`
	// RedirectGuestsToAbout sends signed-out visitors from / to /about,
	// regardless of whether the local timeline is public.
	RedirectGuestsToAbout bool `toml:"redirect_guests_to_about"`
}

// CatchupEnabled reports whether the catch-up view is offered (default true).
func (w Web) CatchupEnabled() bool {
	if w.Catchup == nil {
		return true
	}
	return *w.Catchup
}

// PublicLocalTimelineEnabled reports whether guests may browse the local timeline.
func (w Web) PublicLocalTimelineEnabled() bool {
	if w.PublicLocalTimeline == nil {
		return true
	}
	return *w.PublicLocalTimeline
}

// PublicBlogDirectoryEnabled reports whether guests may browse the blog directory.
func (w Web) PublicBlogDirectoryEnabled() bool {
	if w.PublicBlogDirectory == nil {
		return true
	}
	return *w.PublicBlogDirectory
}

// Auth holds authentication-related configuration.
type Auth struct {
	Registrations      bool   `toml:"registrations"`        // whether open registration is allowed
	InviteRequired     bool   `toml:"invite_required"`      // require invite token to register
	MinPasswordLength  int    `toml:"min_password_length"`  // minimum password length
	RequireApproval    bool   `toml:"require_approval"`     // admin must approve new accounts
	RequireEmail       bool   `toml:"require_email"`        // email is mandatory for registration
	RequireAdminMFA    bool   `toml:"require_admin_mfa"`    // deny administrators without enrolled MFA
	MFAEncryptionKey   string `toml:"-"`                    // base64 AES-256 key; environment only
	EnforceOAuthScopes bool   `toml:"enforce_oauth_scopes"` // deprecated compatibility key; scopes are always enforced
	PasswordPolicy     string `toml:"password_policy"`      // e.g. "min_length:8" (deprecated)
	DevMode            bool   `toml:"dev_mode"`             // enable development shortcuts (e.g. localhost login without password)
}

type Obs struct {
	MetricsEnabled bool   `toml:"metrics_enabled"`
	LogLevel       string `toml:"log_level"` // debug, info, warn, error
	// PprofListen starts Go's net/http/pprof profiler on its own listener
	// (e.g. "127.0.0.1:6060"). Empty disables it. The handler is
	// unauthenticated - bind it to localhost only.
	PprofListen string `toml:"pprof_listen"`
	// AccessLog controls per-request logging. Unset defaults to on. Static
	// assets, proxied media, and health/metrics probes log at debug level
	// regardless, so they stay silent at the default info log level.
	AccessLog *bool `toml:"access_log"`
}

// AccessLogEnabled reports whether per-request logging is on (default true).
func (o Obs) AccessLogEnabled() bool {
	if o.AccessLog == nil {
		return true
	}
	return *o.AccessLog
}

// Housekeeping holds automatic cleanup configuration.
type Housekeeping struct {
	PruneRemoteStatusesAfterDays int    `toml:"prune_remote_statuses_after_days"` // delete remote statuses after this many days (0 = disabled)
	PruneOwnStatusesAfterDays    int    `toml:"prune_own_statuses_after_days"`    // delete own statuses after this many days (0 = disabled)
	PruneOwnStatusesMinLikes     int    `toml:"prune_own_statuses_min_likes"`     // only delete own statuses with fewer than this many likes
	PruneOwnStatusesMinBoosts    int    `toml:"prune_own_statuses_min_boosts"`    // only delete own statuses with fewer than this many boosts
	TombstoneRetentionDays       int    `toml:"tombstone_retention_days"`         // delete tombstones older than this many days (0 = keep forever, default 90)
	Interval                     string `toml:"interval"`                         // how often to run housekeeping (e.g. "1h", "24h"), defaults to "1h"
	MaxDeletePerRun              int    `toml:"max_delete_per_run"`               // max statuses to delete per run (0 = unlimited)
	// HomeFeedRetention caps each local user's materialized home_feed rows.
	// It bounds home/list timeline scrollback depth; keep it >= 100.
	HomeFeedRetention int `toml:"home_feed_retention"`
}

type SMTP struct {
	Host            string `toml:"host"`
	Port            int    `toml:"port"`
	Username        string `toml:"username"`
	Password        string `toml:"password"`
	From            string `toml:"from"`
	DatabaseAlertTo string `toml:"database_alert_to"`
	RequireTLS      bool   `toml:"require_tls"`
	ImplicitTLS     bool   `toml:"implicit_tls"`
	// InsecureSkipVerify permits self-signed SMTP certificates but disables
	// certificate-chain and hostname verification. Keep false outside trusted networks.
	InsecureSkipVerify bool `toml:"insecure_skip_verify"`
}

type Push struct {
	VAPIDPublic  string `toml:"vapid_public_key"`  // VAPID public key (base64-encoded, no padding)
	VAPIDPrivate string `toml:"vapid_private_key"` // VAPID private key (base64-encoded, no padding)
}

// Migration holds account move configuration.
type Migration struct {
	Enabled            bool          `toml:"enabled"`              // whether account moves are allowed
	MaxFollowsImport   int           `toml:"max_follows_import"`   // max follows to import when migrating (0 = unlimited)
	ImportRate         time.Duration `toml:"import_rate"`          // rate limit for importing follows
	CooldownAfterMove  time.Duration `toml:"cooldown_after_move"`  // minimum time before another move is allowed
	RequireTargetAlias bool          `toml:"require_target_alias"` // require alsoKnownAs on the target account
}

// DefaultConfig returns a config with sensible defaults.
func DefaultConfig() *Config {
	// Scale background and database concurrency with available CPUs, but keep
	// both ends bounded. A one-core Pi/zone must not start four DB connections
	// and eight workers merely because low_power was not selected; larger hosts
	// still scale automatically up to the useful limit.
	concurrency := min(8, max(1, runtime.GOMAXPROCS(0)))
	return &Config{
		BackgroundMaintenance: true,
		DB: DB{
			DSN:          "littlefedi.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_pragma=temp_store(MEMORY)",
			MaxOpenConns: concurrency,
			MaxIdleConns: concurrency,
		},
		Streaming: Streaming{Backend: "local"},
		Web:       Web{DefaultLocale: i18n.DefaultLocale, DefaultTheme: "default", DefaultAppearance: "auto"},
		Auth:      Auth{MinPasswordLength: 15},
		Queue: Queue{
			Workers:              concurrency,
			TerminalJobRetention: 24 * time.Hour,
			StaleRunningTimeout:  15 * time.Minute,
			MaintenanceInterval:  1 * time.Hour,
			PollInterval:         2 * time.Second,
			PollBackoffMax:       0, // auto (see Queue.PollBackoffMax)
			LightWorkers:         0, // disabled: delivery/push/email run on the main pool
		},
		Media: Media{
			Backend:               "local",
			Path:                  "media",
			MaxFileSize:           40 * 1024 * 1024, // 40 MB
			MaxMedia:              4,
			ProxyRemote:           true,         // privacy by default: never let local users hit remote instances directly
			CacheRemote:           CacheModeOff, // off | eager | lazy
			CacheMaxAge:           "720h",       // keep cached remote media for 30 days by default
			RenameMedia:           true,
			StripEXIF:             true,
			MaxImagePixels:        16_000_000,
			MaxImageDimension:     0,    // no limit
			MaxStoredDimension:    2560, // downscale stored uploads to 2560px on the longest side
			ProcessingConcurrency: defaultMediaConcurrency(),
			ResampleFilter:        "lanczos",
			S3:                    S3{Region: "us-east-1", UseDefaultCredentials: true},
		},
		Federation: Fed{
			SecureMode:               false,
			CommercialBigTechListURL: "https://littlefedi.org/lists/commercial-bigtech.csv",
			// UserAgent left empty: Load() fills it in once WebDomain is known,
			// unless the file/env already set an explicit override.
			ActorRefreshInterval:           10 * time.Minute,
			ActorStaleAge:                  7 * 24 * time.Hour,
			ActorRefreshOnDemandStaleAge:   24 * time.Hour,
			ActorRefreshConcurrency:        1,
			ActorRefreshBatchSize:          10,
			ActorRefreshLease:              15 * time.Minute,
			ActorRefreshQueueHighWatermark: 100,
			ThreadFetchEnabled:             true,
			ThreadFetchStaleAfter:          15 * time.Minute,
			ThreadFetchTimeout:             3 * time.Second,
			ThreadFetchMaxReplies:          5,
			ThreadFetchMaxAncestorDepth:    20,
			ThreadFetchMaxPages:            1,

			ThreadFetchBackgroundEnabled:     true,
			ThreadFetchBackgroundTimeout:     30 * time.Second,
			ThreadFetchBackgroundMaxReplies:  200,
			ThreadFetchBackgroundMaxPages:    20,
			ThreadFetchBackgroundConcurrency: 1,

			HTTPMaxIdleConns:        64,
			HTTPMaxIdleConnsPerHost: 8,
		},
		RateLimits: RateLimits{
			API:   300,
			Inbox: 100,
		},
		HTTP: HTTP{
			Listen: ":8080",
		},
		SMTP: SMTP{
			Port:       587,
			RequireTLS: true,
		},
		Housekeeping: Housekeeping{
			PruneRemoteStatusesAfterDays: 30,
			PruneOwnStatusesAfterDays:    0,
			PruneOwnStatusesMinLikes:     0,
			PruneOwnStatusesMinBoosts:    0,
			TombstoneRetentionDays:       90,
			Interval:                     "1h",
			MaxDeletePerRun:              500,
			HomeFeedRetention:            800,
		},
		Observability: Obs{
			LogLevel: "info",
		},
		Migration: Migration{
			Enabled:            true,
			MaxFollowsImport:   5000,
			ImportRate:         1 * time.Second,
			CooldownAfterMove:  30 * 24 * time.Hour,
			RequireTargetAlias: false,
		},
		Instance: Instance{
			Name:           "littleFedi",
			MaxStatusChars: 5000,
		},
		Backup: Backup{
			Path:     "backups",
			Interval: "24h",
			Retain:   7,
		},
		Mesh: Mesh{
			MaxCircuits:     64,
			DialTimeout:     30 * time.Second,
			RecordTTL:       24 * time.Hour,
			PublishInterval: 12 * time.Hour,
		},
		Lighthouse: Lighthouse{
			// No default: a non-empty listen address is how a
			// config says "this file runs a relay", which is what lets a
			// relay config omit the ActivityPub domain it has no use for.
			// The relay itself falls back to the standard port.
			MaxCircuitsPerNode: 64,
		},
		Directory: MeshDirectory{
			MaxRecords: 100_000,
		},
		Blog: Blog{
			SlugFormat:       blogslug.DefaultFormat,
			DirectoryEnabled: true,
			// Permalink style is an authoring preference, not a policy an
			// admin normally needs to own, so users may override it unless
			// the admin explicitly takes that away.
			AllowUserSlugFormat: true,
		},
		Cache: Cache{
			Enabled:               true,
			MaxMemoryMB:           32,
			EntityTTL:             "1h",
			PageTTL:               "5m",
			ComponentTTL:          "1h",
			CardTTL:               "1h",
			ServeStale:            true,
			TimelineTTL:           "2h",
			TimelineMaxEntries:    200,
			FederationTTL:         "10m",
			DiscoveryTTL:          "1h",
			NegativeTTL:           "30s",
			MaxFederationObjectKB: 512,
			WarmTimelines:         true,
			ActiveViewerTTL:       "15m",
			WarmMaxViewers:        concurrency,
			WarmDebounce:          "2s",
		},
	}
}

// IsMultiUser returns true if the instance is configured for multi-user mode.
// Single-user mode means: single_user is set, no registrations, no admins, no invite_required.
func (c *Config) IsMultiUser() bool {
	return c.Auth.Registrations || len(c.Admins) > 0 || c.Auth.InviteRequired || c.Auth.RequireApproval
}

// lowPowerRequested peeks the low_power flag before the full config load, so
// the preset can be applied as a defaults layer that the file and env still
// override. The environment decides even over the file (matching the loader's
// env-wins precedence); a missing or unreadable file simply means false.
func lowPowerRequested(path string) bool {
	if v := os.Getenv("LITTLEFEDI_LOW_POWER"); v != "" {
		return v == "true" || v == "1"
	}
	if path == "" {
		return false
	}
	var peek struct {
		LowPower bool `toml:"low_power"`
	}
	if _, err := toml.DecodeFile(path, &peek); err != nil {
		return false
	}
	return peek.LowPower
}

// defaultMediaConcurrency scales parallel image processing with the hardware:
// half the cores, capped at 4 (image decode is the RAM spike, not just CPU).
// The low_power preset forces it back to 1.
func defaultMediaConcurrency() int {
	return min(4, max(1, runtime.GOMAXPROCS(0)/2))
}

// applyLowPowerDefaults replaces the standard defaults with values tuned for
// Pi-Zero-class hardware: single core, <=512 MB RAM, SD-card storage. It runs
// before the config file is decoded, so it must only assign field values -
// no I/O, no validation, no reading user input.
//
// Not touched: RateLimits.Inbox (dropped inbound federation
// triggers remote redelivery storms - more total work than it saves),
// Media.ProcessingConcurrency (already 1), the warmer page size (must match
// what the front ends request or warming always misses).
func applyLowPowerDefaults(cfg *Config) {
	// ~4 MB SQLite page cache per connection instead of the driver default.
	// A user-provided DSN replaces this string wholesale.
	cfg.DB.DSN += "&_pragma=cache_size(-4000)"
	// A single connection avoids multiplying SQLite page caches on tiny hosts.
	cfg.DB.MaxOpenConns = 1
	cfg.DB.MaxIdleConns = 1

	// Single core: one background worker. The enqueue wake channel keeps job
	// latency near zero, so the safety-net poll can be lazier.
	cfg.Queue.Workers = 1
	cfg.Queue.PollInterval = 5 * time.Second

	// The cache stack exists for this hardware; cap it below the 256 MiB
	// process memory limit set in main.go, and bound background warm work.
	cfg.Cache.Enabled = true
	cfg.Cache.MaxMemoryMB = 24
	cfg.Cache.WarmMaxViewers = 1

	// Each mesh circuit holds a TLS session with its own buffers, and the
	// device this preset targets federates with a handful of peers at a time.
	cfg.Mesh.MaxCircuits = 16

	// Image decoding is the RAM spike that OOMs small devices, but 8 MP
	// rejected ordinary 12 MP phone photos. "12 MP" sensors are 4032x3024 =
	// 12,192,768 pixels, so the bound has to sit above the round number:
	// 13 M covers them with headroom and still rejects 48 MP shots. Such a
	// photo peaks around 72 MB decoded (YCbCr source + NRGBA copy,
	// ~5.5 bytes/pixel), which fits the 256 MiB limit main.go sets, with
	// concurrency pinned to 1.
	// MaxImageDimension is left unlimited: pixels is what bounds memory, and a
	// per-side cap only rejected wide panoramas that are cheap to decode.
	cfg.Media.MaxImagePixels = 13_000_000
	// Stored uploads get a tighter cap than the 2560 default: a Pi's disk is an
	// SD card and every later read of the file is cheaper for it.
	cfg.Media.MaxStoredDimension = 1600
	// One image at a time: parallel decodes multiply the RAM spike.
	cfg.Media.ProcessingConcurrency = 1
	// Lanczos resampling dominates thumbnail CPU on a single slow core;
	// linear is visually equivalent at 600px previews for a fraction of it.
	cfg.Media.ResampleFilter = "linear"

	// Shorter federation fetch budgets: less synchronous work in the request
	// path, shorter background crawls, smaller refresh bursts.
	cfg.Federation.ThreadFetchTimeout = 2 * time.Second
	cfg.Federation.ThreadFetchMaxReplies = 3
	cfg.Federation.ThreadFetchBackgroundMaxReplies = 100
	cfg.Federation.ThreadFetchBackgroundMaxPages = 10
	cfg.Federation.ActorRefreshBatchSize = 5
	cfg.Federation.ActorRefreshQueueHighWatermark = 50
	// Idle TLS sessions cost RAM; a Pi's fan-out is small enough that a lean
	// pool still avoids most re-handshakes.
	cfg.Federation.HTTPMaxIdleConns = 20
	cfg.Federation.HTTPMaxIdleConnsPerHost = 4

	// Protects the CPU from a hammering client; generous for human use.
	cfg.RateLimits.API = 120

	// Shorter write bursts on SD storage; half the biggest table's per-user
	// rows (this shortens home/list scrollback to ~400 posts).
	cfg.Housekeeping.MaxDeletePerRun = 200
	cfg.Housekeeping.HomeFeedRetention = 400

	// Compression trades CPU for bandwidth - the wrong trade on one slow core.
	gzipOff := false
	cfg.HTTP.Gzip = &gzipOff
}

// Load loads configuration from a TOML file with env var overrides.
// Env vars take the form LITTLEFEDI_{SECTION}_{KEY}
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	// The low_power preset is a second defaults layer applied before the file
	// decode and env overrides, so precedence is exactly:
	//   defaults < low_power preset < config file < environment.
	lowPower := lowPowerRequested(path)
	if lowPower {
		applyLowPowerDefaults(cfg)
	}

	// Try to load from file if it exists
	if path != "" {
		if _, err := toml.DecodeFile(path, cfg); err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("config: parsing %s: %w", path, err)
			}
		}
	}

	// Apply environment variable overrides
	applyEnvOverrides(cfg)
	// Scope checks are a security boundary, not an optional compatibility mode.
	// Keep accepting the old setting so existing config files still parse.
	cfg.Auth.EnforceOAuthScopes = true

	// LowPower reflects whether the preset was actually applied (the peek
	// already gave env precedence over the file), so a file's low_power value
	// decoded above cannot desynchronize the flag from the applied defaults.
	cfg.LowPower = lowPower

	// Resolve the legacy single-domain setting. LocalDomain is the canonical
	// identity setting inside the process; Domain mirrors it for older callers.
	if cfg.LocalDomain == "" {
		cfg.LocalDomain = cfg.Domain
	}
	if cfg.WebDomain == "" {
		cfg.WebDomain = cfg.LocalDomain
	}
	cfg.LocalDomain = normalizeDomain(cfg.LocalDomain)
	cfg.WebDomain = normalizeDomain(cfg.WebDomain)
	cfg.Domain = cfg.LocalDomain
	if cfg.Federation.UserAgent == "" && cfg.WebDomain != "" {
		cfg.Federation.UserAgent = DefaultUserAgent(cfg.WebDomain)
	}
	cfg.Instance.Name = strings.TrimSpace(cfg.Instance.Name)
	if cfg.Instance.Name == "" {
		cfg.Instance.Name = "littleFedi"
	}

	if err := cfg.validateMesh(); err != nil {
		return nil, err
	}

	// Validate required fields. Two configurations legitimately have no
	// domain: a mesh node, whose domain is derived at startup from a key that
	// does not exist until first run, and a lighthouse, which relays bytes and
	// serves no ActivityPub identity of its own.
	if cfg.LocalDomain == "" && !cfg.Mesh.Enabled && cfg.Lighthouse.Listen == "" && cfg.Directory.Listen == "" {
		return nil, fmt.Errorf("config: local_domain is required (set local_domain, domain, LITTLEFEDI_LOCAL_DOMAIN, or LITTLEFEDI_DOMAIN); " +
			"a littleMesh node sets [mesh] enabled, a lighthouse sets [lighthouse] listen, and a directory sets [directory] listen")
	}
	if cfg.LocalDomain != "" {
		if err := validateDomain("local_domain", cfg.LocalDomain); err != nil {
			return nil, err
		}
		if err := validateDomain("web_domain", cfg.WebDomain); err != nil {
			return nil, err
		}
	}
	if cfg.Auth.DevMode && !loopbackListenAddress(cfg.HTTP.Listen) {
		return nil, fmt.Errorf("config: auth.dev_mode requires http.listen to use localhost or a loopback IP")
	}
	if cfg.Observability.PprofListen != "" && !loopbackListenAddress(cfg.Observability.PprofListen) {
		return nil, fmt.Errorf("config: observability.pprof_listen must use localhost or a loopback IP")
	}
	if cfg.Lighthouse.GatewayListen != "" && !loopbackListenAddress(cfg.Lighthouse.GatewayListen) {
		return nil, fmt.Errorf("config: lighthouse.gateway_listen must use localhost or a loopback IP")
	}
	if (cfg.Auth.MinPasswordLength != 0 && cfg.Auth.MinPasswordLength < 15) || cfg.Auth.MinPasswordLength > 72 {
		return nil, fmt.Errorf("config: auth.min_password_length must be 0 (the secure default) or between 15 and 72")
	}
	if cfg.Auth.MFAEncryptionKey != "" {
		key, err := base64.StdEncoding.DecodeString(cfg.Auth.MFAEncryptionKey)
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("config: LITTLEFEDI_AUTH_MFA_ENCRYPTION_KEY must be a base64-encoded 32-byte key")
		}
	} else if cfg.Auth.RequireAdminMFA {
		return nil, fmt.Errorf("config: auth.require_admin_mfa requires LITTLEFEDI_AUTH_MFA_ENCRYPTION_KEY")
	}
	if cfg.Federation.KeyEncryptionKey != "" {
		if _, err := keycrypt.NewAEAD(cfg.Federation.KeyEncryptionKey); err != nil {
			return nil, fmt.Errorf("config: LITTLEFEDI_FEDERATION_KEY_ENCRYPTION_KEY must be a base64-encoded 32-byte key")
		}
	}
	// A broken permalink template must fail here, not silently produce
	// unusable URLs for every post published afterwards.
	cfg.Blog.SlugFormat = strings.TrimSpace(cfg.Blog.SlugFormat)
	if cfg.Blog.SlugFormat == "" {
		cfg.Blog.SlugFormat = blogslug.DefaultFormat
	}
	if err := blogslug.Validate(cfg.Blog.SlugFormat); err != nil {
		return nil, fmt.Errorf("config: blog.slug_format: %w", err)
	}
	if cfg.SMTP.Host != "" && (cfg.SMTP.Port < 1 || cfg.SMTP.Port > 65535) {
		return nil, fmt.Errorf("config: smtp.port must be between 1 and 65535")
	}
	for _, address := range envList(cfg.SMTP.DatabaseAlertTo) {
		if _, err := mail.ParseAddress(address); err != nil || strings.ContainsAny(address, "\r\n") {
			return nil, fmt.Errorf("config: smtp.database_alert_to contains an invalid address")
		}
	}
	cfg.Streaming.Backend = strings.ToLower(strings.TrimSpace(cfg.Streaming.Backend))
	switch cfg.Streaming.Backend {
	case "", "local":
		cfg.Streaming.Backend = "local"
	case "postgres":
	default:
		return nil, fmt.Errorf("config: streaming.backend must be local or postgres (got %q)", cfg.Streaming.Backend)
	}

	// Validate media cache settings.
	for name, backend := range map[string]string{"media.backend": cfg.Media.OwnedBackend(), "media.cache_backend": cfg.Media.ResolvedCacheBackend()} {
		if backend != "local" && backend != "s3" {
			return nil, fmt.Errorf("config: %s must be local or s3 (got %q)", name, backend)
		}
	}
	if (cfg.Media.OwnedBackend() == "s3" || cfg.Media.ResolvedCacheBackend() == "s3") && cfg.Media.S3.Bucket == "" {
		return nil, fmt.Errorf("config: media.s3.bucket is required when an S3 media backend is selected")
	}
	if cfg.Media.S3.PrivateBucket != "" && cfg.Media.S3.PrivateBucket == cfg.Media.S3.Bucket {
		return nil, fmt.Errorf("config: media.s3.private_bucket must differ from the public media bucket")
	}
	if cfg.Media.OwnedBackend() == "s3" && cfg.Backup.IncludeMedia {
		return nil, fmt.Errorf("config: backup.include_media is only supported with local owned media; use S3 provider backups")
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Media.CacheRemote)) {
	case "", CacheModeOff, CacheModeEager, CacheModeLazy:
	default:
		return nil, fmt.Errorf("config: media.cache_remote must be one of off, eager, lazy (got %q)", cfg.Media.CacheRemote)
	}
	if _, err := cfg.Media.CacheMaxAgeDuration(); err != nil {
		return nil, fmt.Errorf("config: media.cache_max_age: %w", err)
	}
	if cfg.Media.MaxMedia <= 0 || cfg.Media.ProcessingConcurrency <= 0 {
		return nil, fmt.Errorf("config: media max_media and processing_concurrency must be positive")
	}
	// 0 means "no limit" for both image bounds; only a negative value is a typo.
	if cfg.Media.MaxImagePixels < 0 || cfg.Media.MaxImageDimension < 0 {
		return nil, fmt.Errorf("config: media max_image_pixels and max_image_dimension must be 0 (no limit) or positive")
	}
	// 0 means "store the original"; anything below a thumbnail is a typo.
	if cfg.Media.MaxStoredDimension < 0 || (cfg.Media.MaxStoredDimension > 0 && cfg.Media.MaxStoredDimension < 320) {
		return nil, fmt.Errorf("config: media max_stored_dimension must be 0 (keep original) or at least 320")
	}
	if cfg.Queue.Workers <= 0 || cfg.Queue.TerminalJobRetention < 0 || cfg.Queue.StaleRunningTimeout <= 0 || cfg.Queue.MaintenanceInterval <= 0 {
		return nil, fmt.Errorf("config: queue workers/timeouts must be positive (retention may be zero)")
	}
	if cfg.Queue.PollInterval <= 0 || (cfg.Queue.PollBackoffMax != 0 && cfg.Queue.PollBackoffMax < cfg.Queue.PollInterval) {
		return nil, fmt.Errorf("config: queue poll_interval must be positive and poll_backoff_max >= poll_interval (or 0 for auto)")
	}
	if cfg.Queue.LightWorkers < 0 {
		return nil, fmt.Errorf("config: queue light_workers must be >= 0 (0 disables the dedicated light-jobs pool)")
	}
	if cfg.Housekeeping.HomeFeedRetention != 0 && cfg.Housekeeping.HomeFeedRetention < 100 {
		return nil, fmt.Errorf("config: housekeeping home_feed_retention must be >= 100 (it bounds home/list scrollback)")
	}
	if cfg.Federation.ActorRefreshConcurrency <= 0 || cfg.Federation.ThreadFetchBackgroundConcurrency <= 0 {
		return nil, fmt.Errorf("config: federation concurrency limits must be positive")
	}
	if cfg.Federation.ActorRefreshBatchSize <= 0 || cfg.Federation.ActorRefreshLease <= 0 || cfg.Federation.ActorRefreshQueueHighWatermark <= 0 {
		return nil, fmt.Errorf("config: actor refresh batch size, lease, and queue high watermark must be positive")
	}
	if cfg.Federation.ActorRefreshOnDemandStaleAge <= 0 {
		return nil, fmt.Errorf("config: actor refresh on-demand stale age must be positive")
	}
	listURL, err := url.Parse(cfg.Federation.CommercialBigTechListURL)
	if err != nil || listURL.Scheme != "https" || listURL.Hostname() == "" || listURL.User != nil {
		return nil, fmt.Errorf("config: federation.commercial_bigtech_list_url must be an HTTPS URL without credentials")
	}
	if cfg.Cache.Enabled {
		if cfg.Cache.MaxMemoryMB <= 0 {
			return nil, fmt.Errorf("config: cache.max_memory_mb must be positive when cache is enabled")
		}
		if _, err := cfg.Cache.EntityTTLDuration(); err != nil {
			return nil, fmt.Errorf("config: cache.entity_ttl: %w", err)
		}
		if _, err := cfg.Cache.PageTTLDuration(); err != nil {
			return nil, fmt.Errorf("config: cache.page_ttl: %w", err)
		}
		if _, err := cfg.Cache.ComponentTTLDuration(); err != nil {
			return nil, fmt.Errorf("config: cache.component_ttl: %w", err)
		}
		if _, err := cfg.Cache.CardTTLDuration(); err != nil {
			return nil, fmt.Errorf("config: cache.card_ttl: %w", err)
		}
		if _, err := cfg.Cache.TimelineTTLDuration(); err != nil {
			return nil, fmt.Errorf("config: cache.timeline_ttl: %w", err)
		}
		if cfg.Cache.TimelineMaxEntries <= 0 {
			return nil, fmt.Errorf("config: cache.timeline_max_entries must be positive when cache is enabled")
		}
		for name, value := range map[string]string{
			"federation_ttl": cfg.Cache.FederationTTL,
			"discovery_ttl":  cfg.Cache.DiscoveryTTL,
			"negative_ttl":   cfg.Cache.NegativeTTL,
		} {
			d, err := parseDurationWithDays(value)
			if err != nil || d <= 0 {
				if err != nil {
					return nil, fmt.Errorf("config: cache.%s: %w", name, err)
				}
				return nil, fmt.Errorf("config: cache.%s must be positive", name)
			}
		}
		if cfg.Cache.MaxFederationObjectKB <= 0 {
			return nil, fmt.Errorf("config: cache.max_federation_object_kb must be positive when cache is enabled")
		}
		if cfg.Cache.WarmTimelines {
			if _, err := cfg.Cache.ActiveViewerTTLDuration(); err != nil {
				return nil, fmt.Errorf("config: cache.active_viewer_ttl: %w", err)
			}
			d, err := cfg.Cache.WarmDebounceDuration()
			if err != nil {
				return nil, fmt.Errorf("config: cache.warm_debounce: %w", err)
			}
			if d <= 0 {
				return nil, fmt.Errorf("config: cache.warm_debounce must be positive when warming is enabled")
			}
			if cfg.Cache.WarmMaxViewers <= 0 {
				return nil, fmt.Errorf("config: cache.warm_max_viewers must be positive when warming is enabled")
			}
		}
	}

	return cfg, nil
}

// applyEnvOverrides applies environment variable overrides to the config.
// Format: LITTLEFEDI_{SECTION}_{KEY} or LITTLEFEDI_{KEY} for top-level.
func applyEnvOverrides(cfg *Config) {
	// Top-level
	if v := os.Getenv("LITTLEFEDI_DOMAIN"); v != "" {
		cfg.Domain = v
		cfg.LocalDomain = v
	}
	if v := os.Getenv("LITTLEFEDI_LOCAL_DOMAIN"); v != "" {
		cfg.LocalDomain = v
		cfg.Domain = v
	}
	if v := os.Getenv("LITTLEFEDI_WEB_DOMAIN"); v != "" {
		cfg.WebDomain = v
	}
	if v := os.Getenv("LITTLEFEDI_SINGLE_USER"); v != "" {
		cfg.SingleUser = v
	}
	if v := os.Getenv("LITTLEFEDI_SELF_DESTRUCT"); v != "" {
		cfg.SelfDestruct = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_BACKGROUND_MAINTENANCE"); v != "" {
		cfg.BackgroundMaintenance = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_WEB_PASSWORD"); v != "" {
		cfg.Web.Password = v
	}
	if v := os.Getenv("LITTLEFEDI_WEB_DEFAULT_LOCALE"); v != "" {
		cfg.Web.DefaultLocale = v
	}
	if v := os.Getenv("LITTLEFEDI_WEB_DEFAULT_THEME"); v != "" {
		cfg.Web.DefaultTheme = v
	}
	if v := os.Getenv("LITTLEFEDI_WEB_DEFAULT_APPEARANCE"); v != "" {
		cfg.Web.DefaultAppearance = v
	}

	// DB
	if v := os.Getenv("LITTLEFEDI_DB_DSN"); v != "" {
		cfg.DB.DSN = v
	}
	if v := os.Getenv("LITTLEFEDI_DB_MAX_OPEN_CONNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.DB.MaxOpenConns = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_STREAMING_BACKEND"); v != "" {
		cfg.Streaming.Backend = strings.ToLower(strings.TrimSpace(v))
	}

	// Queue
	if v := os.Getenv("LITTLEFEDI_QUEUE_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Queue.Workers = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_QUEUE_TERMINAL_JOB_RETENTION"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Queue.TerminalJobRetention = d
		}
	}
	if v := os.Getenv("LITTLEFEDI_QUEUE_STALE_RUNNING_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Queue.StaleRunningTimeout = d
		}
	}
	if v := os.Getenv("LITTLEFEDI_QUEUE_MAINTENANCE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Queue.MaintenanceInterval = d
		}
	}
	if v := os.Getenv("LITTLEFEDI_QUEUE_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Queue.PollInterval = d
		}
	}
	if v := os.Getenv("LITTLEFEDI_QUEUE_LIGHT_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Queue.LightWorkers = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_QUEUE_POLL_BACKOFF_MAX"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Queue.PollBackoffMax = d
		}
	}

	// HTTP
	if v := os.Getenv("LITTLEFEDI_HTTP_LISTEN"); v != "" {
		cfg.HTTP.Listen = v
	}
	if v := os.Getenv("LITTLEFEDI_HTTP_GZIP"); v != "" {
		enabled := v == "true" || v == "1"
		cfg.HTTP.Gzip = &enabled
	}

	// Observability
	if v := os.Getenv("LITTLEFEDI_OBSERVABILITY_LOG_LEVEL"); v != "" {
		cfg.Observability.LogLevel = v
	}
	if v := os.Getenv("LITTLEFEDI_OBSERVABILITY_PPROF_LISTEN"); v != "" {
		cfg.Observability.PprofListen = v
	}
	if v := os.Getenv("LITTLEFEDI_OBSERVABILITY_ACCESS_LOG"); v != "" {
		enabled := v == "true" || v == "1"
		cfg.Observability.AccessLog = &enabled
	}
	if v := os.Getenv("LITTLEFEDI_OBSERVABILITY_METRICS_ENABLED"); v != "" {
		cfg.Observability.MetricsEnabled = v == "true" || v == "1"
	}

	// Media
	if v := os.Getenv("LITTLEFEDI_MEDIA_BACKEND"); v != "" {
		cfg.Media.Backend = v
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_CACHE_BACKEND"); v != "" {
		cfg.Media.CacheBackend = v
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_PUBLIC_BASE_URL"); v != "" {
		cfg.Media.PublicBaseURL = v
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_PATH"); v != "" {
		cfg.Media.Path = v
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_MAX_FILE_SIZE"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Media.MaxFileSize = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_MAX_MEDIA"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Media.MaxMedia = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_PROXY_REMOTE"); v != "" {
		cfg.Media.ProxyRemote = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_PROXY_SECRET"); v != "" {
		cfg.Media.ProxySecret = v
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_CACHE_REMOTE"); v != "" {
		cfg.Media.CacheRemote = v
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_CACHE_MAX_AGE"); v != "" {
		cfg.Media.CacheMaxAge = v
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_CACHE_PATH"); v != "" {
		cfg.Media.CachePath = v
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_RENAME_MEDIA"); v != "" {
		cfg.Media.RenameMedia = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_STRIP_EXIF"); v != "" {
		cfg.Media.StripEXIF = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_MAX_IMAGE_PIXELS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Media.MaxImagePixels = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_MAX_IMAGE_DIMENSION"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Media.MaxImageDimension = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_MAX_STORED_DIMENSION"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Media.MaxStoredDimension = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_PROCESSING_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Media.ProcessingConcurrency = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_RESAMPLE_FILTER"); v != "" {
		cfg.Media.ResampleFilter = v
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_S3_BUCKET"); v != "" {
		cfg.Media.S3.Bucket = v
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_S3_PRIVATE_BUCKET"); v != "" {
		cfg.Media.S3.PrivateBucket = v
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_S3_REGION"); v != "" {
		cfg.Media.S3.Region = v
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_S3_ENDPOINT"); v != "" {
		cfg.Media.S3.Endpoint = v
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_S3_PREFIX"); v != "" {
		cfg.Media.S3.Prefix = v
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_S3_PATH_STYLE"); v != "" {
		cfg.Media.S3.PathStyle = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_S3_ACCESS_KEY"); v != "" {
		cfg.Media.S3.AccessKey = v
		cfg.Media.S3.UseDefaultCredentials = false
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_S3_SECRET_KEY"); v != "" {
		cfg.Media.S3.SecretKey = v
		cfg.Media.S3.UseDefaultCredentials = false
	}
	if v := os.Getenv("LITTLEFEDI_MEDIA_S3_SESSION_TOKEN"); v != "" {
		cfg.Media.S3.SessionToken = v
	}

	// Instance
	if v := os.Getenv("LITTLEFEDI_INSTANCE_NAME"); v != "" {
		cfg.Instance.Name = v
	}
	if v := os.Getenv("LITTLEFEDI_INSTANCE_MAX_STATUS_CHARS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Instance.MaxStatusChars = n
		}
	}

	// Auth
	if v := os.Getenv("LITTLEFEDI_AUTH_REGISTRATIONS"); v != "" {
		cfg.Auth.Registrations = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_AUTH_INVITE_REQUIRED"); v != "" {
		cfg.Auth.InviteRequired = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_AUTH_MIN_PASSWORD_LENGTH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Auth.MinPasswordLength = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_AUTH_REQUIRE_APPROVAL"); v != "" {
		cfg.Auth.RequireApproval = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_AUTH_REQUIRE_EMAIL"); v != "" {
		cfg.Auth.RequireEmail = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_AUTH_REQUIRE_ADMIN_MFA"); v != "" {
		cfg.Auth.RequireAdminMFA = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_AUTH_MFA_ENCRYPTION_KEY"); v != "" {
		cfg.Auth.MFAEncryptionKey = v
	}
	if v := os.Getenv("LITTLEFEDI_FEDERATION_KEY_ENCRYPTION_KEY"); v != "" {
		cfg.Federation.KeyEncryptionKey = v
	}
	if v := os.Getenv("LITTLEFEDI_AUTH_ENFORCE_OAUTH_SCOPES"); v != "" {
		cfg.Auth.EnforceOAuthScopes = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_AUTH_DEV_MODE"); v != "" {
		cfg.Auth.DevMode = v == "true" || v == "1"
	}

	// Federation
	if v := os.Getenv("LITTLEFEDI_FED_SECURE_MODE"); v != "" {
		cfg.Federation.SecureMode = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_FED_COMMERCIAL_BIGTECH_LIST_URL"); v != "" {
		cfg.Federation.CommercialBigTechListURL = v
	}
	if v := os.Getenv("LITTLEFEDI_FED_ACTOR_REFRESH_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Federation.ActorRefreshInterval = d
		}
	}
	if v := os.Getenv("LITTLEFEDI_FED_ACTOR_STALE_AGE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Federation.ActorStaleAge = d
		}
	}
	if v := os.Getenv("LITTLEFEDI_FED_ACTOR_REFRESH_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Federation.ActorRefreshConcurrency = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_FED_ACTOR_REFRESH_BATCH_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Federation.ActorRefreshBatchSize = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_FED_ACTOR_REFRESH_ON_DEMAND_STALE_AGE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Federation.ActorRefreshOnDemandStaleAge = d
		}
	}
	if v := os.Getenv("LITTLEFEDI_FED_ACTOR_REFRESH_LEASE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Federation.ActorRefreshLease = d
		}
	}
	if v := os.Getenv("LITTLEFEDI_FED_ACTOR_REFRESH_QUEUE_HIGH_WATERMARK"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Federation.ActorRefreshQueueHighWatermark = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_FED_THREAD_FETCH_ENABLED"); v != "" {
		cfg.Federation.ThreadFetchEnabled = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_FED_THREAD_FETCH_STALE_AFTER"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Federation.ThreadFetchStaleAfter = d
		}
	}
	if v := os.Getenv("LITTLEFEDI_FED_THREAD_FETCH_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Federation.ThreadFetchTimeout = d
		}
	}
	if v := os.Getenv("LITTLEFEDI_FED_THREAD_FETCH_MAX_REPLIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Federation.ThreadFetchMaxReplies = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_FED_THREAD_FETCH_MAX_ANCESTOR_DEPTH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Federation.ThreadFetchMaxAncestorDepth = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_FED_THREAD_FETCH_MAX_PAGES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Federation.ThreadFetchMaxPages = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_FED_THREAD_FETCH_BACKGROUND_ENABLED"); v != "" {
		cfg.Federation.ThreadFetchBackgroundEnabled = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_FED_THREAD_FETCH_BACKGROUND_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Federation.ThreadFetchBackgroundTimeout = d
		}
	}
	if v := os.Getenv("LITTLEFEDI_FED_THREAD_FETCH_BACKGROUND_MAX_REPLIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Federation.ThreadFetchBackgroundMaxReplies = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_FED_THREAD_FETCH_BACKGROUND_MAX_PAGES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Federation.ThreadFetchBackgroundMaxPages = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_FED_THREAD_FETCH_BACKGROUND_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Federation.ThreadFetchBackgroundConcurrency = n
		}
	}

	// SMTP
	if v := os.Getenv("LITTLEFEDI_SMTP_HOST"); v != "" {
		cfg.SMTP.Host = v
	}
	if v := os.Getenv("LITTLEFEDI_SMTP_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.SMTP.Port = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_SMTP_USERNAME"); v != "" {
		cfg.SMTP.Username = v
	}
	if v := os.Getenv("LITTLEFEDI_SMTP_PASSWORD"); v != "" {
		cfg.SMTP.Password = v
	}
	if v := os.Getenv("LITTLEFEDI_SMTP_FROM"); v != "" {
		cfg.SMTP.From = v
	}
	if v := os.Getenv("LITTLEFEDI_SMTP_DATABASE_ALERT_TO"); v != "" {
		cfg.SMTP.DatabaseAlertTo = v
	}
	if v := os.Getenv("LITTLEFEDI_SMTP_REQUIRE_TLS"); v != "" {
		cfg.SMTP.RequireTLS = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_SMTP_IMPLICIT_TLS"); v != "" {
		cfg.SMTP.ImplicitTLS = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_SMTP_INSECURE_SKIP_VERIFY"); v != "" {
		cfg.SMTP.InsecureSkipVerify = v == "true" || v == "1"
	}

	// Push / VAPID
	if v := os.Getenv("LITTLEFEDI_PUSH_VAPID_PUBLIC"); v != "" {
		cfg.Push.VAPIDPublic = v
	}
	if v := os.Getenv("LITTLEFEDI_PUSH_VAPID_PRIVATE"); v != "" {
		cfg.Push.VAPIDPrivate = v
	}

	// Housekeeping
	if v := os.Getenv("LITTLEFEDI_HOUSEKEEPING_PRUNE_REMOTE_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Housekeeping.PruneRemoteStatusesAfterDays = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_HOUSEKEEPING_PRUNE_OWN_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Housekeeping.PruneOwnStatusesAfterDays = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_HOUSEKEEPING_HOME_FEED_RETENTION"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Housekeeping.HomeFeedRetention = n
		}
	}

	// Backup
	if v := os.Getenv("LITTLEFEDI_BACKUP_ENABLED"); v != "" {
		cfg.Backup.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_BACKUP_PATH"); v != "" {
		cfg.Backup.Path = v
	}
	if v := os.Getenv("LITTLEFEDI_BACKUP_INTERVAL"); v != "" {
		cfg.Backup.Interval = v
	}
	if v := os.Getenv("LITTLEFEDI_BACKUP_INCLUDE_MEDIA"); v != "" {
		cfg.Backup.IncludeMedia = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_BACKUP_RETAIN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Backup.Retain = n
		}
	}

	// Cache
	if v := os.Getenv("LITTLEFEDI_CACHE_ENABLED"); v != "" {
		cfg.Cache.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_CACHE_MAX_MEMORY_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Cache.MaxMemoryMB = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_CACHE_ENTITY_TTL"); v != "" {
		cfg.Cache.EntityTTL = v
	}
	if v := os.Getenv("LITTLEFEDI_CACHE_PAGE_TTL"); v != "" {
		cfg.Cache.PageTTL = v
	}
	if v := os.Getenv("LITTLEFEDI_CACHE_COMPONENT_TTL"); v != "" {
		cfg.Cache.ComponentTTL = v
	}
	if v := os.Getenv("LITTLEFEDI_CACHE_CARD_TTL"); v != "" {
		cfg.Cache.CardTTL = v
	}
	if v := os.Getenv("LITTLEFEDI_CACHE_SERVE_STALE"); v != "" {
		cfg.Cache.ServeStale = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_CACHE_TIMELINE_TTL"); v != "" {
		cfg.Cache.TimelineTTL = v
	}
	if v := os.Getenv("LITTLEFEDI_CACHE_TIMELINE_MAX_ENTRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Cache.TimelineMaxEntries = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_CACHE_FEDERATION_TTL"); v != "" {
		cfg.Cache.FederationTTL = v
	}
	if v := os.Getenv("LITTLEFEDI_CACHE_DISCOVERY_TTL"); v != "" {
		cfg.Cache.DiscoveryTTL = v
	}
	if v := os.Getenv("LITTLEFEDI_CACHE_NEGATIVE_TTL"); v != "" {
		cfg.Cache.NegativeTTL = v
	}
	if v := os.Getenv("LITTLEFEDI_CACHE_MAX_FEDERATION_OBJECT_KB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Cache.MaxFederationObjectKB = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_CACHE_WARM_TIMELINES"); v != "" {
		cfg.Cache.WarmTimelines = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_CACHE_ACTIVE_VIEWER_TTL"); v != "" {
		cfg.Cache.ActiveViewerTTL = v
	}
	if v := os.Getenv("LITTLEFEDI_CACHE_WARM_MAX_VIEWERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Cache.WarmMaxViewers = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_CACHE_WARM_DEBOUNCE"); v != "" {
		cfg.Cache.WarmDebounce = v
	}

	// Mesh
	if v := os.Getenv("LITTLEFEDI_MESH_ENABLED"); v != "" {
		cfg.Mesh.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_MESH_SUFFIX"); v != "" {
		cfg.Mesh.Suffix = v
	}
	if v := os.Getenv("LITTLEFEDI_MESH_LIGHTHOUSES"); v != "" {
		cfg.Mesh.Lighthouses = envList(v)
	}
	if v := os.Getenv("LITTLEFEDI_MESH_DISCOVER_LIGHTHOUSES"); v != "" {
		cfg.Mesh.DiscoverLighthouses = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_MESH_DIRECT_LISTEN"); v != "" {
		cfg.Mesh.DirectListen = v
	}
	if v := os.Getenv("LITTLEFEDI_MESH_DIRECT_ADVERTISE"); v != "" {
		cfg.Mesh.DirectAdvertise = envList(v)
	}
	if v := os.Getenv("LITTLEFEDI_MESH_ALLOW_PRIVATE_DIRECT"); v != "" {
		cfg.Mesh.AllowPrivateDirect = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_MESH_MAX_CIRCUITS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Mesh.MaxCircuits = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_MESH_EXPOSURE"); v != "" {
		cfg.Mesh.Exposure = mesh.GatewayMode(v)
	}
	if v := os.Getenv("LITTLEFEDI_MESH_FULL_ACCESS"); v != "" {
		cfg.Mesh.FullAccess = envList(v)
	}
	if v := os.Getenv("LITTLEFEDI_MESH_DIAL_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Mesh.DialTimeout = d
		}
	}
	if v := os.Getenv("LITTLEFEDI_MESH_DIRECTORY_URL"); v != "" {
		cfg.Mesh.DirectoryURL = v
	}
	if v := os.Getenv("LITTLEFEDI_MESH_DIRECTORY_STATE"); v != "" {
		cfg.Mesh.DirectoryState = v
	}
	if v := os.Getenv("LITTLEFEDI_MESH_ALIAS"); v != "" {
		cfg.Mesh.Alias = v
	}
	if v := os.Getenv("LITTLEFEDI_MESH_RECORD_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Mesh.RecordTTL = d
		}
	}
	if v := os.Getenv("LITTLEFEDI_MESH_PUBLISH_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Mesh.PublishInterval = d
		}
	}

	// Lighthouse
	if v := os.Getenv("LITTLEFEDI_LIGHTHOUSE_LISTEN"); v != "" {
		cfg.Lighthouse.Listen = v
	}
	if v := os.Getenv("LITTLEFEDI_LIGHTHOUSE_GATEWAY_LISTEN"); v != "" {
		cfg.Lighthouse.GatewayListen = v
	}
	if v := os.Getenv("LITTLEFEDI_LIGHTHOUSE_GATEWAY_MODE"); v != "" {
		cfg.Lighthouse.GatewayMode = mesh.GatewayMode(v)
	}
	if v := os.Getenv("LITTLEFEDI_LIGHTHOUSE_GATEWAY_FULL_ACCESS"); v != "" {
		cfg.Lighthouse.GatewayFullAccess = envList(v)
	}
	if v := os.Getenv("LITTLEFEDI_LIGHTHOUSE_ALLOWLIST"); v != "" {
		cfg.Lighthouse.Allowlist = envList(v)
	}
	if v := os.Getenv("LITTLEFEDI_LIGHTHOUSE_MAX_BYTES_PER_NODE_PER_HOUR"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Lighthouse.MaxBytesPerNodePerHour = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_LIGHTHOUSE_MAX_CIRCUITS_PER_NODE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Lighthouse.MaxCircuitsPerNode = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_LIGHTHOUSE_MAX_CONNECTIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Lighthouse.MaxConnections = n
		}
	}
	if v := os.Getenv("LITTLEFEDI_LIGHTHOUSE_MAX_CONNECTIONS_PER_IP"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Lighthouse.MaxConnectionsPerIP = n
		}
	}

	// Directory
	if v := os.Getenv("LITTLEFEDI_DIRECTORY_LISTEN"); v != "" {
		cfg.Directory.Listen = v
	}
	if v := os.Getenv("LITTLEFEDI_DIRECTORY_STATE_FILE"); v != "" {
		cfg.Directory.StateFile = v
	}
	if v := os.Getenv("LITTLEFEDI_DIRECTORY_OPEN_REGISTRATION"); v != "" {
		cfg.Directory.OpenRegistration = v == "true" || v == "1"
	}
	if v := os.Getenv("LITTLEFEDI_DIRECTORY_MAX_RECORDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Directory.MaxRecords = n
		}
	}
}

// envList splits a comma-separated environment value, so a container-only
// deployment can set the list settings mesh nodes actually need.
func envList(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func normalizeDomain(domain string) string {
	return strings.ToLower(strings.TrimSpace(domain))
}

func loopbackListenAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// DefaultUserAgent is the identifier sent with outbound federation requests.
// A mesh node learns its domain after the config is loaded, so it fills this
// in once the identity is known.
func DefaultUserAgent(webDomain string) string {
	return fmt.Sprintf("littleFedi/%s (+https://%s)", version.Version, webDomain)
}

// validateMesh checks the littleMesh settings and normalizes the suffix.
func (c *Config) validateMesh() error {
	c.Mesh.Suffix = strings.TrimSuffix(normalizeDomain(c.Mesh.Suffix), ".")
	c.Mesh.Alias = strings.TrimSuffix(normalizeDomain(c.Mesh.Alias), ".")
	c.Mesh.DirectoryURL = strings.TrimRight(strings.TrimSpace(c.Mesh.DirectoryURL), "/")
	c.Mesh.DirectoryState = strings.TrimSpace(c.Mesh.DirectoryState)
	c.Directory.Listen = strings.TrimSpace(c.Directory.Listen)
	c.Directory.StateFile = strings.TrimSpace(c.Directory.StateFile)
	if c.Directory.Listen != "" {
		if _, _, err := net.SplitHostPort(c.Directory.Listen); err != nil {
			return fmt.Errorf("config: directory.listen must be host:port (got %q)", c.Directory.Listen)
		}
		if c.Directory.StateFile == "" {
			return fmt.Errorf("config: directory.state_file is required when directory.listen is set")
		}
		if c.Directory.MaxRecords <= 0 {
			return fmt.Errorf("config: directory.max_records must be positive")
		}
	}
	if c.Mesh.DirectoryURL != "" {
		u, err := url.Parse(c.Mesh.DirectoryURL)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
			(u.Scheme != "https" && u.Scheme != "http") {
			return fmt.Errorf("config: mesh.directory_url must be an HTTP(S) base URL")
		}
		host := u.Hostname()
		ip := net.ParseIP(host)
		loopback := strings.EqualFold(host, "localhost") || ip != nil && ip.IsLoopback()
		if u.Scheme != "https" && !loopback {
			return fmt.Errorf("config: mesh.directory_url must use HTTPS unless it targets loopback")
		}
		if c.Mesh.DirectoryState == "" {
			return fmt.Errorf("config: mesh.directory_state is required with mesh.directory_url")
		}
	}
	if c.Mesh.Alias != "" {
		if err := validateDomain("mesh.alias", c.Mesh.Alias); err != nil {
			return err
		}
		if c.Mesh.DirectoryURL == "" {
			return fmt.Errorf("config: mesh.alias requires mesh.directory_url")
		}
		if c.LocalDomain != "" && c.LocalDomain != c.Mesh.Alias {
			return fmt.Errorf("config: local_domain must match mesh.alias when a readable mesh alias is configured")
		}
		if c.WebDomain != "" {
			nodeID, suffix, ok := mesh.SplitDomain(c.WebDomain)
			if !ok || !mesh.ValidNodeID(nodeID) || c.Mesh.Suffix != "" && suffix != c.Mesh.Suffix {
				return fmt.Errorf("config: web_domain must be the technical <node-id>.%s domain or unset when mesh.alias is configured", c.Mesh.Suffix)
			}
		}
	}
	if c.Mesh.RecordTTL <= 0 {
		return fmt.Errorf("config: mesh.record_ttl must be positive")
	}
	if c.Mesh.RecordTTL > mesh.MaxDirectoryRecordTTL {
		return fmt.Errorf("config: mesh.record_ttl must not exceed %s", mesh.MaxDirectoryRecordTTL)
	}
	if c.Mesh.PublishInterval <= 0 || c.Mesh.PublishInterval >= c.Mesh.RecordTTL {
		return fmt.Errorf("config: mesh.publish_interval must be positive and shorter than mesh.record_ttl")
	}
	c.Mesh.Exposure = mesh.GatewayMode(strings.ToLower(strings.TrimSpace(string(c.Mesh.Exposure))))
	if c.Mesh.Exposure == "" {
		c.Mesh.Exposure = mesh.GatewayModeFederation
	}
	switch c.Mesh.Exposure {
	case mesh.GatewayModeFederation, mesh.GatewayModeFull, mesh.GatewayModeAllowlist:
	default:
		return fmt.Errorf("config: mesh.exposure must be federation, full, or allowlist")
	}
	if c.Mesh.Exposure != mesh.GatewayModeAllowlist && len(c.Mesh.FullAccess) > 0 {
		return fmt.Errorf("config: mesh.full_access requires exposure = \"allowlist\"")
	}
	for i, nodeID := range c.Mesh.FullAccess {
		nodeID = strings.ToLower(strings.TrimSpace(nodeID))
		if !mesh.ValidNodeID(nodeID) {
			return fmt.Errorf("config: mesh.full_access contains invalid node id %q", nodeID)
		}
		c.Mesh.FullAccess[i] = nodeID
	}
	for index, entry := range c.Mesh.Lighthouses {
		address, pin, found := strings.Cut(strings.TrimSpace(entry), "@")
		address = strings.TrimSpace(address)
		pin = strings.ToLower(strings.TrimSpace(pin))
		if address == "" {
			return fmt.Errorf("config: mesh.lighthouses contains an empty address")
		}
		if found && !mesh.ValidNodeID(pin) {
			return fmt.Errorf("config: mesh lighthouse pin %q is not a node id", pin)
		}
		c.Mesh.Lighthouses[index] = address
		if found {
			c.Mesh.Lighthouses[index] += "@" + pin
		}
	}
	if c.Lighthouse.MaxConnections < 0 {
		return fmt.Errorf("config: lighthouse.max_connections must be 0 (the default) or positive")
	}
	if c.Lighthouse.MaxConnectionsPerIP < 0 {
		return fmt.Errorf("config: lighthouse.max_connections_per_ip must be 0 (the default) or positive")
	}
	c.Lighthouse.GatewayMode = mesh.GatewayMode(strings.ToLower(strings.TrimSpace(string(c.Lighthouse.GatewayMode))))
	c.Lighthouse.Exposure = mesh.GatewayMode(strings.ToLower(strings.TrimSpace(string(c.Lighthouse.Exposure))))
	if c.Lighthouse.GatewayMode != "" && c.Lighthouse.Exposure != "" &&
		c.Lighthouse.GatewayMode != c.Lighthouse.Exposure {
		return fmt.Errorf("config: lighthouse.gateway_mode and lighthouse.exposure conflict")
	}
	if c.Lighthouse.GatewayMode == "" {
		c.Lighthouse.GatewayMode = c.Lighthouse.Exposure
	}
	if c.Lighthouse.GatewayMode == "" {
		c.Lighthouse.GatewayMode = mesh.GatewayModeFederation
	}
	switch c.Lighthouse.GatewayMode {
	case mesh.GatewayModeFederation, mesh.GatewayModeFull, mesh.GatewayModeAllowlist:
	default:
		return fmt.Errorf("config: lighthouse.gateway_mode must be federation, full, or allowlist")
	}
	if c.Lighthouse.GatewayMode != mesh.GatewayModeAllowlist && len(c.Lighthouse.GatewayFullAccess) > 0 {
		return fmt.Errorf("config: lighthouse.gateway_full_access requires gateway_mode = \"allowlist\"")
	}
	for i, nodeID := range c.Lighthouse.GatewayFullAccess {
		nodeID = strings.ToLower(strings.TrimSpace(nodeID))
		if !mesh.ValidNodeID(nodeID) {
			return fmt.Errorf("config: lighthouse.gateway_full_access contains invalid node id %q", nodeID)
		}
		c.Lighthouse.GatewayFullAccess[i] = nodeID
	}
	if c.Mesh.Suffix != "" && !mesh.ValidSuffix(c.Mesh.Suffix) {
		return fmt.Errorf(`config: mesh.suffix must be a DNS-safe "mesh" name (got %q)`, c.Mesh.Suffix)
	}
	if c.Lighthouse.GatewayListen != "" && (c.Mesh.Suffix == "" || c.Mesh.Suffix == "mesh") {
		return fmt.Errorf("config: lighthouse.gateway_listen requires a public mesh.suffix")
	}
	if !c.Mesh.Enabled {
		return nil
	}
	if len(c.Mesh.Lighthouses) == 0 && c.Mesh.DirectListen == "" {
		return fmt.Errorf("config: mesh.enabled needs at least one mesh.lighthouses entry, or a mesh.direct_listen address for a node that is already reachable")
	}
	if c.Mesh.Suffix == "" && len(c.Mesh.Lighthouses) == 0 {
		return fmt.Errorf("config: mesh.suffix is required when no lighthouse is configured to derive it from")
	}
	if c.Mesh.MaxCircuits < 0 {
		return fmt.Errorf("config: mesh.max_circuits must be 0 (the default) or positive")
	}
	if c.Mesh.Suffix == "" {
		address, _, _ := strings.Cut(c.Mesh.Lighthouses[0], "@")
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			host = strings.Trim(address, "[]")
		}
		if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
			return fmt.Errorf("config: mesh.suffix is required when the first lighthouse uses IPv6")
		}
		if port != "" && port != mesh.DefaultLighthousePort {
			return fmt.Errorf("config: mesh.suffix is required when the first lighthouse uses non-default port %s", port)
		}
	}
	if c.Mesh.DirectListen != "" {
		if _, _, err := net.SplitHostPort(c.Mesh.DirectListen); err != nil {
			return fmt.Errorf("config: mesh.direct_listen must be host:port (got %q)", c.Mesh.DirectListen)
		}
	}
	return nil
}

func validateDomain(name, domain string) error {
	if strings.ContainsAny(domain, "/?#") || strings.Contains(domain, "://") || strings.ContainsAny(domain, " \t\r\n") {
		return fmt.Errorf("config: %s must be a hostname with optional port, without scheme or path (got %q)", name, domain)
	}
	u, err := url.Parse("//" + domain)
	if err != nil || u.Host != domain || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("config: %s must be a valid hostname with optional port (got %q)", name, domain)
	}
	return nil
}

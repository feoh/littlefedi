package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/accounts"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/api"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/app"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/backup"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/cache"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/federation"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/housekeeping"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/instancemeta"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/keycrypt"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mail"
	medialib "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media/objectstore"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mediacache"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/memguard"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mesh"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/metrics"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/oauth"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/push"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/queue"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/streaming"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/version"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/webui"
)

// cacheBudgetBytes is the whole in-memory cache budget, before cacheBudgets
// splits it between the layers. Zero when the cache is switched off.
func cacheBudgetBytes(cfg *config.Config) int64 {
	if !cfg.Cache.Enabled || cfg.Cache.MaxMemoryMB <= 0 {
		return 0
	}
	return int64(cfg.Cache.MaxMemoryMB) * 1024 * 1024
}

func cacheBudgets(total int64, lowPower bool) (entity, timeline, page, federation int64) {
	entity, timeline, page = total*40/100, total*25/100, total*20/100
	if lowPower {
		entity, timeline = total*25/100, total*10/100
	}
	federation = total - entity - timeline - page
	return
}

func main() {
	// The portable SQLite fallback owns memory outside the Go heap. Keep the Go
	// side bounded automatically so NetBSD/ARM and small illumos zones are safe
	// even when low_power was not selected. GOMEMLIMIT remains an explicit
	// operator override. This must run before `init`, which opens SQLite too.
	//
	// The configuration has not been read yet, so this is the conservative
	// floor and nothing more: enough for the CLI subcommands below, which hold
	// no cache and return before anything else starts. The daemon replaces it
	// with a limit derived from what it is actually going to hold, once it
	// knows. Never lower the value here without checking memguard.Compute: a
	// limit under the working set does not save a small host, it stops it.
	operatorMemoryLimit := os.Getenv("GOMEMLIMIT") != ""
	if storage.SQLiteArenaConstrained() && !operatorMemoryLimit {
		debug.SetMemoryLimit(memguard.MinAutoLimit)
	}

	// Look for a CLI subcommand (init, admin, migrate, post, help) anywhere after global flags.
	// Global flags like --config can appear before the subcommand.
	if cmd, cmdArgs, configPath := extractCLICommand(os.Args); cmd != "" {
		if cmd == "init" {
			if err := keycrypt.SetKey(os.Getenv("LITTLEFEDI_FEDERATION_KEY_ENCRYPTION_KEY")); err != nil {
				fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
				os.Exit(1)
			}
			if err := runInitCommand(cmdArgs, configPath, os.Stdin, os.Stdout); err != nil {
				fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
				os.Exit(1)
			}
			return
		}
		// Help and usage-only commands don't need config.
		if cmd == "help" || cmd == "version" || (cmd == "admin" && len(cmdArgs) == 0) || (cmd == "mesh" && len(cmdArgs) == 0) || (cmd == "post" && hasHelpFlag(cmdArgs)) {
			if cmd == "version" {
				fmt.Println("littlefedi " + version.Version)
			} else if cmd == "help" {
				printUsage()
			} else if cmd == "post" {
				printPostUsage(os.Stderr)
			} else if cmd == "mesh" {
				printMeshUsage()
			} else {
				printAdminUsage()
			}
			return
		}

		cfg, err := config.Load(configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
			os.Exit(1)
		}
		if err := keycrypt.SetKey(cfg.Federation.KeyEncryptionKey); err != nil {
			fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
			os.Exit(1)
		}
		if cfg.SelfDestruct && cmd != "admin" && (cmd != "backup" || len(cmdArgs) > 0 && cmdArgs[0] == "restore") {
			fmt.Fprintln(os.Stderr, "Self-destruct mode is enabled; mutating CLI commands are refused.")
			return
		}

		switch cmd {
		case "admin":
			runAdminCommand(cmdArgs, cfg)
		case "migrate":
			runMigrate(cfg)
		case "post":
			runPostCommand(cmdArgs, cfg)
		case "backup":
			runBackupCommand(cmdArgs, cfg)
		case "mesh":
			runMeshCommand(cmdArgs, cfg)
		case "lighthouse":
			runLighthouseCommand(cmdArgs, cfg)
		case "directory":
			runDirectoryCommand(cmdArgs, cfg)
		}
		return
	}

	configPath := flag.String("config", "", "Path to TOML config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
		os.Exit(1)
	}
	// Installed before anything reads an account: a sealed key that cannot be
	// unsealed has to stop the process here, not halfway through the first
	// delivery.
	if err := keycrypt.SetKey(cfg.Federation.KeyEncryptionKey); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
		os.Exit(1)
	}

	logLevel := slog.LevelInfo
	switch cfg.Observability.LogLevel {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	}
	slog.SetLogLoggerLevel(logLevel)
	var handler slog.Handler
	if stdoutSupportsColor() {
		handler = newColorHandler(os.Stdout, logLevel)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)

	slog.Info("starting littlefedi",
		"local_domain", cfg.LocalDomain,
		"web_domain", cfg.WebDomain,
		"listen", cfg.HTTP.Listen,
		"background_maintenance", cfg.BackgroundMaintenance)
	if storage.SQLiteArenaConstrained() {
		// A single image decode can be the largest Go allocation in the process.
		// Do not multiply it beside the fallback driver's reserved arena.
		cfg.Media.ProcessingConcurrency = 1
		slog.Info("portable SQLite memory guard active",
			"db_max_open_conns", storage.SQLiteMaxPoolSize(cfg.DB.MaxOpenConns),
			"media_processing_concurrency", 1)
	}
	if cfg.LowPower {
		slog.Info("low_power preset active",
			"queue_workers", cfg.Queue.Workers,
			"cache_enabled", cfg.Cache.Enabled,
			"cache_max_memory_mb", cfg.Cache.MaxMemoryMB,
			"db_max_open_conns", cfg.DB.MaxOpenConns,
			"home_feed_retention", cfg.Housekeeping.HomeFeedRetention,
			"gzip", cfg.HTTP.GzipEnabled())
	}

	// A soft memory limit lets the GC run lazily below it and protects a small
	// device from OOM, but only while it is a limit the process can actually
	// reach. Derive it from the SQLite arena, the cache budget and the host's
	// memory rather than picking a number: a limit under what is already
	// committed pins the collector at 100% forever and frees nothing, which is
	// a worse failure than the one it was guarding against.
	//
	// GOGC is untouched: lowering it trades scarce CPU for RAM the
	// limit already caps. A user-provided GOMEMLIMIT always wins.
	sqliteArena := storage.SQLiteArenaBytes(cfg.DB.MaxOpenConns)
	physMem := memguard.PhysicalMemory()
	memPlan := memguard.Compute(memguard.Inputs{
		Arena:   sqliteArena,
		Cache:   cacheBudgetBytes(cfg),
		PhysMem: physMem,
	})
	if memPlan.CacheReduced {
		slog.Warn("cache budget does not fit in this host's memory; reduced",
			"configured_mib", cfg.Cache.MaxMemoryMB,
			"effective_mib", memPlan.Cache>>20,
			"sqlite_arena_mib", sqliteArena>>20,
			"physmem_mib", physMem>>20)
		cfg.Cache.MaxMemoryMB = int(memPlan.Cache >> 20)
		if cfg.Cache.MaxMemoryMB <= 0 {
			cfg.Cache.Enabled = false
		}
	}
	manageMemoryLimit := !operatorMemoryLimit && (storage.SQLiteArenaConstrained() || cfg.LowPower)
	if manageMemoryLimit {
		debug.SetMemoryLimit(memPlan.Limit)
		slog.Info("Go memory limit set",
			"limit_mib", memPlan.Limit>>20,
			"ceiling_mib", memPlan.Ceiling>>20,
			"sqlite_arena_mib", sqliteArena>>20,
			"cache_mib", memPlan.Cache>>20,
			"headroom_mib", memguard.WorkingHeadroom>>20,
			"physmem_mib", physMem>>20)
	}
	// On portable WASM-SQLite hosts an unlimited housekeeping delete pass runs
	// an unbounded ORDER BY over media tables inside the driver's hard 32MB
	// memory arena and can crash the process. Clamp rather than trust the
	// config.
	if storage.SQLiteArenaConstrained() && cfg.Housekeeping.MaxDeletePerRun <= 0 {
		cfg.Housekeeping.MaxDeletePerRun = 500
		slog.Warn("housekeeping: max_delete_per_run=0 (unlimited) is unsafe with the portable WASM SQLite driver; clamped",
			"max_delete_per_run", cfg.Housekeeping.MaxDeletePerRun)
	}

	medialib.ConfigureProcessingLimits(
		cfg.Media.MaxImagePixels,
		cfg.Media.MaxImageDimension,
		cfg.Media.ProcessingConcurrency,
	)
	medialib.ConfigureResampleFilter(cfg.Media.ResampleFilter)
	medialib.ConfigureStoredDimension(cfg.Media.MaxStoredDimension)

	emailMailer := mail.New(cfg.SMTP, cfg.WebDomain, cfg.LocalDomain)
	var sendDatabaseAlert func(to, subject, body string) error
	if emailMailer != nil {
		sendDatabaseAlert = emailMailer.Send
	}
	databaseAlert := newDatabaseAlert(sendDatabaseAlert, cfg.WebDomain, cfg.SMTP.DatabaseAlertTo, nil)
	store, err := storage.NewBunStorage(
		cfg.DB.DSN,
		cfg.DB.MaxOpenConns,
		cfg.DB.MaxIdleConns,
		cfg.DB.LogQueries,
	)
	if err != nil {
		databaseAlert.notifyBeforeExit(err)
		slog.Error("failed to initialize storage", "error", err)
		os.Exit(1)
	}
	rawStore := store
	defer store.Close()
	// Read the pool size back rather than trusting the configured one: the
	// SQLite driver may have capped it, and the background limiter has to be
	// sized from what the database actually hands out.
	effectiveDBConns := cfg.DB.MaxOpenConns
	if stats, ok := store.(interface{ DBStats() sql.DBStats }); ok {
		metrics.SetDBStatsProvider(stats.DBStats)
		if n := stats.DBStats().MaxOpenConnections; n > 0 {
			effectiveDBConns = n
		}
	}
	backgroundLimit := queue.NewBackgroundLimit(effectiveDBConns)

	ctx, cancelApp := context.WithCancel(context.Background())
	defer cancelApp()

	// The derivation above accounts for what the configuration declares. It
	// cannot account for a workload nobody modelled, so watch the collector and
	// give the limit back some room if it turns out to have been too tight.
	// Room is only ever given back to a limit this process chose itself: an
	// operator's GOMEMLIMIT is their decision, and the answer to it going badly
	// is to say so loudly rather than to overrule it, which is why the watching
	// covers that case too. Where no limit is in force at all there is nothing
	// to diagnose, and watching anyway would let an unrelated cause, a very low
	// GOGC being the obvious one, be reported as a limit problem.
	if manageMemoryLimit || operatorMemoryLimit {
		go memguard.Watch(ctx, memPlan.Ceiling, manageMemoryLimit)
	}

	if err := store.RunMigrations(ctx); err != nil {
		databaseAlert.notifyBeforeExit(err)
		slog.Error("failed to run migrations", "error", err)
		os.Exit(1)
	}
	slog.Info("database migrations complete")
	if emailMailer == nil {
		slog.Warn("database email alerts disabled: SMTP is not configured")
	} else {
		admins, adminErr := store.GetAdmins(ctx)
		if adminErr != nil {
			slog.Error("database alerts: failed to load administrator addresses", "error", adminErr)
		} else {
			databaseAlert.refresh(admins)
		}
		if reporter, ok := rawStore.(interface{ SetCriticalErrorHandler(func(error)) }); ok {
			reporter.SetCriticalErrorHandler(databaseAlert.notify)
		}
		go runDatabaseMonitor(ctx, rawStore, databaseAlert)
	}

	// littleMesh settles the instance's identity, and for a node with no
	// domain of its own the identity settles the domain. Everything that mints
	// an ActivityPub URI (account bootstrap, the instance actor, the
	// federator) reads cfg.WebDomain, so this has to happen first.
	meshIdentity, err := resolveMeshIdentity(ctx, store, cfg)
	if err != nil {
		slog.Error("failed to resolve mesh identity", "error", err)
		os.Exit(1)
	}
	var meshNode *mesh.Node
	if meshIdentity != nil {
		directory, directoryErr := meshDirectoryClient(cfg)
		if directoryErr != nil {
			slog.Error("failed to load mesh directory state", "error", directoryErr)
			os.Exit(1)
		}
		meshNode, err = startMeshNode(ctx, cfg, meshIdentity, directory)
		if err != nil {
			slog.Error("failed to start mesh node", "error", err)
			os.Exit(1)
		}
		defer meshNode.Close()
		slog.Info("littleMesh enabled", "node_id", meshIdentity.NodeID, "domain", cfg.WebDomain)
		startMeshDirectoryPublisher(ctx, cfg, meshIdentity, directory)
	}

	// Fresh planner statistics at every start: stale SQLite stats send
	// timeline head queries onto pathological plans (full sort instead of the
	// ordered partial index). Bounded by analysis_limit, so this is cheap.
	if err := store.Optimize(ctx); err != nil {
		slog.Warn("planner statistics refresh failed", "error", err)
	}

	// Relationship-set memoisation lives inside the raw store (decorators can't
	// see its internal calls); capture the invalidation hook before wrapping so
	// the postgres broker can keep it coherent across processes.
	relInvalidator, _ := store.(interface{ InvalidateRelationshipCaches() })

	// Set up the worker pool and wrap the store so every successful local
	// enqueue wakes an idle worker immediately - the poll loop is only a
	// safety net for retry-scheduled jobs and cross-process enqueues
	// (postgres mode). This wrapper must sit BELOW the cache decorators:
	// consumers type-assert optional capabilities (TimelineGenerationFor) on
	// the outermost store, and an outer wrapper embedding only the Storage
	// interface would hide them - which silently disabled page-cache
	// generation validation, freezing timelines for a full page TTL.
	foregroundGate := queue.NewForegroundGate()
	workerPool := queue.NewWorkerPool(store, cfg.Queue.Workers)
	workerPool.SetName("main")
	if cfg.SelfDestruct {
		workerPool.SetAllowedKinds(models.SelfDestructDeliveryJobKind)
	}
	workerPool.SetForegroundGate(foregroundGate)
	workerPool.SetBackgroundLimit(backgroundLimit)
	workerPool.ConfigureMaintenance(
		cfg.Queue.TerminalJobRetention,
		cfg.Queue.StaleRunningTimeout,
		cfg.Queue.MaintenanceInterval,
	)
	if cfg.SelfDestruct {
		// Keep failed and legacy completed work intact while this mode is active.
		workerPool.ConfigureMaintenance(0, cfg.Queue.StaleRunningTimeout, cfg.Queue.MaintenanceInterval)
	}
	pollBackoffMax := cfg.Queue.PollBackoffMax
	if pollBackoffMax == 0 {
		// Auto: local enqueues and retries self-wake a worker, so single-process
		// idle polling is pure crash recovery. Postgres multi-process keeps a
		// tighter cap - the poll also picks up other processes' enqueues.
		if strings.EqualFold(cfg.Streaming.Backend, "postgres") {
			pollBackoffMax = 60 * time.Second
		} else {
			pollBackoffMax = 5 * time.Minute
		}
	}
	workerPool.ConfigurePolling(cfg.Queue.PollInterval, pollBackoffMax)

	// Optional dedicated pool for the light, mostly network-bound outbound
	// job kinds: federation delivery (one signed POST to one remote inbox),
	// its fan-out expander, push notifications, and email. These are short
	// and often latency-sensitive (a remote instance or a user expects
	// timely delivery), but the main pool's workers block synchronously on
	// the heavier kinds, inbox (signature verification, actor fetch, home
	// feed fan-out) and media_cache (image decode), which can delay them.
	// Giving them their own pool/goroutine avoids that without touching job
	// priority (priority only orders queued rows; it can't preempt a job
	// already running). Disabled (light_workers=0) leaves all four kinds on
	// the main pool, identical to previous behavior. lightTarget is whichever
	// pool should register their handlers below.
	var lightPool *queue.WorkerPool
	lightTarget := workerPool
	if cfg.Queue.LightWorkers > 0 && !cfg.SelfDestruct {
		lightPool = queue.NewWorkerPool(store, cfg.Queue.LightWorkers)
		lightPool.SetName("light")
		lightPool.SetAllowedKinds(app.LightJobKinds...)
		// Both pools contend for the same reserve slot, so the delay is what
		// decides which one gets it. Light jobs are short and their latency is
		// visible to people (a boost that has not landed yet, a push that has
		// not arrived), while the main pool holds slow inbox and media work,
		// so the light pool is given the shorter wait.
		lightPool.SetQueueReserveDelay(3 * time.Second)
		lightPool.SetForegroundGate(foregroundGate)
		// Not given the background limiter: these kinds are
		// dominated by the remote server's response time, not by database work,
		// so capping their concurrency would slow federation down without
		// freeing the connection they were barely using.
		// Maintenance (stale-job recovery, payload/terminal-job pruning) is
		// idempotent and cheap at the default 1h interval; running it from
		// both pools against the same jobs table is a deliberate, harmless
		// duplication rather than new cross-pool coordination.
		lightPool.ConfigureMaintenance(
			cfg.Queue.TerminalJobRetention,
			cfg.Queue.StaleRunningTimeout,
			cfg.Queue.MaintenanceInterval,
		)
		lightPool.ConfigurePolling(cfg.Queue.PollInterval, pollBackoffMax)
		lightTarget = lightPool
	}

	if lightPool != nil {
		lightKinds := make(map[string]bool, len(app.LightJobKinds))
		for _, k := range app.LightJobKinds {
			lightKinds[k] = true
		}
		store = storage.NewEnqueueSignalStorageByKind(store, func(kind string, n int) {
			if lightKinds[kind] {
				lightPool.NotifyN(n)
			} else {
				workerPool.NotifyN(n)
			}
		})
	} else {
		store = storage.NewEnqueueSignalStorageN(store, workerPool.NotifyN)
	}

	// Keep blog hooks below the caches so the outer store exposes timeline generations.
	store = wrapBlogStorage(store, cfg)

	// Optional in-RAM cache (Redis-free) to speed up timeline/profile navigation
	// on low-power devices. The entity layer transparently wraps storage; the
	// page layer is injected into the web/API handlers further below. Wrapping
	// happens after migrations so schema changes always run against the raw store.
	var pageCache *cache.MemoryCache
	var entityCache *cache.MemoryCache
	var timelineCache *cache.MemoryCache
	var federationResponseCache *cache.MemoryCache
	var federationResponseTTL time.Duration
	var discoveryTTL time.Duration
	var negativeTTL time.Duration
	var pageTTL time.Duration
	var componentTTL time.Duration
	var cardTTL time.Duration
	if cfg.Cache.Enabled {
		entityBudget, timelineBudget, pageBudget, federationBudget := cacheBudgets(cacheBudgetBytes(cfg), cfg.LowPower)
		entityTTL, _ := cfg.Cache.EntityTTLDuration()
		federationResponseTTL, _ = cfg.Cache.FederationTTLDuration()
		discoveryTTL, _ = cfg.Cache.DiscoveryTTLDuration()
		negativeTTL, _ = cfg.Cache.NegativeTTLDuration()
		timelineTTL, _ := cfg.Cache.TimelineTTLDuration()
		// A local broker means one process owns the SQLite-style cache coherence:
		// every membership-changing write passes through the decorators below.
		// Expiring a hot membership window here only forces the multi-second full
		// home query again. Keep it until an explicit invalidation or LRU eviction.
		// PostgreSQL/multi-process mode retains the configured TTL as a backstop
		// for a broker notification missed by this process.
		if !strings.EqualFold(cfg.Streaming.Backend, "postgres") {
			timelineTTL = 0
		}
		pageTTL, _ = cfg.Cache.PageTTLDuration()
		componentTTL, _ = cfg.Cache.ComponentTTLDuration()
		cardTTL, _ = cfg.Cache.CardTTLDuration()
		entityCache = cache.New(entityBudget)
		store = storage.NewCachingStorage(store, entityCache, entityTTL)
		federationResponseCache = cache.New(federationBudget)
		timelineCache = cache.New(timelineBudget)
		store = storage.NewTimelineCachingStorageWithResponses(store, timelineCache, federationResponseCache, timelineTTL, cfg.Cache.TimelineMaxEntries)
		pageCache = cache.New(pageBudget)
		metrics.SetCacheProviders(map[string]func() cache.Stats{
			"entity": entityCache.Stats, "timeline": timelineCache.Stats,
			"components": pageCache.Stats, "federation": federationResponseCache.Stats,
		})
		slog.Info("in-memory cache enabled",
			"max_memory_mb", cfg.Cache.MaxMemoryMB,
			"entity_budget_bytes", entityBudget,
			"timeline_budget_bytes", timelineBudget,
			"page_budget_bytes", pageBudget,
			"federation_budget_bytes", federationBudget,
			"entity_ttl", entityTTL, "timeline_ttl", timelineTTL,
			"timeline_max_entries", cfg.Cache.TimelineMaxEntries, "page_ttl", pageTTL,
			"component_ttl", componentTTL, "card_ttl", cardTTL, "serve_stale", cfg.Cache.ServeStale)
	}
	// Set up local account(s). Decommissioning must not create or update human
	// identities; it only uses the identities already persisted.
	if cfg.SelfDestruct {
		slog.Warn("self-destruct mode: skipping local account bootstrap")
	} else if cfg.SingleUser != "" {
		// Single-user mode: create/maintain the configured user (always admin)
		account, err := accounts.BootstrapLocalAccount(ctx, store, cfg.WebDomain, cfg.SingleUser, cfg.Web.Password, true)
		if err != nil {
			slog.Error("failed to set up local account", "error", err)
			os.Exit(1)
		}
		slog.Info("local account ready", "username", account.Username, "actor", account.ActorURI)
	} else if len(cfg.Admins) > 0 {
		// Multi-user mode: ensure admin accounts exist with the configured password (if any)
		for _, username := range cfg.Admins {
			account, err := accounts.BootstrapLocalAccount(ctx, store, cfg.WebDomain, username, cfg.Web.Password, true)
			if err != nil {
				slog.Error("failed to set up admin account", "username", username, "error", err)
				os.Exit(1)
			}
			slog.Info("admin account ready", "username", account.Username, "actor", account.ActorURI)
		}
	} else if cfg.IsMultiUser() {
		slog.Info("multi-user mode enabled, no admin accounts configured in config (use CLI to create first admin)")
	}
	if err := validatePersistedWebDomain(ctx, store, cfg.WebDomain); err != nil {
		slog.Error("configured web domain conflicts with persisted ActivityPub identities", "error", err)
		os.Exit(1)
	}

	// Set up instance actor for signed fetches
	instanceSigner, err := federation.SetupInstanceActorWithDomains(ctx, store, cfg.LocalDomain, cfg.WebDomain)
	if err != nil {
		slog.Error("failed to set up instance actor", "error", err)
		os.Exit(1)
	}
	metrics.Init(cfg.LocalDomain, version.Version)

	slog.Info("instance actor ready for signed fetches")
	if cfg.SelfDestruct {
		run, created, err := store.ScheduleSelfDestruct(ctx)
		if err != nil {
			slog.Error("failed to schedule self-destruct deliveries", "error", err)
			os.Exit(1)
		}
		if created {
			slog.Warn("self-destruct deliveries scheduled", "identities", run.IdentityCount, "peer_inboxes", run.InboxCount, "deliveries", run.DeliveryCount)
		} else {
			slog.Warn("self-destruct mode resumed", "started_at", run.StartedAt, "deliveries", run.DeliveryCount)
		}
	}

	// Set up federator for outbound federation (created before verifier so it can
	// be reused by the key-fetch closure, avoiding new allocation on every miss).
	federator := federation.NewFederatorWithDomains(store, cfg.LocalDomain, cfg.WebDomain, instanceSigner, cfg.Federation.UserAgent)
	federator.SetAccountUpdateHook(func(accountID string) {
		if pageCache != nil {
			pageCache.DeleteDependencies("account:" + accountID)
		}
	})
	federator.SetTransportLimits(cfg.Federation.HTTPMaxIdleConns, cfg.Federation.HTTPMaxIdleConnsPerHost)
	federation.SetSignOutboundOIP(cfg.Federation.SignWithOIP)
	if federationResponseCache != nil {
		federator.SetFetchCache(federationResponseCache, federationResponseTTL, discoveryTTL, negativeTTL, cfg.Cache.MaxFederationObjectKB*1024)
	}
	threadResolver := federation.NewThreadResolver(store, federator, cfg.WebDomain, federation.ThreadFetchConfig{
		Enabled:          cfg.Federation.ThreadFetchEnabled,
		StaleAfter:       cfg.Federation.ThreadFetchStaleAfter,
		Timeout:          cfg.Federation.ThreadFetchTimeout,
		MaxReplies:       cfg.Federation.ThreadFetchMaxReplies,
		MaxAncestorDepth: cfg.Federation.ThreadFetchMaxAncestorDepth,
		MaxPages:         cfg.Federation.ThreadFetchMaxPages,

		BackgroundEnabled:     cfg.Federation.ThreadFetchBackgroundEnabled,
		BackgroundTimeout:     cfg.Federation.ThreadFetchBackgroundTimeout,
		BackgroundMaxReplies:  cfg.Federation.ThreadFetchBackgroundMaxReplies,
		BackgroundMaxPages:    cfg.Federation.ThreadFetchBackgroundMaxPages,
		BackgroundConcurrency: cfg.Federation.ThreadFetchBackgroundConcurrency,
	})
	threadResolver.SetShutdownContext(ctx)

	// Set up verifier for inbound signature verification
	verifier := federation.NewVerifier(func(keyID string) (string, error) {
		actorURI := federation.ExtractKeyOwner(keyID)
		account, err := store.GetAccountByURI(ctx, actorURI)
		if err != nil {
			// Verification only needs the public key: FetchActorLight stores
			// the actor without the count-enriching collection fetches, which
			// would otherwise run synchronously inside the inbound HTTP
			// handler on first contact with a new actor. Counts are refreshed
			// later by the ActorRefresher queue.
			account, err = federator.FetchActorLight(ctx, actorURI)
			if err != nil {
				return "", fmt.Errorf("fetch actor for key verification: %w", err)
			}
		}
		publicKey, keyErr := federation.PublicKeyPEMForID(account, keyID)
		if keyErr == nil {
			return publicKey, nil
		}
		if account.Domain != nil {
			account, err = federator.RefreshActor(ctx, actorURI, federation.ActorRefreshOptions{Force: true})
			if err == nil {
				return federation.PublicKeyPEMForID(account, keyID)
			}
		}
		return "", keyErr
	})

	// Set up inbox processor and delivery handler
	inboxProcessor := federation.NewInboxProcessor(store, federator, cfg.WebDomain)
	inboxProcessor.SetBlockSensitiveRelationshipNotifications(cfg.Instance.BlockSensitiveRelationshipNotifications)
	inboxProcessor.ConfigureBackfill(ctx, cfg.Federation.ThreadFetchBackgroundConcurrency)
	inboxProcessor.SetComponentCache(pageCache)
	deliveryHandler := federation.NewDeliveryHandler(store, federator)
	actorRefresher := federation.NewActorRefresher(
		store, federator,
		cfg.Federation.ActorRefreshInterval,
		cfg.Federation.ActorStaleAge,
		cfg.Federation.ActorRefreshOnDemandStaleAge,
		cfg.Federation.ActorRefreshLease,
		cfg.Federation.ActorRefreshBatchSize,
		cfg.Federation.ActorRefreshQueueHighWatermark,
	)
	federator.SetStaleActorRefresh(actorRefresher.EnqueueIfStale)

	// Streaming API
	// In-process pub/sub hub for the Mastodon streaming API. The event router
	// is the single fan-out point for live timelines/notifications; its
	// serializer (the API handler) is wired in later, once that handler exists.
	// The inbox processor is given the router before the worker pool starts so
	// inbound activities can publish without a data race.
	streamHub := streaming.NewHub()
	streamRouter := streaming.NewEventRouter(streamHub, store, nil)
	streamBroker, err := streaming.NewBroker(cfg.Streaming.Backend, cfg.DB.DSN)
	if err != nil {
		slog.Error("streaming broker configuration failed", "error", err)
		os.Exit(1)
	}
	streamPublisher := streaming.NewBrokerPublisher(streamBroker)
	defer streamBroker.Close()

	effectiveMediaConfig := activeMediaConfig(ctx, store, cfg.Media)
	ownedMediaStore, err := objectstore.New(effectiveMediaConfig, false)
	if err != nil {
		slog.Error("media storage configuration failed", "error", err)
		os.Exit(1)
	}
	if err := ownedMediaStore.Check(ctx); err != nil {
		slog.Error("media storage unavailable", "error", err)
		os.Exit(1)
	}
	mediaPublicBases := []string{ownedMediaStore.URL("media/probe")}
	workerPool.RegisterHandler(models.AccountDeletionDeliveryJobKind, app.HandleAccountDeletionDeliveryJob(store, federator, ownedMediaStore))
	workerPool.RegisterHandler(models.AccountDeletionFinalizeJobKind, app.HandleAccountDeletionFinalizeJob(store, ownedMediaStore))
	registerMediaMigrationHandler(workerPool, store)
	registerBlogRebuildHandler(workerPool, store, cfg)

	// Register inbox handler (processes incoming ActivityPub activities)
	workerPool.RegisterHandler("inbox", func(ctx context.Context, payload string) error {
		err := inboxProcessor.ProcessActivity(ctx, []byte(payload))
		if err == nil {
			metrics.IncrementInboxProcessed()
		} else if ctx.Err() == nil {
			metrics.IncrementInboxFailed()
		}
		return err
	})

	// Register delivery handler (delivers activities to remote instances;
	// dedicated pool when light_workers > 0)
	lightTarget.RegisterHandler("delivery", func(ctx context.Context, payload string) error {
		return deliveryHandler.ProcessDelivery(ctx, payload)
	})
	// Self-destruct delivery always stays on the main pool: it's the only
	// kind that pool is restricted to under SelfDestruct, and the light pool
	// is never created in that mode.
	workerPool.RegisterHandler(models.SelfDestructDeliveryJobKind, func(ctx context.Context, payload string) error {
		return deliveryHandler.ProcessSelfDestructDelivery(ctx, payload)
	})
	// Expands a follower delivery into per-inbox delivery jobs off the
	// request path (the post handler persists one fan-out job and returns).
	lightTarget.RegisterHandler(app.DeliveryFanoutJobKind, app.HandleDeliveryFanoutJob(store))
	workerPool.RegisterHandler(federation.ActorRefreshJobKind, actorRefresher.ProcessJob)
	if bigTechSyncer, syncErr := federation.NewBigTechListSyncer(store, cfg.Federation.CommercialBigTechListURL); syncErr != nil {
		slog.Warn("commercial Big Tech list configuration is invalid", "error", syncErr)
	} else {
		workerPool.RegisterHandler(federation.CommercialBigTechRefreshJobKind, func(jobCtx context.Context, _ string) error {
			state, _ := store.GetFederationPolicyState(jobCtx)
			if state == nil || !state.CommercialBigTechEnabled {
				return nil
			}
			var refreshErr error
			for attempt := 0; attempt < 3; attempt++ {
				if refreshErr = bigTechSyncer.Refresh(jobCtx); refreshErr == nil || jobCtx.Err() != nil {
					break
				}
			}
			if refreshErr != nil {
				slog.Warn("commercial Big Tech list refresh failed; keeping last valid snapshot", "error", refreshErr)
			}
			state, _ = store.GetFederationPolicyState(jobCtx)
			if state != nil && state.CommercialBigTechEnabled && cfg.BackgroundMaintenance {
				_ = store.EnqueueJob(jobCtx, &models.Job{Kind: federation.CommercialBigTechRefreshJobKind, Payload: "{}", RunAt: time.Now().Add(24 * time.Hour), MaxAttempts: 3, Priority: -10})
			}
			return nil
		})
		if cfg.BackgroundMaintenance {
			if state, stateErr := store.GetFederationPolicyState(ctx); stateErr == nil && state.CommercialBigTechEnabled {
				runAt := time.Now()
				if state.LastSuccess != nil && time.Since(*state.LastSuccess) < 24*time.Hour {
					runAt = state.LastSuccess.Add(24 * time.Hour)
				}
				_ = store.EnqueueJob(ctx, &models.Job{Kind: federation.CommercialBigTechRefreshJobKind, Payload: "{}", RunAt: runAt, MaxAttempts: 3, Priority: -10})
			}
		}
	}
	collectionSyncer := federation.NewCollectionSyncer(store, federator)
	workerPool.RegisterHandler(federation.CollectionSyncJobKind, collectionSyncer.ProcessJob)
	workerPool.RegisterHandler("poll_close", func(ctx context.Context, payload string) error {
		var v struct {
			PollID string `json:"poll_id"`
		}
		if err := json.Unmarshal([]byte(payload), &v); err != nil {
			return err
		}
		return store.ClosePoll(ctx, v.PollID)
	})

	var vapidKeys *push.VAPIDKeys
	if !cfg.SelfDestruct {
		vapidKeys, err = setupVAPIDKeys(ctx, store, cfg)
		if err != nil {
			slog.Warn("push: vapid key setup failed, push notifications disabled", "error", err)
		}
	}

	var pushHandler func(context.Context, string) error
	if vapidKeys != nil {
		pushHandler = push.HandlePushJob(ctx, store, push.NewSender(vapidKeys, "https://"+cfg.WebDomain), cfg.WebDomain)
	} else {
		pushHandler = func(context.Context, string) error { return nil }
	}

	lightTarget.RegisterHandler("push_notification", pushHandler)

	lightTarget.RegisterHandler("send_email", mail.HandleEmailJob(store, emailMailer))

	// Remote media cache
	// "off"   - never persist remote media (proxy/stream only)
	// "eager" - a background sweep caches all remote media shortly after ingest
	// "lazy"  - remote media is cached the first time it is accessed via the proxy
	cacheMode := cfg.Media.CacheMode()
	cacheMaxAge, _ := cfg.Media.CacheMaxAgeDuration() // validated in config.Load
	cachePath := cfg.Media.ResolvedCachePath()
	var mediaCache mediacache.Cacher = medialib.NewCache(cachePath, cfg.Media.MaxFileSize, cfg.Media.StripEXIF)
	var cacheObjectStore objectstore.Store = objectstore.NewLocal(cachePath, "/media/cache/")
	if cacheMode != config.CacheModeOff {
		var cacheErr error
		cacheObjectStore, cacheErr = objectstore.New(cfg.Media, true)
		if cacheErr != nil {
			slog.Error("media cache storage configuration failed", "error", cacheErr)
			os.Exit(1)
		}
		if cacheErr = cacheObjectStore.Check(ctx); cacheErr != nil {
			slog.Error("media cache storage unavailable", "error", cacheErr)
			os.Exit(1)
		}
		mediaPublicBases = append(mediaPublicBases, cacheObjectStore.URL("cache/probe"))
		if cacheObjectStore.Backend() != "local" {
			objectCache, cacheErr := medialib.NewObjectCache(cacheObjectStore, cfg.Media.MaxFileSize, cfg.Media.StripEXIF)
			if cacheErr != nil {
				slog.Error("initialize media object cache", "error", cacheErr)
				os.Exit(1)
			}
			mediaCache = objectCache
		}
		workerPool.RegisterHandler(mediacache.JobKind, func(ctx context.Context, payload string) error {
			return mediacache.Process(ctx, store, mediaCache, payload)
		})
	} else {
		// Drain jobs left by the previous configuration. Without a handler they
		// would remain ready forever and keep the queue in an unhealthy state.
		workerPool.RegisterHandler(mediacache.JobKind, func(context.Context, string) error { return nil })
	}

	// Eager cache sweep: periodically enqueue cache jobs for any remote media that
	// is not yet stored locally. This covers every ingestion path uniformly and
	// also caches existing media after switching the mode to "eager".
	if !cfg.SelfDestruct && cfg.BackgroundMaintenance && cacheMode == config.CacheModeEager {
		go runEagerCacheSweep(ctx, store)
	}

	// Queue depth is computed at /metrics scrape time - zero queries when
	// nobody scrapes (the common single-user case).
	metrics.SetQueueDepthFunc(func() int64 {
		count, err := store.CountPendingJobs(ctx)
		if err != nil {
			return -1
		}
		return int64(count)
	})

	houseInterval, err := time.ParseDuration(cfg.Housekeeping.Interval)
	if err != nil {
		houseInterval = 1 * time.Hour
	}

	if !cfg.SelfDestruct && cfg.Federation.ActorRefreshInterval > 0 {
		go actorRefresher.Run(ctx)
	} else {
		slog.Info("actor refresher disabled")
	}

	housekeeper := housekeeping.New(
		store,
		cfg.WebDomain,
		cfg.Media.Path,
		houseInterval,
		cfg.Housekeeping.PruneRemoteStatusesAfterDays,
		cfg.Housekeeping.PruneOwnStatusesAfterDays,
		cfg.Housekeeping.PruneOwnStatusesMinLikes,
		cfg.Housekeeping.PruneOwnStatusesMinBoosts,
		cfg.Housekeeping.TombstoneRetentionDays,
		cfg.Housekeeping.MaxDeletePerRun,
		cfg.Housekeeping.HomeFeedRetention,
		cacheMaxAge,
		cachePath,
		ownedMediaStore,
		cacheObjectStore,
	)
	housekeeper.SetCacheMaxBytes(cfg.Media.CacheMaxSizeBytes())

	workerPool.RegisterHandler(models.StatusExpireJobKind, func(ctx context.Context, payload string) error {
		var v struct {
			StatusID string `json:"status_id"`
		}
		if err := json.Unmarshal([]byte(payload), &v); err != nil {
			return err
		}
		return housekeeper.ExpireStatus(ctx, v.StatusID)
	})

	if !cfg.SelfDestruct && cfg.BackgroundMaintenance {
		go func() {
			// DB-driven media cleanup runs hourly regardless of housekeeping config
			mediaTicker := time.NewTicker(1 * time.Hour)
			defer mediaTicker.Stop()

			// The orphaned-media sweep walks the whole media directory; on SD-card
			// storage that's too heavy to repeat hourly when nothing changed.
			orphanTicker := time.NewTicker(24 * time.Hour)
			defer orphanTicker.Stop()

			// Status housekeeping runs on the configured interval
			houseTicker := time.NewTicker(houseInterval)
			defer houseTicker.Stop()

			// Delay the first run so a cold boot serves its first requests before
			// competing with prune/GC work.
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Minute):
				housekeeper.RunOnce(ctx)
			}

			for {
				select {
				case <-ctx.Done():
					return
				case <-mediaTicker.C:
					pruned, err := medialib.PruneStaleMedia(ctx, store, cfg.Media.Path, 24*time.Hour, ownedMediaStore)
					if err != nil {
						slog.Warn("media: stale media pruning failed", "error", err)
					} else if pruned > 0 {
						slog.Info("media: pruned stale unattached media", "count", pruned)
					}
				case <-orphanTicker.C:
					orphaned, err := medialib.RemoveOrphanedMedia(ctx, store, cfg.Media.Path, ownedMediaStore)
					if err != nil {
						slog.Warn("media: orphaned media removal failed", "error", err)
					} else if orphaned > 0 {
						slog.Info("media: removed orphaned media", "count", orphaned)
					}
				case <-houseTicker.C:
					housekeeper.RunOnce(ctx)
				}
			}
		}()
	}

	if !cfg.SelfDestruct && cfg.BackgroundMaintenance && cfg.Backup.Enabled {
		go runPeriodicBackup(ctx, store, cfg, cfg.WebDomain)
	}

	r := chi.NewRouter()

	r.Use(chimw.RequestID)

	trustedProxies, err := newTrustedProxySet(cfg.HTTP.Trusted)
	if err != nil {
		slog.Error("invalid trusted proxy configuration", "error", err)
		os.Exit(1)
	}
	if len(cfg.HTTP.Trusted) > 0 {
		slog.Info("trusted proxies configured", "proxies", cfg.HTTP.Trusted)
	}
	r.Use(realIPMiddleware(trustedProxies, pinnedMeshPeers(cfg.Mesh.Lighthouses)))
	if cfg.Observability.AccessLogEnabled() {
		r.Use(accessLogMiddleware)
	}
	r.Use(chimw.Recoverer)
	r.Use(corsMiddleware)
	r.Use(multipartCleanupMiddleware)
	r.Use(chimw.Timeout(60 * time.Second))
	r.Use(foregroundPriorityMiddleware(foregroundGate))
	if cfg.SelfDestruct {
		r.Use(selfDestructGate)
	}

	// Metrics middleware (count requests)
	r.Use(metricsMiddleware)

	r.Use(securityHeadersMiddleware)

	// Dynamic-response compression. The explicit content-type list keeps
	// media streams and text/event-stream (streaming API) uncompressed.
	// Everything else we serve as text belongs here: feeds and CSV exports are
	// the most compressible responses the server produces.
	if cfg.HTTP.GzipEnabled() {
		r.Use(chimw.Compress(4,
			"text/html",
			"text/css",
			"text/plain",
			"text/csv",
			"application/javascript",
			"application/json",
			"application/activity+json",
			"application/jrd+json",
			"application/rss+xml",
			"application/xml",
			"application/xrd+xml",
		))
	}

	// Rate limiting (simple token-bucket per IP)
	apiLimiter := newIPRateLimiter(cfg.RateLimits.API, time.Minute)
	r.Use(apiLimiter.middleware)
	inboxLimiter := newIPRateLimiter(cfg.RateLimits.Inbox, time.Minute)
	r.Use(limitInboxPOST(inboxLimiter))
	registrationLimiter := newIPRateLimiter(10, time.Hour)
	r.Use(limitPOSTPaths(registrationLimiter, "/auth/sign_up", "/api/v1/accounts"))
	// App registration is cheap and clients re-register on every failed login
	// attempt, so a tight limit turns one bad flow into an instance that looks
	// broken. Still bounded, to keep the table from being filled by a script.
	appRegistrationLimiter := newIPRateLimiter(100, time.Hour)
	r.Use(limitPOSTPaths(appRegistrationLimiter, "/api/v1/apps"))

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		if err := store.Ping(r.Context()); err != nil {
			slog.Error("health check failed", "error", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"status":"unhealthy"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"healthy"}`))
	})

	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := store.Ping(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"status":"not ready"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ready"}`))
	})

	if cfg.Observability.MetricsEnabled {
		r.Get("/metrics", metrics.Handler().ServeHTTP)
	}

	// Remote media proxy: stream remote media through this instance so local
	// users never connect to the originating remote server (privacy). When
	// disabled, media URLs are served unchanged and clients hit the remote
	// instance directly.
	mediaProxy := medialib.NewProxy(cfg.Media.ProxyRemote, cfg.Media.ProxySecret, cfg.WebDomain)
	for _, base := range mediaPublicBases {
		mediaProxy.AddLocalMediaBase(base)
	}
	// Lazy cache mode hooks the proxy: the first time an item is proxied, a
	// background job stores it locally so subsequent loads are served from cache.
	if cacheMode == config.CacheModeLazy {
		if mediaProxy.Enabled() {
			mediaProxy.SetCacheHook(func(remoteURL string) {
				if _, err := mediacache.Enqueue(context.Background(), store, remoteURL); err != nil {
					slog.Warn("media: failed to enqueue lazy cache job", "url", remoteURL, "error", err)
				}
			})
		} else {
			slog.Warn("media: cache_remote=lazy has no effect while proxy_remote is disabled; use eager mode or enable proxy_remote")
		}
	}
	if mediaProxy.Enabled() {
		// Enforce reject_media and suspend-severity domain blocks at the proxy.
		mediaProxy.SetRejectHook(func(host string) bool {
			if host == "" {
				return false
			}
			block, err := store.GetDomainBlock(context.Background(), strings.ToLower(host))
			if err != nil || block == nil {
				return false
			}
			return block.RejectMedia || block.Severity == "suspend"
		})
		r.Get("/proxy/media", mediaProxy.Handler().ServeHTTP)
		r.Head("/proxy/media", mediaProxy.Handler().ServeHTTP)
		r.Get("/proxy/media/{filename}", mediaProxy.Handler().ServeHTTP)
		r.Head("/proxy/media/{filename}", mediaProxy.Handler().ServeHTTP)
		slog.Info("media: remote media proxy enabled", "cache_mode", cacheMode)
	}

	fedHandler := federation.NewHandlerWithDomains(store, cfg.LocalDomain, cfg.WebDomain, verifier, federator)
	fedHandler.SetResponseCacheOptions(federationResponseCache, federationResponseTTL, discoveryTTL, cfg.Cache.MaxFederationObjectKB*1024)
	fedHandler.SetOpenRegistrations(cfg.Auth.Registrations)
	fedHandler.SetInstanceMetadata(cfg.Instance.Name, instancemeta.Meta{
		ShortDescription: cfg.Instance.ShortDescription,
		Description:      cfg.Instance.Description,
	})
	fedHandler.SetSecureMode(cfg.Federation.SecureMode)
	fedHandler.RegisterRoutes(r)

	apiConfig := api.Config{
		Domain:                      cfg.LocalDomain,
		LocalDomain:                 cfg.LocalDomain,
		WebDomain:                   cfg.WebDomain,
		Secure:                      cfg.Federation.SecureMode,
		UserAgent:                   cfg.Federation.UserAgent,
		MediaPath:                   cfg.Media.Path,
		MaxMediaSize:                cfg.Media.MaxFileSize,
		MaxMedia:                    cfg.Media.MaxMedia,
		RenameMedia:                 cfg.Media.RenameMedia,
		StripEXIF:                   cfg.Media.StripEXIF,
		MigrationConfigured:         true,
		MigrationEnabled:            cfg.Migration.Enabled,
		MigrationMaxFollowsImport:   cfg.Migration.MaxFollowsImport,
		MigrationImportRate:         cfg.Migration.ImportRate,
		MigrationCooldownAfterMove:  cfg.Migration.CooldownAfterMove,
		MigrationRequireTargetAlias: cfg.Migration.RequireTargetAlias,
		MediaProxy:                  mediaProxy,
		MediaStore:                  ownedMediaStore,
		InstanceName:                cfg.Instance.Name,
		MaxStatusChars:              cfg.Instance.MaxStatusChars,
		InstanceRules:               cfg.Instance.Rules,
		InstanceShortDescription:    cfg.Instance.ShortDescription,
		InstanceDescription:         cfg.Instance.Description,
		InstanceExtendedDescription: cfg.Instance.ExtendedDescription,
		InstanceTermsOfService:      cfg.Instance.TermsOfService,
		InstanceContactEmail:        cfg.Instance.ContactEmail,
		DefaultLocale:               cfg.Web.DefaultLocale,
		BlogAllAccounts:             cfg.Blog.AllAccounts,
	}
	apiConfig.BlockSensitiveRelationshipNotifications = cfg.Instance.BlockSensitiveRelationshipNotifications
	apiHandler := api.NewHandler(store, federator, apiConfig, threadResolver)
	apiHandler.SetInboxProcessor(inboxProcessor)
	// Set multi-user configuration on the API handler
	apiHandler.Registrations = cfg.Auth.Registrations
	apiHandler.InviteRequired = cfg.Auth.InviteRequired
	apiHandler.RequireApproval = cfg.Auth.RequireApproval
	apiHandler.RequireEmail = cfg.Auth.RequireEmail
	apiHandler.RequireAdminMFA = cfg.Auth.RequireAdminMFA
	apiHandler.MinPasswordLength = cfg.Auth.MinPasswordLength
	apiHandler.ContactUsername = cfg.ContactUsername
	apiHandler.SetPageCache(pageCache, pageTTL, componentTTL)
	apiHandler.SetServeStale(cfg.Cache.ServeStale)
	// Wire the mailer (shared with the send_email job handler) to both frontends
	mailer := emailMailer
	apiHandler.SetMailer(mailer)
	vapidPublic := ""
	if vapidKeys != nil {
		vapidPublic = vapidKeys.Public
	}
	webHandler := webui.NewHandlerWithDomains(store, federator, cfg, cfg.LocalDomain, cfg.WebDomain, cfg.SingleUser, cfg.Web.Password, cfg.Media.Path, cfg.Media.MaxFileSize, cfg.Media.StripEXIF, cfg.Media.RenameMedia, housekeeper, mediaProxy, threadResolver)
	webHandler.SetMailer(mailer)
	webHandler.SetVAPIDPublicKey(vapidPublic)
	webHandler.SetMediaStore(ownedMediaStore)
	webHandler.SetInboxProcessor(inboxProcessor)
	webHandler.SetPageCache(pageCache, pageTTL, componentTTL, cardTTL)
	webHandler.SetServeStale(cfg.Cache.ServeStale)
	oauthHandler := oauth.NewHandlerWithVAPID(store, cfg.WebDomain, vapidPublic)
	oauthHandler.SetAuthenticationPolicy(cfg.Auth.RequireEmail, cfg.Auth.RequireAdminMFA)
	oauthHandler.SetConsentRenderer(func(w http.ResponseWriter, req *http.Request, data oauth.ConsentPageData) {
		webHandler.RenderOAuthAuthorize(w, req, webui.OAuthAuthorizePage{
			AppName: data.AppName, AppWebsite: data.AppWebsite, Username: data.Username,
			ClientID: data.ClientID, RedirectURI: data.RedirectURI,
			ResponseType: data.ResponseType, Scope: data.Scope, State: data.State,
			CodeChallenge: data.CodeChallenge, CodeChallengeMethod: data.CodeChallengeMethod,
			CSRFToken: data.CSRFToken, Scopes: data.Scopes,
			AllowURL: template.URL(data.AllowURL), DenyURL: template.URL(data.DenyURL),
		})
	})
	apiHandler.RegisterRoutes(r, oauthHandler)
	workerPool.RegisterHandler("scheduled_status", apiHandler.PublishScheduledStatus)
	workerPool.RegisterHandler("backup", webui.HandleBackupJob(store, cfg, cfg.WebDomain))
	workerPool.RegisterHandler(models.AccountExportJobKind, app.HandleAccountExportJob(store, ownedMediaStore))
	workerPool.RegisterHandler(models.AccountExportCleanupJobKind, app.HandleAccountExportCleanupJob(store, ownedMediaStore))
	workerPool.RegisterHandler(models.AccountImportJobKind, app.HandleAccountImportJob(store, ownedMediaStore, federator, cfg.WebDomain))
	workerPool.RegisterHandler(models.FollowImportJobKind, app.HandleFollowImportJob(store, federator, cfg.LocalDomain, cfg.WebDomain, cfg.Migration.ImportRate))
	workerPool.RegisterHandler(models.NotificationCreatedJobKind, notificationCreatedHandler(store, streamBroker))
	workerPool.RegisterHandler(webui.OutboxFetchJobKind, webui.HandleOutboxFetchJob(store, federator, inboxProcessor))

	// Now that the API handler (the streaming serializer) exists, finish wiring
	// the streaming fan-out: give the router its serializer and register the
	// streaming publisher on the API and web UI services so locally created
	// statuses and notifications are streamed live. The HTTP server is not yet
	// listening, so these assignments race with nothing.
	streamRouter.SetSerializer(apiHandler)
	streamRouter.SetHydrator(apiHandler)

	// Optional proactive cache warmer: when new statuses/notifications arrive,
	// a single low-priority background worker re-primes recently-active
	// viewers' timeline/component/notification caches so their next page open
	// is a hit instead of a rebuild - the visible delay on low-power devices.
	// The warm service shares the decorated store and the page cache with the
	// API and web UI handlers, so the entries it populates are exactly the
	// ones they read.
	var warmer *app.Warmer
	if !cfg.SelfDestruct && cfg.Cache.Enabled && cfg.Cache.WarmTimelines {
		warmSvc := app.NewWithDomainsAndMediaPath(store, federator, cfg.LocalDomain, cfg.WebDomain, cfg.Media.Path)
		warmSvc.SetMediaStore(ownedMediaStore)
		warmSvc.SetPageCache(pageCache, pageTTL, componentTTL)
		activeViewerTTL, _ := cfg.Cache.ActiveViewerTTLDuration()
		warmDebounce, _ := cfg.Cache.WarmDebounceDuration()
		warmer = app.NewWarmer(warmSvc, app.WarmerOptions{
			MaxViewers:      cfg.Cache.WarmMaxViewers,
			ActiveViewerTTL: activeViewerTTL,
			Debounce:        warmDebounce,
		})
		warmer.SetForegroundGate(foregroundGate)
		apiHandler.SetViewerTouch(warmer.TouchPath)
		webHandler.SetViewerTouch(warmer.TouchPath)
		go warmer.Run(ctx)
		// Boot warm: viewer tracking is in-memory, so after a restart nobody
		// would be warmed until their first (cold, multi-second) visit. Seed
		// the tracker with every local account and trigger one pass so the
		// timeline windows and components are rebuilt in the background while
		// the instance is still settling. Delayed so startup queue recovery
		// gets the core first.
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
			locals, err := store.GetLocalAccounts(ctx)
			if err != nil {
				slog.Warn("boot warm: list local accounts failed", "error", err)
				return
			}
			for _, a := range locals {
				warmer.Touch(a.ID)
			}
			warmer.Notify("status.created", "", "")
			warmer.Notify("notification.created", "", "")
			slog.Info("boot warm scheduled", "viewers", len(locals))
		}()
		slog.Info("cache warmer enabled",
			"max_viewers", cfg.Cache.WarmMaxViewers,
			"active_viewer_ttl", activeViewerTTL, "debounce", warmDebounce)
	}

	if err := streamBroker.Start(ctx, func(eventCtx context.Context, event streaming.BrokerEvent) {
		// PostgreSQL NOTIFY carries status/notification events to every process.
		// Clearing the shared membership/federation-response cache here makes
		// process-local RAM caches coherent; missed notifications remain bounded
		// by the hard timeline TTL and always fall back to authoritative SQL.
		crossProcess := strings.EqualFold(cfg.Streaming.Backend, "postgres")
		if event.Kind == "federation.policy.invalidate" {
			store.InvalidateFederationPolicy()
			return
		}
		invalidateAll := crossProcess && event.Kind == "cache.invalidate"
		var eventStatus *models.Status
		if strings.HasPrefix(event.Kind, "status.") && event.ID != "" {
			eventStatus, _ = store.GetStatus(eventCtx, event.ID)
		}
		if federationResponseCache != nil && (invalidateAll || crossProcess && strings.HasPrefix(event.Kind, "status.")) {
			federationResponseCache.DeletePrefix("fed:")
		}
		if timelineCache != nil && (invalidateAll || crossProcess && event.Kind != "status.created" && strings.HasPrefix(event.Kind, "status.")) {
			timelineCache.Clear()
		}
		if crossProcess && event.Kind == "status.created" && eventStatus != nil {
			if observer, ok := store.(interface {
				TimelineStatusCreated(context.Context, *models.Status)
			}); ok {
				observer.TimelineStatusCreated(eventCtx, eventStatus)
			}
		}
		if entityCache != nil && invalidateAll {
			entityCache.Clear()
		}
		if relInvalidator != nil && invalidateAll {
			relInvalidator.InvalidateRelationshipCaches()
		}
		// Page snapshots validate themselves against the timeline generation
		// counters on read (see app.Service.PageCached), so status events need
		// no eager page wipe when the timeline decorator is active - the
		// generation already advanced inside CreateStatus (local broker) or via
		// TimelineStatusCreated above (postgres). Only stores without generation
		// tracking keep the old eager invalidation.
		_, generationTracked := store.(interface {
			TimelineGenerations() (general, home uint64)
		})
		if pageCache != nil && invalidateAll {
			pageCache.Clear()
		} else if pageCache != nil {
			if crossProcess && event.Kind == "cache.invalidate" {
				// Cross-process coherence only. On the local broker the write
				// that emitted this already invalidated its own viewer's pages
				// synchronously; every inbound notification also emits it, so
				// honoring it here wiped all page snapshots (and fenced any
				// build in flight) on routine federation traffic.
				pageCache.DeleteDependencies(app.PageCacheDependency)
				pageCache.DeleteDependencies(app.HomePageCacheDependency)
			} else if strings.HasPrefix(event.Kind, "status.") && !generationTracked {
				affectsHome := true
				if event.Kind == "status.created" && eventStatus != nil {
					if relevant, relevanceErr := store.StatusAffectsHomeTimeline(eventCtx, eventStatus); relevanceErr == nil {
						affectsHome = relevant
					}
				}
				pageCache.DeleteDependencies(app.PageCacheDependency)
				if affectsHome {
					pageCache.DeleteDependencies(app.HomePageCacheDependency)
				}
			}
			// Only edits and deletes can invalidate cached artifacts that embed
			// the status. A status.created has no dependents yet, and wiping on
			// it would advance the cache version on every arrival, fencing out
			// concurrent page/component stores under routine federation traffic.
			if (event.Kind == "status.updated" || event.Kind == "status.deleted") && event.ID != "" {
				pageCache.DeleteDependencies("status:" + event.ID)
			}
		}
		streamRouter.HandleBrokerEvent(eventCtx, event)
		// Warm after invalidation and live fan-out so the debounced pass
		// rebuilds fresh entries rather than re-serving what was just dropped.
		// Notify never blocks: the local broker publishes synchronously inside
		// the status-creation path.
		if warmer != nil {
			warmer.Notify(event.Kind, event.ID, event.AccountID)
		}
	}); err != nil {
		slog.Error("streaming broker startup failed", "backend", cfg.Streaming.Backend, "error", err)
		os.Exit(1)
	}
	inboxProcessor.SetStreamPublisher(streamPublisher)
	apiHandler.SetStreamPublisher(streamPublisher)
	webHandler.SetStreamPublisher(streamPublisher)
	streaming.NewHandler(streamHub, store, cfg.Auth.RequireEmail, cfg.Auth.RequireAdminMFA).RegisterRoutes(r)
	slog.Info("streaming API enabled", "endpoint", "/api/v1/streaming", "broker", cfg.Streaming.Backend)

	// Start workers only after all event consumers are ready; otherwise inbox
	// jobs recovered at startup could publish into an uninitialized stream.
	workerPool.Start(ctx)
	defer workerPool.Stop()
	if lightPool != nil {
		lightPool.Start(ctx)
		defer lightPool.Stop()
	}

	webHandler.RegisterRoutes(r)

	// Media serving. Cached remote media is served from its own (possibly
	// separate) directory at the /media/cache/ prefix; this static route takes
	// precedence over the general /media/{id}/* route below. When a cached file
	// is missing (e.g. the cache was cleared on disk, or a stale DB reference),
	// fall back to proxying the origin instead of returning a 404, and reset the
	// referencing rows so the media is re-cached.
	if cachePath != "" {
		r.Get("/media/cache/*", func(w http.ResponseWriter, req *http.Request) {
			rel := strings.TrimPrefix(req.URL.Path, "/media/cache/")
			if rel == "" || strings.Contains(rel, "..") {
				http.NotFound(w, req)
				return
			}
			fullPath, ok := medialib.OwnedLocalPath(cachePath, "/media/"+rel)
			if !ok {
				http.NotFound(w, req)
				return
			}
			if info, err := os.Stat(fullPath); err == nil && !info.IsDir() {
				// Content-addressed remote media cache URLs are immutable:
				// the file at a given URL can never change, so the browser and
				// any fronting cache may hold it indefinitely.
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				http.ServeFile(w, req, fullPath)
				return
			}
			// Missing cache file: recover the origin URL and proxy it.
			remote, err := store.ReconcileMissingCacheURL(req.Context(), req.URL.Path)
			if err != nil || remote == "" {
				http.NotFound(w, req)
				return
			}
			parsedRemote, err := url.Parse(remote)
			if err != nil || parsedRemote.User != nil || parsedRemote.Host == "" ||
				(parsedRemote.Scheme != "http" && parsedRemote.Scheme != "https") {
				http.NotFound(w, req)
				return
			}
			target := parsedRemote.String()
			if mediaProxy.Enabled() {
				target = mediaProxy.URL(target)
			}
			http.Redirect(w, req, target, http.StatusFound)
		})
	}
	mediaPath := cfg.Media.Path
	if mediaPath != "" {
		r.Handle("/media/{id}/*", localMediaHandler(mediaPath))
	}

	// Optional pprof profiler on its own listener.
	if cfg.Observability.PprofListen != "" {
		startPprofServer(cfg.Observability.PprofListen)
	}

	newServer := func(addr string) *http.Server {
		return &http.Server{
			Addr:              addr,
			Handler:           r,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    1 << 20,
		}
	}
	srv := newServer(cfg.HTTP.Listen)

	go func() {
		slog.Info("listening", "addr", cfg.HTTP.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// Inbound federation over littleMesh is the same server on a different
	// listener: same router, same middleware, same ActivityPub routes. A mesh
	// circuit is just a net.Conn.
	if meshNode != nil {
		meshSrv := newServer("")
		meshFullAccess := make(map[string]bool, len(cfg.Mesh.FullAccess))
		for _, nodeID := range cfg.Mesh.FullAccess {
			meshFullAccess[strings.ToLower(nodeID)] = true
		}
		meshSrv.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			peer, _ := req.Context().Value(meshPeerContextKey{}).(string)
			if !mesh.RequestAllowed(cfg.Mesh.Exposure, meshFullAccess[strings.ToLower(peer)], req) {
				http.NotFound(w, req)
				return
			}
			r.ServeHTTP(w, req)
		})
		meshSrv.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
			if peer, ok := mesh.PeerOf(conn); ok {
				return context.WithValue(ctx, meshPeerContextKey{}, peer)
			}
			return ctx
		}
		go func() {
			slog.Info("listening on the mesh", "domain", cfg.WebDomain)
			if err := meshSrv.Serve(meshNode.Listener()); err != nil && err != http.ErrServerClosed {
				slog.Error("mesh server error", "error", err)
			}
		}()
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	slog.Info("shutting down", "signal", sig)
	cancelApp()

	// Give outstanding requests 30s to complete
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	workerPool.Stop()
	if lightPool != nil {
		lightPool.Stop()
	}

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("forced shutdown", "error", err)
	}

	slog.Info("shutdown complete")
}

// foregroundPriorityMiddleware marks browser and Mastodon API requests as
// interactive work. Federation/discovery/media/streaming traffic stays in the
// background class and therefore cannot starve a user request on small hosts.
func foregroundPriorityMiddleware(gate *queue.ForegroundGate) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isForegroundRequest(r) {
				if isPreemptibleBackgroundHTTP(r.URL.Path) {
					ctx, cancel := gate.BackgroundContext(r.Context(), true)
					defer cancel()
					next.ServeHTTP(w, r.WithContext(ctx))
				} else {
					next.ServeHTTP(w, r)
				}
				return
			}
			leave := gate.Enter()
			defer leave()
			next.ServeHTTP(w, r)
		})
	}
}

// neutralPathPrefixes are neither foreground nor preemptible: federation and
// asset traffic that must not hold the gate open, and must not be cancelled
// under it either. Cancelling a media fetch mid-flight is a broken image on
// the page that asked for it, not work that can be retried later.
var neutralPathPrefixes = []string{
	"/inbox", "/users/", "/objects/", "/.well-known/", "/nodeinfo/",
	"/media/", "/proxy/", "/emoji/", "/static/", "/robots.txt", "/health", "/readyz", "/metrics",
}

func isPreemptibleBackgroundHTTP(path string) bool {
	if strings.HasPrefix(path, "/api/v1/streaming") {
		return false
	}
	if strings.HasPrefix(path, "/api/") {
		return true
	}
	for _, prefix := range neutralPathPrefixes {
		if strings.HasPrefix(path, prefix) {
			return false
		}
	}
	// Anonymous HTML pages are crawler/public work and may yield to a logged-in
	// browser request on resource-constrained single-user instances.
	return true
}

func isForegroundRequest(r *http.Request) bool {
	path := r.URL.Path
	if strings.HasPrefix(path, "/api/v1/streaming") {
		return false
	}
	if strings.HasPrefix(path, "/api/") {
		return strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Authorization"))), "bearer ")
	}
	if strings.HasPrefix(path, "/oauth/") || strings.HasPrefix(path, "/auth/") {
		return true
	}
	for _, prefix := range neutralPathPrefixes {
		if strings.HasPrefix(path, prefix) {
			return false
		}
	}
	_, err := r.Cookie("session")
	return err == nil
}

// validatePersistedWebDomain prevents an unsafe WEB_DOMAIN change from making
// served actor documents disagree with the identifiers and signing keys stored
// in the database. LOCAL_DOMAIN may differ because it is a handle namespace.
func validatePersistedWebDomain(ctx context.Context, store storage.Storage, webDomain string) error {
	accounts, err := store.ListLocalIdentities(ctx)
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}
	for _, account := range accounts {
		if account.ActorURI == "" {
			continue
		}
		u, err := url.Parse(account.ActorURI)
		if err != nil || !strings.EqualFold(u.Host, webDomain) {
			return fmt.Errorf("local actor %q is stored on %q, not configured web_domain %q; changing web_domain after creating accounts is unsafe", account.Username, u.Host, webDomain)
		}
	}
	return nil
}

// extractCLICommand scans os.Args for a CLI subcommand (admin, migrate, post, help)
// while handling global flags like --config that may appear before it.
// Returns the command name, the remaining args after the command, and the config path.
func extractCLICommand(args []string) (cmd string, cmdArgs []string, configPath string) {
	// Scan args from index 1 (program name is index 0)
	// Skip --config <value> pairs, stop at first recognized command
	i := 1
	for i < len(args) {
		a := args[i]
		switch a {
		case "--config", "-config":
			if i+1 < len(args) {
				configPath = args[i+1]
				i += 2
				continue
			}
			i++
			continue
		case "--help", "-h":
			return "help", nil, configPath
		case "--version":
			return "version", nil, configPath
		case "init", "admin", "migrate", "post", "backup", "mesh", "lighthouse", "directory", "help", "version":
			return a, args[i+1:], configPath
		default:
			if strings.HasPrefix(a, "--config=") || strings.HasPrefix(a, "-config=") {
				configPath = strings.SplitN(a, "=", 2)[1]
				i++
				continue
			}
			// Skip unknown flags (they'll be handled later if we're not in CLI mode)
			if len(a) > 0 && a[0] == '-' {
				i++
				continue
			}
			// Not a CLI mode invocation
			return "", nil, ""
		}
	}
	return "", nil, ""
}

// runEagerCacheSweep periodically finds remote media that has not been cached
// locally and enqueues background cache jobs for it. This is the engine of the
// "eager" cache mode: it caches all remote media shortly after it is received,
// independent of the ingestion path, and also backfills existing media after
// the mode is switched on.
func runEagerCacheSweep(ctx context.Context, store storage.Storage) {
	const (
		sweepInterval    = 30 * time.Second
		sweepIntervalMax = 10 * time.Minute
		batchLimit       = 200
	)

	sweep := func() int {
		// Expired negative-cache entries (permanently-dead URLs to retry) are pruned
		// by the housekeeper, which runs in every cache mode.

		// De-duplicate by remote URL so we enqueue one job per distinct file.
		seen := make(map[string]struct{})
		enqueued := 0
		enqueue := func(remoteURL string) {
			if remoteURL == "" {
				return
			}
			if _, ok := seen[remoteURL]; ok {
				return
			}
			seen[remoteURL] = struct{}{}
			ok, err := mediacache.Enqueue(ctx, store, remoteURL)
			if err != nil {
				slog.Warn("media: failed to enqueue eager cache job", "url", remoteURL, "error", err)
				return
			}
			if ok {
				enqueued++
			}
		}

		// Status attachments.
		media, err := store.GetUncachedRemoteMedia(ctx, batchLimit)
		if err != nil {
			slog.Warn("media: eager cache sweep query failed", "error", err)
		} else {
			for _, m := range media {
				enqueue(m.RemoteURL)
			}
		}

		// Profile pictures and headers.
		images, err := store.GetUncachedRemoteAccountImageURLs(ctx, batchLimit)
		if err != nil {
			slog.Warn("media: eager cache sweep (account images) query failed", "error", err)
		} else {
			for _, u := range images {
				enqueue(u)
			}
		}

		// Custom emoji images.
		emojis, err := store.GetUncachedRemoteEmoji(ctx, batchLimit)
		if err != nil {
			slog.Warn("media: eager cache sweep (emojis) query failed", "error", err)
		} else {
			for _, e := range emojis {
				enqueue(e.RemoteURL)
			}
		}

		if enqueued > 0 {
			slog.Info("media: eager cache sweep enqueued jobs", "count", enqueued)
		}
		return enqueued
	}

	// Run once shortly after startup, then adaptively: three full uncached-media
	// queries every 30s are wasted work on a quiet instance, so idle passes back
	// off (doubling up to 10 min) and any pass that finds work snaps back to the
	// base interval. Eager caching still reacts within 30s while media flows.
	interval := sweepInterval
	if sweep() == 0 {
		interval *= 2
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if sweep() > 0 {
				interval = sweepInterval
			} else if interval < sweepIntervalMax {
				interval *= 2
				if interval > sweepIntervalMax {
					interval = sweepIntervalMax
				}
			}
			timer.Reset(interval)
		}
	}
}

func runPeriodicBackup(ctx context.Context, store storage.Storage, cfg *config.Config, domain string) {
	interval := cfg.Backup.IntervalDuration()
	path := cfg.Backup.ResolvedPath()

	slog.Info("backup: periodic backup enabled", "interval", interval, "path", path)

	nextRun := func() time.Time {
		if t, ok := backup.LastBackupTime(path); ok {
			due := t.Add(interval)
			if due.After(time.Now()) {
				slog.Info("backup: next backup scheduled based on last run",
					"last", t, "due", due)
				return due
			}
		}
		return time.Now()
	}()

	for {
		delay := time.Until(nextRun)
		if delay > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}

		select {
		case <-ctx.Done():
			return
		default:
		}

		backupPath, err := backup.CreateBackup(ctx, store, cfg)
		if err != nil {
			slog.Error("backup: periodic backup failed", "error", err)
			mail.NotifyAdminsBackup(ctx, store, domain, false, "")
		} else {
			mail.NotifyAdminsBackup(ctx, store, domain, true, backupPath)
		}

		nextRun = time.Now().Add(interval)
	}
}

// metricsMiddleware counts requests by path prefix.
func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		path := r.URL.Path
		class := "federation"
		switch {
		case strings.HasPrefix(path, "/api/v"):
			class = "api"
			metrics.IncrementAPIRequests()
		case strings.HasPrefix(path, "/inbox") || strings.HasPrefix(path, "/users/"):
			class = "inbox"
			metrics.IncrementInboxRequests()
		case strings.HasPrefix(path, "/auth") || strings.HasPrefix(path, "/settings") || strings.HasPrefix(path, "/notifications") || path == "/" || strings.HasPrefix(path, "/public") || strings.HasPrefix(path, "/@") || path == "/about" || path == "/terms":
			class = "web"
			metrics.IncrementWebRequests()
		default:
			metrics.IncrementFedRequests()
		}
		next.ServeHTTP(w, r)
		metrics.ObserveRequestDuration(class, time.Since(started))
	})
}

// corsMiddleware adds CORS headers so web-based clients (Phanpy, Enafore, etc.)
// can call the API from their own domain.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Max-Age", "86400")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// multipartCleanupMiddleware removes temporary files created by
// ParseMultipartForm after every request, including error and panic paths.
func multipartCleanupMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// cspCommonDirectives are the CSP directives shared by every response.
const cspCommonDirectives = "style-src-attr 'unsafe-inline'; " +
	"img-src 'self' data: https:; " +
	"media-src 'self' https:; " +
	"connect-src 'self' https: ws: wss:; " +
	"frame-ancestors 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self';"

// noncelessCSP is the constant policy for routes that never render HTML
// templates: no inline scripts/styles execute there, so no nonce source is
// needed and the per-request random nonce can be skipped entirely.
const noncelessCSP = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self'; " +
	cspCommonDirectives

// noncelessCSPPath reports whether the request path serves only static files,
// proxied media, or JSON, never an HTML template, so the constant CSP
// applies. Keep this list conservative: anything HTML-rendering (including
// /oauth/authorize) must keep the nonce.
func noncelessCSPPath(path string) bool {
	return strings.HasPrefix(path, "/static/") ||
		strings.HasPrefix(path, "/media/") ||
		strings.HasPrefix(path, "/emoji/") ||
		strings.HasPrefix(path, "/api/") ||
		path == "/sw.js"
}

// securityHeadersMiddleware adds security-related HTTP headers to all responses.
func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")

		if noncelessCSPPath(r.URL.Path) {
			w.Header().Set("Content-Security-Policy", noncelessCSP)
		} else {
			nonceBytes := make([]byte, 18)
			if _, err := rand.Read(nonceBytes); err != nil {
				slog.Error("failed to generate CSP nonce", "error", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
			r.Header.Set("X-littleFedi-CSP-Nonce", nonce)
			w.Header().Set("Content-Security-Policy",
				"default-src 'self'; "+
					"script-src 'self' 'nonce-"+nonce+"'; "+
					"style-src 'self' 'nonce-"+nonce+"'; "+
					cspCommonDirectives)
		}

		// Add HSTS when behind HTTPS (reverse proxy sets X-Forwarded-Proto)
		if r.Header.Get("X-Forwarded-Proto") == "https" || r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}

		next.ServeHTTP(w, r)
	})
}

// ipRateLimiter implements a simple in-memory per-IP token-bucket rate limiter.
type ipRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rateBucket
	limit   int
	window  time.Duration
}

type rateBucket struct {
	tokens   float64
	lastSeen time.Time
}

type trustedProxySet struct {
	networks []*net.IPNet
}

type meshPeerContextKey struct{}

func pinnedMeshPeers(entries []string) map[string]bool {
	peers := make(map[string]bool)
	for _, entry := range entries {
		_, pin, found := strings.Cut(entry, "@")
		pin = strings.ToLower(strings.TrimSpace(pin))
		if found && mesh.ValidNodeID(pin) {
			peers[pin] = true
		}
	}
	return peers
}

func newTrustedProxySet(entries []string) (*trustedProxySet, error) {
	set := &trustedProxySet{}
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if ip := net.ParseIP(entry); ip != nil {
			bits := 128
			if ip.To4() != nil {
				ip = ip.To4()
				bits = 32
			}
			set.networks = append(set.networks, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, network, err := net.ParseCIDR(entry)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: %w", entry, err)
		}
		set.networks = append(set.networks, network)
	}
	return set, nil
}

func (s *trustedProxySet) contains(ip net.IP) bool {
	if s == nil || ip == nil {
		return false
	}
	for _, network := range s.networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func remoteIP(remoteAddr string) net.IP {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return net.ParseIP(host)
	}
	return net.ParseIP(strings.Trim(remoteAddr, "[]"))
}

// clientIP trusts forwarding headers only while walking through configured
// proxy hops from right to left. The first untrusted hop is the client identity;
// headers sent directly by untrusted peers are ignored.
func clientIP(r *http.Request, trusted *trustedProxySet) string {
	return clientIPFrom(r, trusted, false)
}

func clientIPFrom(r *http.Request, trusted *trustedProxySet, trustImmediate bool) string {
	immediate := remoteIP(r.RemoteAddr)
	if immediate == nil && !trustImmediate {
		return r.RemoteAddr
	}
	client := immediate
	trustedHop := trustImmediate || trusted.contains(immediate)
	if trustedHop {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			parts := strings.Split(forwarded, ",")
			for i := len(parts) - 1; i >= 0 && trustedHop; i-- {
				candidate := net.ParseIP(strings.TrimSpace(parts[i]))
				if candidate == nil {
					break
				}
				client = candidate
				trustedHop = trusted.contains(client)
			}
		} else if candidate := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); candidate != nil {
			client = candidate
		}
	}
	if client == nil {
		return r.RemoteAddr
	}
	return client.String()
}

func realIPMiddleware(trusted *trustedProxySet, trustedMeshPeers map[string]bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			trustImmediate := trusted.contains(remoteIP(r.RemoteAddr))
			if peer, ok := r.Context().Value(meshPeerContextKey{}).(string); ok {
				trustImmediate = trustedMeshPeers[strings.ToLower(peer)]
			}
			if !trustImmediate {
				r.Header.Del("X-Forwarded-For")
				r.Header.Del("X-Real-IP")
				r.Header.Del("X-Forwarded-Proto")
			}
			r.RemoteAddr = clientIPFrom(r, trusted, trustImmediate)
			next.ServeHTTP(w, r)
		})
	}
}

func limitPOSTPaths(limiter *ipRateLimiter, paths ...string) func(http.Handler) http.Handler {
	limited := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		limited[path] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		guarded := limiter.middleware(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				if _, ok := limited[r.URL.Path]; ok {
					guarded.ServeHTTP(w, r)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func limitInboxPOST(limiter *ipRateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		guarded := limiter.middleware(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			isUserInbox := strings.HasPrefix(r.URL.Path, "/users/") && strings.HasSuffix(r.URL.Path, "/inbox")
			if r.Method == http.MethodPost && (r.URL.Path == "/inbox" || isUserInbox) {
				guarded.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func newIPRateLimiter(limit int, window time.Duration) *ipRateLimiter {
	l := &ipRateLimiter{
		buckets: make(map[string]*rateBucket),
		limit:   limit,
		window:  window,
	}
	// Background cleanup of stale entries
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			l.mu.Lock()
			cutoff := time.Now().Add(-window)
			for ip, b := range l.buckets {
				if b.lastSeen.Before(cutoff) {
					delete(l.buckets, ip)
				}
			}
			l.mu.Unlock()
		}
	}()
	return l
}

func (l *ipRateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if l.limit <= 0 {
			next.ServeHTTP(w, r)
			return
		}

		ip := clientIP(r, nil)

		l.mu.Lock()
		now := time.Now()
		b, ok := l.buckets[ip]
		if !ok {
			b = &rateBucket{tokens: float64(l.limit), lastSeen: now}
			l.buckets[ip] = b
		}

		// Refill tokens proportional to elapsed time
		elapsed := now.Sub(b.lastSeen).Seconds()
		refillRate := float64(l.limit) / l.window.Seconds()
		b.tokens += elapsed * refillRate
		if b.tokens > float64(l.limit) {
			b.tokens = float64(l.limit)
		}
		b.lastSeen = now

		if b.tokens < 1 {
			l.mu.Unlock()
			w.Header().Set("Retry-After", fmt.Sprintf("%.0f", l.window.Seconds()))
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		b.tokens--
		l.mu.Unlock()

		next.ServeHTTP(w, r)
	})
}

// printUsage prints the top-level usage summary.
func printUsage() {
	fmt.Fprintf(os.Stderr, `littleFedi - a minimalist ActivityPub server

Usage:
  littlefedi [--config <path>]              Start the server
  littlefedi init [--config <path>]        Interactively create an instance or littleMesh lighthouse
  littlefedi [--config <path>] admin ...    Run admin CLI commands
  littlefedi [--config <path>] backup ...   Run backup and restore commands
  littlefedi [--config <path>] post ...     Publish a post from a file or stdin
  littlefedi [--config <path>] migrate      Run database migrations only
  littlefedi [--config <path>] mesh ...     littleMesh identity and diagnostics
  littlefedi [--config <path>] lighthouse   Run a littleMesh relay for nodes behind NAT
  littlefedi [--config <path>] directory    Run the littleMesh directory
  littlefedi version                        Print the version and exit
  littlefedi help                           Show this help

Run 'littlefedi admin', 'littlefedi backup', 'littlefedi mesh', or 'littlefedi post --help' for command help.
`)
}

// runMigrate runs database migrations and exits.
func runMigrate(cfg *config.Config) {
	store, err := storage.NewBunStorage(
		cfg.DB.DSN,
		cfg.DB.MaxOpenConns,
		cfg.DB.MaxIdleConns,
		cfg.DB.LogQueries,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.RunMigrations(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error running migrations: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Migrations complete")
}

// setupVAPIDKeys resolves the persistent Web Push application-server key.
// Explicit config wins; otherwise the first process creates a singleton in
// the database and every subsequent process/restart reuses it.
func setupVAPIDKeys(ctx context.Context, store storage.Storage, cfg *config.Config) (*push.VAPIDKeys, error) {
	hasPublic := cfg.Push.VAPIDPublic != ""
	hasPrivate := cfg.Push.VAPIDPrivate != ""
	if hasPublic != hasPrivate {
		return nil, fmt.Errorf("push: both vapid_public_key and vapid_private_key must be configured together")
	}

	if hasPublic {
		keys, err := push.ParseVAPIDKeys(cfg.Push.VAPIDPublic, cfg.Push.VAPIDPrivate)
		if err != nil {
			return nil, fmt.Errorf("invalid vapid keys in config: %w", err)
		}
		rotated, err := store.ReplaceVAPIDKeyPair(ctx, &models.VAPIDKeyPair{
			ID: "application", PublicKey: keys.Public, PrivateKey: keys.Private,
		})
		if err != nil {
			return nil, fmt.Errorf("persist configured vapid keys: %w", err)
		}
		if rotated {
			slog.Warn("push: configured VAPID key changed; existing subscriptions were cleared and must register again")
		} else {
			slog.Info("push: using configured VAPID keys")
		}
		return keys, nil
	}

	candidate, err := push.GenerateVAPIDKeys()
	if err != nil {
		return nil, fmt.Errorf("generate vapid keys: %w", err)
	}
	persisted, err := store.GetOrCreateVAPIDKeyPair(ctx, &models.VAPIDKeyPair{
		ID: "application", PublicKey: candidate.Public, PrivateKey: candidate.Private,
	})
	if err != nil {
		return nil, fmt.Errorf("persist generated vapid keys: %w", err)
	}
	keys, err := push.ParseVAPIDKeys(persisted.PublicKey, persisted.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("invalid persisted vapid keys: %w", err)
	}
	slog.Info("push: using database-persisted VAPID keys", "vapid_public_key", keys.Public)
	return keys, nil
}

func notificationCreatedHandler(store storage.Storage, broker streaming.Broker) queue.JobHandler {
	return func(ctx context.Context, id string) error {
		notif, err := store.GetNotification(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := broker.Publish(ctx, streaming.BrokerEvent{Kind: "cache.invalidate"}); err != nil {
			return err
		}
		return broker.Publish(ctx, streaming.BrokerEvent{Kind: "notification.created", ID: notif.ID, AccountID: notif.AccountID})
	}
}

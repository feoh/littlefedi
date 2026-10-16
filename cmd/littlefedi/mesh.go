package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mesh"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// resolveMeshIdentity loads this instance's littleMesh key pair, creating it
// on first run, and settles the domain every ActivityPub URI is minted from.
//
// It must run before anything reads cfg.WebDomain, because for a node with no
// domain of its own the domain *is* the identity: "<node-id>.mesh.<lighthouse>".
// It is a no-op when mesh is disabled.
func resolveMeshIdentity(ctx context.Context, store storage.Storage, cfg *config.Config) (*mesh.Identity, error) {
	if !cfg.Mesh.Enabled {
		return nil, nil
	}

	fresh, err := mesh.NewIdentity()
	if err != nil {
		return nil, err
	}
	freshPEM, err := fresh.PrivateKeyPEM()
	if err != nil {
		return nil, err
	}
	// The candidate is only stored when no identity exists yet, so a restart -
	// or two processes starting at once - can never mint a second node ID and
	// silently change the instance's domain.
	stored, err := store.GetOrCreateMeshIdentity(ctx, &models.MeshIdentity{
		NodeID:     fresh.NodeID,
		PrivateKey: freshPEM,
	})
	if err != nil {
		return nil, fmt.Errorf("load mesh identity: %w", err)
	}
	identity, err := mesh.IdentityFromPEM(stored.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("stored mesh identity is unusable: %w", err)
	}
	if identity.NodeID != stored.NodeID {
		return nil, fmt.Errorf("stored mesh identity is inconsistent: key is %s but node id is recorded as %s", identity.NodeID, stored.NodeID)
	}

	if err := applyMeshDomain(cfg, identity.NodeID); err != nil {
		return nil, err
	}

	// Publish a dial-only node so short-lived commands can still reach mesh
	// peers - `post` resolves mentions, `admin refresh-actors` fetches actors.
	// It is not started, so it neither binds a port nor registers with a
	// lighthouse, and therefore cannot displace the running instance's
	// registration. The server replaces it with a fully started node.
	directory, directoryErr := meshDirectoryClient(cfg)
	if directoryErr != nil {
		slog.Warn("mesh outbound transport unavailable for this command", "error", directoryErr)
		return identity, nil
	}
	nodeConfig := meshNodeConfig(cfg, directory)
	if node, err := mesh.New(identity, nodeConfig); err == nil {
		mesh.SetDefault(node)
	} else {
		slog.Warn("mesh outbound transport unavailable for this command", "error", err)
	}
	return identity, nil
}

// applyMeshDomain gives a domainless instance the domain its mesh identity
// implies, and refuses a configured mesh domain that belongs to another key.
// An instance that already has a public domain keeps it: for that deployment
// the mesh is an extra way out, not a new identity.
func applyMeshDomain(cfg *config.Config, nodeID string) error {
	suffix := cfg.Mesh.Suffix
	if suffix == "" {
		if len(cfg.Mesh.Lighthouses) == 0 {
			return fmt.Errorf("mesh: cannot derive a domain without mesh.suffix or a lighthouse")
		}
		suffix = mesh.SuffixForLighthouse(strings.SplitN(cfg.Mesh.Lighthouses[0], "@", 2)[0])
	}
	domain := nodeID + "." + suffix

	if cfg.WebDomain == "" {
		cfg.WebDomain = domain
		if cfg.Mesh.Alias != "" {
			cfg.LocalDomain = cfg.Mesh.Alias
		} else {
			cfg.LocalDomain = domain
		}
		cfg.Domain = cfg.LocalDomain
	} else if configured, _, isMesh := mesh.SplitDomain(cfg.WebDomain); isMesh && configured != nodeID {
		return fmt.Errorf("web_domain %q belongs to mesh node %s, but this instance's identity is %s; "+
			"either remove web_domain to adopt %s or restore the matching mesh identity",
			cfg.WebDomain, configured, nodeID, domain)
	}
	if cfg.LocalDomain == "" {
		if cfg.Mesh.Alias != "" {
			cfg.LocalDomain = cfg.Mesh.Alias
		} else {
			cfg.LocalDomain = cfg.WebDomain
		}
		cfg.Domain = cfg.LocalDomain
	}
	if cfg.Federation.UserAgent == "" {
		cfg.Federation.UserAgent = config.DefaultUserAgent(cfg.WebDomain)
	}
	return nil
}

// startMeshNode brings the node online and publishes it as the process-wide
// mesh transport, so clients built later in startup pick it up.
func startMeshNode(ctx context.Context, cfg *config.Config, identity *mesh.Identity, directory *mesh.DirectoryClient) (*mesh.Node, error) {
	node, err := mesh.New(identity, meshNodeConfig(cfg, directory))
	if err != nil {
		return nil, err
	}
	if err := node.Start(ctx); err != nil {
		return nil, err
	}
	mesh.SetDefault(node)
	return node, nil
}

func meshNodeConfig(cfg *config.Config, directory *mesh.DirectoryClient) mesh.Config {
	return mesh.Config{
		Suffix:              cfg.Mesh.Suffix,
		Lighthouses:         cfg.Mesh.Lighthouses,
		Directory:           directory,
		DiscoverLighthouses: cfg.Mesh.DiscoverLighthouses,
		DirectListen:        cfg.Mesh.DirectListen,
		DirectAdvertise:     cfg.Mesh.DirectAdvertise,
		AllowPrivateDirect:  cfg.Mesh.AllowPrivateDirect,
		MaxCircuits:         cfg.Mesh.MaxCircuits,
		DialTimeout:         cfg.Mesh.DialTimeout,
	}
}

func meshDirectoryClient(cfg *config.Config) (*mesh.DirectoryClient, error) {
	if cfg.Mesh.DirectoryURL == "" {
		return nil, nil
	}
	return mesh.NewPersistentDirectoryClient(cfg.Mesh.DirectoryURL, nil, cfg.Mesh.DirectoryState)
}

func meshDirectoryLocators(cfg *config.Config) []mesh.LighthouseLocator {
	locators := make([]mesh.LighthouseLocator, 0, mesh.MaxDirectoryLighthouses)
	for _, entry := range cfg.Mesh.Lighthouses {
		locator, err := mesh.DirectoryLocator(entry)
		if err != nil {
			slog.Warn("mesh lighthouse omitted from public directory record", "entry", entry, "error", err)
			continue
		}
		if len(locators) == mesh.MaxDirectoryLighthouses {
			slog.Warn("mesh lighthouse omitted from public directory record", "entry", entry,
				"error", "record already has three lighthouses")
			continue
		}
		locators = append(locators, locator)
	}
	return locators
}

func publishMeshDirectoryRecord(ctx context.Context, cfg *config.Config, identity *mesh.Identity, directory *mesh.DirectoryClient) error {
	if cfg.Mesh.Alias == "" || directory == nil {
		return nil
	}
	payload, err := directory.Publish(ctx, identity, cfg.Mesh.Alias, cfg.Mesh.RecordTTL, meshDirectoryLocators(cfg))
	if err != nil {
		return err
	}
	slog.Info("mesh directory record published", "alias", payload.Alias,
		"sequence", payload.Sequence, "expires_at", time.Unix(payload.ExpiresAt, 0),
		"lighthouses", len(payload.Lighthouses))
	return nil
}

func startMeshDirectoryPublisher(ctx context.Context, cfg *config.Config, identity *mesh.Identity, directory *mesh.DirectoryClient) {
	if cfg.Mesh.Alias == "" || directory == nil {
		return
	}
	go func() {
		delay := time.Duration(0)
		for {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			publishCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := publishMeshDirectoryRecord(publishCtx, cfg, identity, directory)
			cancel()
			if err != nil {
				slog.Error("mesh directory publication failed", "error", err)
				delay = min(cfg.Mesh.PublishInterval, time.Minute)
			} else {
				delay = cfg.Mesh.PublishInterval
			}
		}
	}()
}

// runLighthouseCommand serves a relay, and optionally the HTTPS gateway that
// makes the nodes behind it visible to the ordinary fediverse.
func runLighthouseCommand(args []string, cfg *config.Config) {
	printID := false
	for _, arg := range args {
		switch arg {
		case "--print-id", "-print-id":
			printID = true
		case "--help", "-h":
			printLighthouseUsage()
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown lighthouse option %q\n", arg)
			printLighthouseUsage()
			os.Exit(1)
		}
	}

	if cfg.Lighthouse.Listen == "" {
		cfg.Lighthouse.Listen = ":" + mesh.DefaultLighthousePort
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := openStore(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()
	if err := store.RunMigrations(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error running migrations: %v\n", err)
		os.Exit(1)
	}

	// A lighthouse is a mesh member like any other and keeps its identity in
	// the same place, so operators back it up the same way.
	identity, err := loadMeshIdentity(ctx, store)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading mesh identity: %v\n", err)
		os.Exit(1)
	}
	// Operators publish this so nodes can pin the relay's identity in their
	// own configuration; grepping it out of the logs is a poor substitute.
	if printID {
		fmt.Println(identity.NodeID)
		return
	}

	lighthouse, err := mesh.NewLighthouse(identity, mesh.LighthouseConfig{
		Listen:                 cfg.Lighthouse.Listen,
		Allowlist:              cfg.Lighthouse.Allowlist,
		MaxBytesPerNodePerHour: cfg.Lighthouse.MaxBytesPerNodePerHour,
		MaxCircuitsPerNode:     cfg.Lighthouse.MaxCircuitsPerNode,
		MaxConnections:         cfg.Lighthouse.MaxConnections,
		MaxConnectionsPerIP:    cfg.Lighthouse.MaxConnectionsPerIP,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error starting lighthouse: %v\n", err)
		os.Exit(1)
	}

	go func() {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
		sig := <-signals
		slog.Info("shutting down lighthouse", "signal", sig)
		cancel()
	}()

	if cfg.Lighthouse.GatewayListen != "" {
		if err := startMeshGateway(ctx, cfg, identity); err != nil {
			fmt.Fprintf(os.Stderr, "Error starting mesh gateway: %v\n", err)
			os.Exit(1)
		}
	}

	if err := lighthouse.ListenAndServe(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Lighthouse error: %v\n", err)
		os.Exit(1)
	}
}

func runDirectoryCommand(args []string, cfg *config.Config) {
	if len(args) > 0 && args[0] == "reserve" {
		if len(args) != 3 || cfg.Directory.StateFile == "" {
			printDirectoryUsage()
			os.Exit(1)
		}
		directory, err := mesh.NewPersistentDirectory(
			cfg.Directory.StateFile, cfg.Directory.OpenRegistration, cfg.Directory.MaxRecords)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading directory: %v\n", err)
			os.Exit(1)
		}
		defer directory.Close()
		if err := directory.Reserve(args[1], args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "Error reserving alias: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("reserved %s for %s\n", strings.ToLower(strings.TrimSuffix(args[1], ".")),
			strings.ToLower(args[2]))
		return
	}
	for _, arg := range args {
		switch arg {
		case "--help", "-h":
			printDirectoryUsage()
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown directory option %q\n", arg)
			printDirectoryUsage()
			os.Exit(1)
		}
	}
	if cfg.Directory.Listen == "" || cfg.Directory.StateFile == "" {
		fmt.Fprintln(os.Stderr, "directory requires directory.listen and directory.state_file")
		os.Exit(1)
	}
	directory, err := mesh.NewPersistentDirectory(
		cfg.Directory.StateFile, cfg.Directory.OpenRegistration, cfg.Directory.MaxRecords)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading directory: %v\n", err)
		os.Exit(1)
	}
	defer directory.Close()
	if cfg.Directory.OpenRegistration {
		slog.Warn("mesh directory open registration is enabled; reservations are safer for public deployment")
	}
	server := &http.Server{
		Addr:              cfg.Directory.Listen,
		Handler:           directory,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	stopped := make(chan os.Signal, 1)
	signal.Notify(stopped, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-stopped
		slog.Info("shutting down mesh directory", "signal", sig)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	slog.Info("mesh directory listening", "addr", cfg.Directory.Listen, "state", cfg.Directory.StateFile,
		"open_registration", cfg.Directory.OpenRegistration, "max_records", cfg.Directory.MaxRecords)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "Directory error: %v\n", err)
		os.Exit(1)
	}
}

// startMeshGateway serves the HTTPS front door. It dials circuits as an
// ordinary mesh node (through the lighthouse it sits beside), so the node on
// the far end sees a normal, pinned peer rather than a special case. It never
// registers, because nothing needs to reach the gateway over the mesh.
//
// The gateway opens circuits through its own lighthouse over
// loopback TCP. Wire an in-process shortcut if that hop shows up in a profile.
func startMeshGateway(ctx context.Context, cfg *config.Config, identity *mesh.Identity) error {
	suffix := cfg.Mesh.Suffix
	if suffix == "" {
		suffix = mesh.SuffixForLighthouse(cfg.Lighthouse.Listen)
	}
	if suffix == "mesh" {
		return fmt.Errorf("lighthouse.gateway_listen needs mesh.suffix set to the public name of this lighthouse, e.g. \"mesh.faro.example\"")
	}
	directory, err := meshDirectoryClient(cfg)
	if err != nil {
		return err
	}

	node, err := mesh.New(identity, mesh.Config{
		Suffix:      suffix,
		Lighthouses: []string{loopbackAddress(cfg.Lighthouse.Listen) + "@" + identity.NodeID},
		Directory:   directory,
	})
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr: cfg.Lighthouse.GatewayListen,
		Handler: mesh.Gateway(node, mesh.GatewayConfig{
			Mode:       cfg.Lighthouse.GatewayMode,
			FullAccess: cfg.Lighthouse.GatewayFullAccess,
			Directory:  directory,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	go func() {
		mode := cfg.Lighthouse.GatewayMode
		if mode == "" {
			mode = mesh.GatewayModeFederation
		}
		slog.Info("mesh gateway listening", "addr", cfg.Lighthouse.GatewayListen, "suffix", suffix,
			"mode", mode, "full_access", len(cfg.Lighthouse.GatewayFullAccess))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("mesh gateway error", "error", err)
		}
	}()
	return nil
}

// runMeshCommand implements `littlefedi mesh <subcommand>`.
func runMeshCommand(args []string, cfg *config.Config) {
	if len(args) == 0 {
		printMeshUsage()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, err := openStore(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()
	if err := store.RunMigrations(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error running migrations: %v\n", err)
		os.Exit(1)
	}

	switch args[0] {
	case "id":
		identity, err := resolveMeshIdentity(ctx, store, cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		if identity == nil {
			fmt.Fprintln(os.Stderr, "Mesh is disabled. Set [mesh] enabled = true to join littleMesh.")
			return
		}
		fmt.Printf("node id:  %s\n", identity.NodeID)
		fmt.Printf("domain:   %s\n", cfg.WebDomain)
		fmt.Printf("handles:  @<user>@%s\n", cfg.LocalDomain)
		if len(cfg.Mesh.Lighthouses) > 0 {
			fmt.Printf("lighthouses: %s\n", strings.Join(cfg.Mesh.Lighthouses, ", "))
		}
	case "ping":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: littlefedi mesh ping <node-id|domain>")
			os.Exit(1)
		}
		if err := meshPing(ctx, store, cfg, args[1]); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
	case "peers":
		if err := meshPeers(ctx, store, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
	case "publish":
		identity, err := resolveMeshIdentity(ctx, store, cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		directory, err := meshDirectoryClient(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		if identity == nil || directory == nil || cfg.Mesh.Alias == "" {
			fmt.Fprintln(os.Stderr, "mesh publish requires mesh.enabled, mesh.alias, mesh.directory_url, and mesh.directory_state")
			os.Exit(1)
		}
		if err := publishMeshDirectoryRecord(ctx, cfg, identity, directory); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
	default:
		printMeshUsage()
	}
}

// meshPing opens a circuit and reports how it got there. It is the first thing
// to run when a peer looks unreachable: it separates "the lighthouse cannot be
// reached" from "the peer is not registered" from "the peer is there".
func meshPing(ctx context.Context, store storage.Storage, cfg *config.Config, target string) error {
	identity, err := resolveMeshIdentity(ctx, store, cfg)
	if err != nil {
		return err
	}
	if identity == nil {
		return fmt.Errorf("mesh is disabled; set [mesh] enabled = true")
	}

	// Not started: a node only has to register to *receive*
	// circuits, and registering here would displace the running instance's own
	// registration on the lighthouse - a diagnostic that knocks the instance
	// offline while it runs is worse than no diagnostic.
	directory, err := meshDirectoryClient(cfg)
	if err != nil {
		return err
	}
	node, err := mesh.New(identity, meshNodeConfig(cfg, directory))
	if err != nil {
		return err
	}
	defer node.Close()

	domain := target
	if mesh.ValidNodeID(target) {
		domain = target + "." + node.Suffix()
	}
	if _, _, ok := mesh.SplitDomain(domain); !ok {
		return fmt.Errorf("%q is neither a node id nor a mesh domain", target)
	}

	start := time.Now()
	conn, err := node.DialContext(ctx, "tcp", domain+":443")
	if err != nil {
		return fmt.Errorf("no circuit to %s: %w", domain, err)
	}
	defer conn.Close()

	path := "relay"
	if mesh.IsDirect(conn) {
		path = "direct"
	}
	fmt.Printf("circuit to %s established in %s (%s)\n", domain, time.Since(start).Round(time.Millisecond), path)
	return nil
}

// meshPeers lists the mesh instances this node has federated with.
func meshPeers(ctx context.Context, store storage.Storage, cfg *config.Config) error {
	identity, err := resolveMeshIdentity(ctx, store, cfg)
	if err != nil {
		return err
	}
	if identity == nil {
		return fmt.Errorf("mesh is disabled; set [mesh] enabled = true")
	}

	instances, err := store.ListInstances(ctx)
	if err != nil {
		return fmt.Errorf("list instances: %w", err)
	}
	peers := instances[:0]
	for _, instance := range instances {
		if _, _, ok := mesh.SplitDomain(instance.Domain); ok {
			peers = append(peers, instance)
		}
	}
	if len(peers) == 0 {
		fmt.Println("No mesh peers known yet.")
		return nil
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].Domain < peers[j].Domain })
	for _, peer := range peers {
		state := "reachable"
		if peer.UnreachableSince != nil {
			state = "unreachable since " + peer.UnreachableSince.Format(time.RFC3339)
		}
		fmt.Printf("%s  %s\n", peer.Domain, state)
	}
	return nil
}

// loadMeshIdentity returns the stored identity, creating one if needed, without
// touching the instance's domain settings.
func loadMeshIdentity(ctx context.Context, store storage.Storage) (*mesh.Identity, error) {
	fresh, err := mesh.NewIdentity()
	if err != nil {
		return nil, err
	}
	freshPEM, err := fresh.PrivateKeyPEM()
	if err != nil {
		return nil, err
	}
	stored, err := store.GetOrCreateMeshIdentity(ctx, &models.MeshIdentity{
		NodeID:     fresh.NodeID,
		PrivateKey: freshPEM,
	})
	if err != nil {
		return nil, err
	}
	return mesh.IdentityFromPEM(stored.PrivateKey)
}

func openStore(cfg *config.Config) (storage.Storage, error) {
	return storage.NewBunStorage(cfg.DB.DSN, cfg.DB.MaxOpenConns, cfg.DB.MaxIdleConns, cfg.DB.LogQueries)
}

// loopbackAddress rewrites a wildcard listen address into one the local
// process can dial.
func loopbackAddress(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		return net.JoinHostPort("127.0.0.1", port)
	}
	return listen
}

func printMeshUsage() {
	fmt.Fprint(os.Stderr, `littleMesh commands:

  littlefedi mesh id                    Show this instance's node id and domain
  littlefedi mesh ping <node-id|domain> Open a circuit to a peer and time it
  littlefedi mesh peers                 List known mesh peers
  littlefedi mesh publish               Publish the signed alias and lighthouse record

  littlefedi lighthouse                 Run a relay for nodes behind NAT

See docs/littlemesh.md for the setup guides.
`)
}

func printLighthouseUsage() {
	fmt.Fprint(os.Stderr, `Usage: littlefedi [--config <path>] lighthouse [--print-id]

Runs a littleMesh relay: it introduces nodes that cannot accept connections and
copies encrypted bytes between them.

  --print-id   Print this relay's node id and exit. Publish it alongside the
               relay's address so nodes can pin it: "faro.example:7842@<id>".

The configuration needs a [db] dsn and a [lighthouse] listen address. A relay
serves no ActivityPub identity, so it needs no domain. See docs/littlemesh.md.
`)
}

func printDirectoryUsage() {
	fmt.Fprint(os.Stderr, `Usage:
  littlefedi [--config <path>] directory
  littlefedi [--config <path>] directory reserve <alias> <node-id>

Runs the littleMesh directory of aliases and lighthouses. Put the
listener behind HTTPS and configure [directory] listen and state_file. Closed
registration is the default; reserve each alias before its first publication.
`)
}

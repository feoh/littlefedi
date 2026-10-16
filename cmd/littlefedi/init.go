package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/term"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/accounts"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/federation"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mesh"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

type initAnswers struct {
	name, localDomain, webDomain, listen, username, password string
	trustedProxies                                           []string
	lowPower, multiUser, registrations                       bool
	// The instance profile published on /about and through the instance APIs.
	// The long-form extended description and terms of service are deliberately
	// not asked for here (they are pages, not prompt answers), and the
	// generated config points at /admin/instance for them instead.
	shortDescription, description, contactEmail string
	rules                                       []string
	blog, blogAllowed, blogAllAccounts          bool
	lighthouse                                  bool
	gateway                                     bool
	gatewayDirectory                            bool
	gatewayHost                                 string
	// mesh selects littleMesh, for an instance with no public domain. Its
	// domain is derived from a key pair generated during this run, so
	// localDomain and webDomain stay empty.
	mesh        bool
	lighthouses []string
	// meshAlias enables a readable handle alias resolved by a signed
	// directory while the actor ID remains bound to the full node ID.
	meshAlias    bool
	alias        string
	directoryURL string
	meshSuffix   string
	// meshReach adds outbound littleMesh reachability to an instance that
	// keeps its own public domain: it can still call mesh-only
	// peers, but its own identity and inbound federation are unaffected.
	meshReach            bool
	meshReachLighthouses []string
}

func runInitCommand(args []string, configPath string, in io.Reader, out io.Writer) error {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config", "-config":
			if i+1 == len(args) {
				return fmt.Errorf("--config needs a path")
			}
			configPath = args[i+1]
			i++
		case "--help", "-h":
			printInitUsage(out)
			return nil
		default:
			return fmt.Errorf("unknown init option %q", args[i])
		}
	}
	if configPath == "" {
		configPath = "./littlefedi.toml"
	}
	configPath, err := filepath.Abs(configPath)
	if err != nil {
		return fmt.Errorf("resolve config path: %w", err)
	}
	dbPath := filepath.Join(filepath.Dir(configPath), "littlefedi.db")
	if exists(configPath) || exists(dbPath) {
		return fmt.Errorf("refusing to initialize: %s or %s already exists", configPath, dbPath)
	}

	answers, err := promptInit(in, out)
	if err != nil {
		return err
	}
	directoryStatePath := ""
	if answers.meshAlias || answers.gatewayDirectory {
		directoryStatePath = initDirectoryStatePath(dbPath)
		if exists(directoryStatePath) {
			return fmt.Errorf("refusing to initialize: %s already exists", directoryStatePath)
		}
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(configPath), ".littlefedi-init-*.toml")
	if err != nil {
		return fmt.Errorf("stage config: %w", err)
	}
	tmpPath := tmp.Name()
	published := false
	createdDB := false
	defer func() {
		if !published {
			_ = os.Remove(tmpPath)
		}
		if createdDB && !published {
			removeSQLiteFiles(dbPath)
		}
		if directoryStatePath != "" && !published {
			removeSQLiteFiles(directoryStatePath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure staged config: %w", err)
	}
	if _, err := tmp.WriteString(initTOML(answers, dbPath)); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write staged config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close staged config: %w", err)
	}
	cfg, err := config.Load(tmpPath)
	if err != nil {
		return fmt.Errorf("validate configuration: %w", err)
	}
	if answers.meshAlias || answers.gatewayDirectory {
		directory, err := mesh.NewPersistentDirectoryClient(
			cfg.Mesh.DirectoryURL, nil, cfg.Mesh.DirectoryState)
		if err != nil {
			return fmt.Errorf("initialize mesh directory state: %w", err)
		}
		if err := directory.Close(); err != nil {
			return fmt.Errorf("close mesh directory state: %w", err)
		}
	}

	store, err := storage.NewBunStorage(cfg.DB.DSN, cfg.DB.MaxOpenConns, cfg.DB.MaxIdleConns, cfg.DB.LogQueries)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	createdDB = true
	ctx := context.Background()
	if err := store.RunMigrations(ctx); err != nil {
		_ = store.Close()
		return fmt.Errorf("run migrations: %w", err)
	}
	if answers.lighthouse {
		identity, err := loadMeshIdentity(ctx, store)
		if err != nil {
			_ = store.Close()
			return fmt.Errorf("create lighthouse identity: %w", err)
		}
		if err := store.Close(); err != nil {
			return fmt.Errorf("close database: %w", err)
		}
		if err := os.Link(tmpPath, configPath); err != nil {
			return fmt.Errorf("publish config: %w", err)
		}
		published = true
		_ = os.Remove(tmpPath)
		fmt.Fprintf(out, "Initialized littleMesh lighthouse.\nConfig: %s\nLighthouse identity: %s\nStart with: littlefedi --config %s lighthouse\n", configPath, identity.NodeID, configPath)
		return nil
	}
	// On a mesh instance this generates the node key and, with it, the domain
	// every identity below is minted under. Init does not publish a process-wide
	// transport; the real server does that when it starts.
	var meshIdentity *mesh.Identity
	if cfg.Mesh.Enabled {
		meshIdentity, err = loadMeshIdentity(ctx, store)
		if err == nil {
			err = applyMeshDomain(cfg, meshIdentity.NodeID)
		}
		if err != nil {
			_ = store.Close()
			return fmt.Errorf("set up mesh identity: %w", err)
		}
	}
	admin, err := accounts.BootstrapLocalAccount(ctx, store, cfg.WebDomain, answers.username, answers.password, true)
	if err != nil {
		_ = store.Close()
		return fmt.Errorf("create administrator: %w", err)
	}
	if answers.blogAllowed {
		if err := store.SetBlogAllowed(ctx, admin.ID, true); err != nil {
			_ = store.Close()
			return fmt.Errorf("allow blog publishing: %w", err)
		}
	}
	if _, err := federation.SetupInstanceActorWithDomains(ctx, store, cfg.LocalDomain, cfg.WebDomain); err != nil {
		_ = store.Close()
		return fmt.Errorf("create instance actor: %w", err)
	}
	if _, err := setupVAPIDKeys(ctx, store, cfg); err != nil {
		_ = store.Close()
		return fmt.Errorf("generate VAPID keys: %w", err)
	}
	if err := store.Close(); err != nil {
		return fmt.Errorf("close database: %w", err)
	}
	if err := os.Link(tmpPath, configPath); err != nil {
		return fmt.Errorf("publish config: %w", err)
	}
	published = true
	_ = os.Remove(tmpPath)
	fmt.Fprintf(out, "Initialized littleFedi.\nConfig: %s\nAdministrator: @%s@%s\n",
		configPath, answers.username, cfg.LocalDomain)
	if answers.meshAlias && meshIdentity != nil {
		fmt.Fprintf(out, "\nReadable alias reservation required before publication:\n  %s -> %s\nAsk the directory operator to run:\n  littlefedi --config <directory-config> directory reserve %s %s\nThe instance publishes automatically after the reservation exists.\n",
			answers.alias, meshIdentity.NodeID, answers.alias, meshIdentity.NodeID)
		fmt.Fprintf(out, "After the reservation, start with: littlefedi --config %s\n", configPath)
	} else {
		fmt.Fprintf(out, "Start with: littlefedi --config %s\n", configPath)
	}
	fmt.Fprintln(out, "For common customizations, see the Configuration section in README.md; the complete reference is config/littlefedi.toml.example.")
	return nil
}

func promptInit(in io.Reader, out io.Writer) (initAnswers, error) {
	var a initAnswers
	var err error
	if a.lighthouse, err = promptInitTarget(in, out); err != nil {
		return a, err
	}
	if a.lighthouse {
		fmt.Fprintln(out, "A lighthouse is a publicly reachable littleMesh relay. It stores no accounts and sees only encrypted traffic. TCP port 7842 is the standard.")
		a.listen, err = promptText(in, out, "Lighthouse listen address", ":7842", validateListen)
		if err != nil {
			return a, err
		}
		fmt.Fprintln(out, "An HTTPS gateway makes mesh nodes reachable by ordinary fediverse servers. It needs wildcard DNS and a wildcard TLS certificate on a reverse proxy such as Caddy.")
		if a.gateway, err = promptBool(in, out, "Enable the HTTPS gateway", false); err != nil || !a.gateway {
			return a, err
		}
		a.gatewayHost, err = promptText(in, out, "Gateway base domain (the gateway uses *.mesh.<domain>)", "", validateInitDomain)
		if err != nil {
			return a, err
		}
		fmt.Fprintln(out, "Readable littleMesh handles need the gateway to resolve aliases through the directory.")
		if a.gatewayDirectory, err = promptBool(in, out, "Enable readable directory aliases", true); err != nil || !a.gatewayDirectory {
			return a, err
		}
		a.directoryURL, err = promptText(in, out, "Directory URL", "", validateInitDirectoryURL)
		return a, err
	}
	fmt.Fprintln(out, "This name is shown in the web UI and instance metadata; you can change it later in the config file.")
	if a.name, err = promptText(in, out, "Instance name", "littleFedi", func(string) error { return nil }); err != nil {
		return a, err
	}
	fmt.Fprintln(out, "The normal fediverse setup needs a domain name pointing at this machine, served over public HTTPS. littleMesh is the alternative: this instance is named by a key pair it generates now, and reaches other instances through a relay, so it works from behind NAT or CGNAT with no domain, no certificate, and no port forwarding. A mesh instance can only federate with mesh-aware instances unless its relay operator runs a gateway.")
	if a.mesh, err = promptBool(in, out, "Join littleMesh instead of using a public domain", false); err != nil {
		return a, err
	}

	if a.mesh {
		fmt.Fprintln(out, "With a directory, littleMesh can give you a readable handle while the actor ID stays tied to the node key, and lighthouses can be replaced without changing either. You need an alias reserved on the directory and pinned public lighthouses. Say no to keep the long handle made from the key.")
		if a.meshAlias, err = promptBool(in, out, "Use a readable directory alias", false); err != nil {
			return a, err
		}
		if a.meshAlias {
			if a.alias, err = promptText(in, out, "Readable alias domain (for example red-house.mesh.example.com)", "", validateInitDomain); err != nil {
				return a, err
			}
			if a.directoryURL, err = promptText(in, out, "Directory URL", "", validateInitDirectoryURL); err != nil {
				return a, err
			}
			if a.meshSuffix, err = promptText(in, out, "Stable technical mesh suffix", "", validateInitDomain); err != nil {
				return a, err
			}
			fmt.Fprintln(out, "Each public lighthouse must include its Ed25519 node-ID pin. The directory publishes at most three locators. The relay carries encrypted circuits but cannot impersonate the technical node ID.")
			lighthouses, err := promptText(in, out, "Pinned public lighthouse address(es), comma-separated", "", validateInitDirectoryLighthouses)
			if err != nil {
				return a, err
			}
			a.lighthouses = splitAndTrim(lighthouses)
		} else {
			fmt.Fprintln(out, "The relay (\"lighthouse\") this instance registers with. It never sees your traffic in clear - it only forwards encrypted bytes - but it does decide whether to carry them. Preferred format: host:7842@node-id (host:7842 is accepted without a pin), comma-separated for more than one. The first host becomes part of your permanent domain; add backups at the end and never reorder the list.")
			lighthouses, err := promptText(in, out, "Lighthouse address(es)", "", validateInitLighthouses)
			if err != nil {
				return a, err
			}
			a.lighthouses = splitAndTrim(lighthouses)
		}
	} else {
		fmt.Fprintln(out, "This becomes part of every account handle on this instance (@user@domain) and its ActivityPub identifiers. Other servers store it permanently, so changing it later breaks federation with anyone who already follows a local account.")
		if a.localDomain, err = promptText(in, out, "Local (handle) domain", "", validateInitDomain); err != nil {
			return a, err
		}
		fmt.Fprintln(out, "Set this only if the domain visitors and other servers actually connect to differs from the handle domain above (e.g. the handle domain is a vanity name that redirects here). Leave blank if they're the same.")
		if a.webDomain, err = promptText(in, out, "Web domain (blank = local domain)", "", func(s string) error {
			if s == "" {
				return nil
			}
			return validateInitDomain(s)
		}); err != nil {
			return a, err
		}
		if a.webDomain == "" {
			a.webDomain = a.localDomain
		}
		fmt.Fprintln(out, "Optional: this instance keeps its own domain either way, but it can also open outbound littleMesh circuits to reach instances that have no domain of their own (e.g. ones running behind NAT). Listing two lighthouses gives that reachability a backup if one relay is down; listing one still works.")
		if a.meshReach, err = promptBool(in, out, "Add littleMesh reachability", false); err != nil {
			return a, err
		}
		if a.meshReach {
			lighthouses, err := promptText(in, out, "Lighthouse address(es), comma-separated", "", validateInitReachLighthouses)
			if err != nil {
				return a, err
			}
			a.meshReachLighthouses = splitAndTrim(lighthouses)
		}
	}

	fmt.Fprintln(out, "Address and port the littlefedi process itself binds to. If you're putting nginx/haproxy/Caddy in front of it, this is usually a local address like 127.0.0.1:8080, not the public port.")
	listenDefault := ":8080"
	if a.mesh {
		// A mesh instance is reached over circuits, so its HTTP listener only
		// needs to serve the operator's own browser.
		listenDefault = "127.0.0.1:8080"
	}
	if a.listen, err = promptText(in, out, "HTTP listen address", listenDefault, validateListen); err != nil {
		return a, err
	}
	if !a.mesh {
		fmt.Fprintln(out, "Only needed if you're running behind a reverse proxy. Lists the proxy's own IPs/CIDRs so littlefedi trusts its X-Forwarded-For header and logs real client IPs instead of the proxy's. Leave blank if there's no reverse proxy.")
		trustedProxies, err := promptText(in, out, "Trusted proxy IPs/CIDRs (comma-separated; blank = none)", "", func(s string) error {
			if s == "" {
				return nil
			}
			_, err := newTrustedProxySet(strings.Split(s, ","))
			return err
		})
		if err != nil {
			return a, err
		}
		a.trustedProxies = splitAndTrim(trustedProxies)
	}
	fmt.Fprintln(out, "Trims memory and worker usage for small VPS/single-board deployments (fewer queue workers, smaller media dimensions, smaller cache). Say yes on constrained hardware; no otherwise.")
	if a.lowPower, err = promptBool(in, out, "Use low-power preset", false); err != nil {
		return a, err
	}
	fmt.Fprintln(out, "Single-user runs this instance for one account only (you). Multi-user allows several local accounts and adds an admin/registrations layer. Most personal instances want single-user.")
	if a.multiUser, err = promptMode(in, out); err != nil {
		return a, err
	}
	if a.multiUser {
		fmt.Fprintln(out, "If yes, anyone can sign up for an account on this instance without an invite. If no, only accounts you create manually can log in.")
		if a.registrations, err = promptBool(in, out, "Open registrations", false); err != nil {
			return a, err
		}
	}
	if err := promptInstanceProfile(in, out, &a); err != nil {
		return a, err
	}
	fmt.Fprintln(out, "Login name for the first administrator account, created now with the password you set below.")
	if a.username, err = promptText(in, out, "First administrator username", "", accounts.ValidateUsername); err != nil {
		return a, err
	}
	if blogBuildSupported() {
		fmt.Fprintln(out, "Adds an optional static blog site (separate from the federated timeline) served alongside this instance. Say no if you only want the fediverse account.")
		if a.blog, err = promptBool(in, out, "Enable the static blog site", false); err != nil {
			return a, err
		}
		if a.blog {
			if a.multiUser {
				fmt.Fprintln(out, "Say yes to let every account on this instance publish a blog. Say no to hand it out account by account, from Admin > Accounts or `admin blog allow`.")
				if a.blogAllAccounts, err = promptBool(in, out, "Allow blog publishing for every account", false); err != nil {
					return a, err
				}
			}
			// The administrator is granted either way: on a single-user
			// instance there is nobody else to ask about, and on a
			// multi-user one this keeps them publishing if all_accounts is
			// ever turned back off.
			if a.blogAllAccounts {
				a.blogAllowed = true
			} else {
				fmt.Fprintln(out, "Lets this administrator publish blog posts. You can grant this to other accounts later; it doesn't affect who can post to the federated timeline.")
				if a.blogAllowed, err = promptBool(in, out, "Allow blog publishing for "+a.username, true); err != nil {
					return a, err
				}
			}
		}
	}
	fmt.Fprintln(out, "Password for the administrator account above. You'll be asked to type it twice.")
	for {
		fmt.Fprint(out, "Administrator password: ")
		a.password, err = readInitPassword(in)
		fmt.Fprintln(out)
		if err != nil {
			return a, err
		}
		if err := accounts.ValidatePassword(a.password, accounts.DefaultMinPasswordLength); err != nil {
			fmt.Fprintln(out, "Invalid password:", err)
			continue
		}
		fmt.Fprint(out, "Confirm password: ")
		confirmation, err := readInitPassword(in)
		fmt.Fprintln(out)
		if err != nil {
			return a, err
		}
		if confirmation != a.password {
			fmt.Fprintln(out, "Passwords do not match.")
			continue
		}
		break
	}
	return a, nil
}

// promptInstanceProfile asks for the description, contact address, and rules
// this instance publishes about itself. It is one opt-in block rather than
// four unavoidable prompts because none of it is needed to boot, and a
// single-user instance may never want an About page at all - which is why it
// defaults to yes only in multi-user mode, where strangers read the page
// before deciding to join.
func promptInstanceProfile(in io.Reader, out io.Writer, a *initAnswers) error {
	fmt.Fprintln(out, "Optional: what this instance says about itself on its /about page and to every Mastodon client. All of it, including the longer \"about\" text and terms of service, can be written or changed later at /admin/instance.")
	describe, err := promptBool(in, out, "Describe this instance now", a.multiUser)
	if err != nil || !describe {
		return err
	}
	if a.shortDescription, err = promptText(in, out, "One-line description (blank = none)", "", func(string) error { return nil }); err != nil {
		return err
	}
	if a.description, err = promptText(in, out, "A paragraph about this instance (blank = none)", "", func(string) error { return nil }); err != nil {
		return err
	}
	fmt.Fprintln(out, "Published to clients as the address to report problems to. Leave blank to publish none.")
	if a.contactEmail, err = promptText(in, out, "Moderation contact email (blank = none)", "", func(s string) error {
		if s == "" {
			return nil
		}
		return accounts.ValidateEmail(s)
	}); err != nil {
		return err
	}
	fmt.Fprintln(out, "Server rules, one per prompt, shown on the About page and on the registration form. Press enter on an empty rule to finish.")
	for {
		rule, err := promptText(in, out, fmt.Sprintf("Rule %d (blank = done)", len(a.rules)+1), "", func(string) error { return nil })
		if err != nil {
			return err
		}
		if strings.TrimSpace(rule) == "" {
			return nil
		}
		a.rules = append(a.rules, strings.TrimSpace(rule))
	}
}

func promptInitTarget(in io.Reader, out io.Writer) (bool, error) {
	for {
		fmt.Fprint(out, "Configure an instance or lighthouse [instance]: ")
		value, err := readInitLine(in)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "", "i", "instance":
			return false, nil
		case "l", "lighthouse":
			return true, nil
		default:
			fmt.Fprintln(out, "Please choose instance or lighthouse.")
		}
	}
}

func promptText(in io.Reader, out io.Writer, label, fallback string, validate func(string) error) (string, error) {
	for {
		if fallback == "" {
			fmt.Fprintf(out, "%s: ", label)
		} else {
			fmt.Fprintf(out, "%s [%s]: ", label, fallback)
		}
		value, err := readInitLine(in)
		if err != nil {
			return "", err
		}
		value = strings.TrimSpace(value)
		if value == "" {
			value = fallback
		}
		if err := validate(value); err != nil {
			fmt.Fprintln(out, "Invalid value:", err)
			continue
		}
		return value, nil
	}
}

func promptBool(in io.Reader, out io.Writer, label string, fallback bool) (bool, error) {
	choice := "y/N"
	if fallback {
		choice = "Y/n"
	}
	for {
		fmt.Fprintf(out, "%s [%s]: ", label, choice)
		value, err := readInitLine(in)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "":
			return fallback, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Fprintln(out, "Please answer yes or no.")
		}
	}
}

func promptMode(in io.Reader, out io.Writer) (bool, error) {
	for {
		fmt.Fprint(out, "Mode: single-user or multi-user [single]: ")
		value, err := readInitLine(in)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "", "s", "single", "single-user":
			return false, nil
		case "m", "multi", "multi-user":
			return true, nil
		default:
			fmt.Fprintln(out, "Please choose single-user or multi-user.")
		}
	}
}

func readInitLine(in io.Reader) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := in.Read(buf)
		if n > 0 && buf[0] != '\r' && buf[0] != '\n' {
			b.WriteByte(buf[0])
		}
		if n > 0 && buf[0] == '\n' {
			return b.String(), nil
		}
		if err != nil {
			if err == io.EOF && b.Len() > 0 {
				return b.String(), nil
			}
			return "", err
		}
	}
}

func readInitPassword(in io.Reader) (string, error) {
	if in == os.Stdin && term.IsTerminal(int(os.Stdin.Fd())) {
		password, err := term.ReadPassword(int(os.Stdin.Fd()))
		return string(password), err
	}
	return readInitLine(in)
}

func validateInitDomain(domain string) error {
	path, err := os.CreateTemp("", "littlefedi-init-domain-*.toml")
	if err != nil {
		return err
	}
	name := path.Name()
	defer os.Remove(name)
	if _, err := fmt.Fprintf(path, "local_domain = %s\n", strconv.Quote(domain)); err != nil {
		_ = path.Close()
		return err
	}
	if err := path.Close(); err != nil {
		return err
	}
	_, err = config.Load(name)
	return err
}

func validateListen(listen string) error {
	if _, _, err := net.SplitHostPort(listen); err != nil {
		return fmt.Errorf("must be host:port or :port")
	}
	if _, err := net.ResolveTCPAddr("tcp", listen); err != nil {
		return err
	}
	return nil
}

func initTOML(a initAnswers, dbPath string) string {
	if a.lighthouse {
		meshConfig := ""
		gatewayConfig := ""
		if a.gateway {
			meshConfig = fmt.Sprintf("[mesh]\nsuffix = %s\n", strconv.Quote("mesh."+a.gatewayHost))
			if a.gatewayDirectory {
				meshConfig += fmt.Sprintf("directory_url = %s\ndirectory_state = %s\n",
					strconv.Quote(a.directoryURL), strconv.Quote(initDirectoryStatePath(dbPath)))
			}
			meshConfig += "\n"
			gatewayConfig = "gateway_listen = \"127.0.0.1:8443\" # plain HTTP; put Caddy/TLS in front\n" +
				"gateway_mode = \"federation\"       # expose ActivityPub and public media, not the Web UI/API\n"
		}
		return fmt.Sprintf(`[db]
dsn = %s

%s
[lighthouse]
listen = %s
%s

# Bandwidth is the cost of a public relay. This default allows 5 GiB per node
# per hour; set 0 only for a private relay whose users you trust.
max_bytes_per_node_per_hour = 5368709120
# allowlist = []                        # empty = open; otherwise list allowed node IDs
# max_circuits_per_node = 64
# max_connections = 4096
# max_connections_per_ip = 128

[observability]
# log_level = "debug"
`, strconv.Quote(dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_pragma=temp_store(MEMORY)"), meshConfig, strconv.Quote(a.listen), gatewayConfig)
	}
	var b strings.Builder
	if a.mesh {
		// No domain is written: it is derived from the node key at startup, so
		// pasting a 52-character node id in here would only be a way to get it
		// wrong. `littlefedi mesh id` prints the resulting domain.
		b.WriteString("# This instance is named by its littleMesh key pair; see `littlefedi mesh id`.\n")
	} else {
		fmt.Fprintf(&b, "local_domain = %s\n", strconv.Quote(a.localDomain))
		if a.webDomain != a.localDomain {
			fmt.Fprintf(&b, "web_domain = %s\n", strconv.Quote(a.webDomain))
		}
	}
	if a.lowPower {
		b.WriteString("low_power = true\n")
	}
	if a.multiUser {
		fmt.Fprintf(&b, "admins = [%s]\n", strconv.Quote(a.username))
	} else {
		fmt.Fprintf(&b, "single_user = %s\n", strconv.Quote(a.username))
	}
	fmt.Fprintf(&b, "\n[instance]\nname = %s\n", strconv.Quote(a.name))
	b.WriteString(initInstanceProfileTOML(a))
	fmt.Fprintf(&b, "\n[db]\ndsn = %s\n\n[http]\nlisten = %s\n", strconv.Quote(dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_pragma=temp_store(MEMORY)"), strconv.Quote(a.listen))
	if len(a.trustedProxies) > 0 {
		quoted := make([]string, len(a.trustedProxies))
		for i, proxy := range a.trustedProxies {
			quoted[i] = strconv.Quote(proxy)
		}
		fmt.Fprintf(&b, "trusted_proxies = [%s]\n", strings.Join(quoted, ", "))
	} else {
		b.WriteString("# trusted_proxies = [\"10.0.0.0/8\", \"172.16.0.0/12\"]  # only if behind nginx/haproxy\n")
	}
	if a.mesh {
		quoted := make([]string, len(a.lighthouses))
		for i, lighthouse := range a.lighthouses {
			quoted[i] = strconv.Quote(lighthouse)
		}
		fmt.Fprintf(&b, "\n[mesh]\nenabled = true\nlighthouses = [%s]\n", strings.Join(quoted, ", "))
		if a.meshAlias {
			fmt.Fprintf(&b, "suffix = %s\n", strconv.Quote(a.meshSuffix))
			fmt.Fprintf(&b, "directory_url = %s\n", strconv.Quote(a.directoryURL))
			fmt.Fprintf(&b, "directory_state = %s\n", strconv.Quote(initDirectoryStatePath(dbPath)))
			fmt.Fprintf(&b, "alias = %s\n", strconv.Quote(a.alias))
		}
		b.WriteString("exposure = \"federation\"\n")
		b.WriteString("# full_access = [\"<node-id>\"]        # source peers allowed when exposure = \"allowlist\"\n")
		b.WriteString("# direct_listen = \"0.0.0.0:7842\"   # accept unrelayed connections, if this host is reachable\n")
		b.WriteString("# allow_private_direct = true       # only for a mesh confined to one LAN\n")
	}
	if a.meshReach {
		quoted := make([]string, len(a.meshReachLighthouses))
		for i, lighthouse := range a.meshReachLighthouses {
			quoted[i] = strconv.Quote(lighthouse)
		}
		fmt.Fprintf(&b, "\n[mesh]\nenabled = true\nlighthouses = [%s]\n", strings.Join(quoted, ", "))
	}
	if a.multiUser && a.registrations {
		b.WriteString("\n[auth]\nregistrations = true\n")
	}
	if a.blog {
		b.WriteString("\n[blog]\nenabled = true\n")
		if a.blogAllAccounts {
			b.WriteString("all_accounts = true\n")
		}
	}
	b.WriteString(`

# Common customizations. Uncomment only the settings you want to change.
# See the Configuration section in README.md; the complete reference is
# config/littlefedi.toml.example.

# In a multi-process PostgreSQL deployment, set false on every process except
# the one responsible for housekeeping and periodic backups.
# background_maintenance = true

[media]
# max_file_size = 10485760              # upload limit in bytes (example: 10 MB; default: 40 MB)
# proxy_remote = true                   # keep remote media requests behind this instance (default: true)
# cache_remote = "lazy"                  # "off", "lazy", or "eager"; lazy caches remote media after its first use
# cache_max_age = "720h"                 # cached remote-media lifetime; use "0" to keep it forever (default: 30 days)
# max_stored_dimension = 2560           # downscale stored JPEG/PNG uploads to this longest side; 0 keeps the
#                                       # original (default: 2560; low-power preset: 1600)

[housekeeping]
# prune_remote_statuses_after_days = 30 # retain remote posts for this many days; 0 disables pruning (default: 30)
# max_delete_per_run = 500              # with hourly runs, daily capacity is this value x 24 (default: 500)
#                                        # 0 is unlimited.
# Relay-fed instances should set this above sustained daily ingest, with headroom.
# Shorter retention reduces the stored window, not deletion throughput.
# Orphaned remote accounts are swept separately, at most 500 per day.

[queue]
# workers = 4                            # defaults to available CPUs, bounded to 1..8; low-power uses 1
#                                        # set only to override automatic scaling

[backup]
# enabled = true                        # create periodic server-side backups (default: false)
# interval = "24h"                      # backup frequency; accepts hours or days such as "7d" (default: daily)
# include_media = true                  # include owned uploads; remote cache is always excluded (default: false)
# retain = 7                            # backups to keep; 0 keeps all (default: 7)

[cache]
# enabled = false                       # disable the in-memory cache (default: enabled)
# max_memory_mb = 64                    # memory budget in MB (default: 32; low-power preset: 24)

[observability]
# log_level = "debug"                   # debug, info, warn, or error (default: info)
# metrics_enabled = true                # expose Prometheus metrics (default: false)

[smtp]
# host = "smtp.example.com"
# port = 587                            # use 465 with implicit_tls = true
# username = "littlefedi@example.com"
# password = "secret"                  # prefer LITTLEFEDI_SMTP_PASSWORD
# from = "littleFedi <littlefedi@example.com>"
# Used for startup DB failures; once running, DB admins are included automatically.
# database_alert_to = "admin@example.com"
# require_tls = true                    # fail if STARTTLS is unavailable (default: true)
# implicit_tls = false                  # use TLS immediately instead of STARTTLS
# insecure_skip_verify = false          # disables certificate-chain and hostname verification
`)
	return b.String()
}

// initInstanceProfileTOML renders the [instance] profile fields. Whatever was
// left blank is written commented out rather than omitted, so the generated
// file still shows an operator every field that exists and where the rest of
// the profile is edited.
func initInstanceProfileTOML(a initAnswers) string {
	var b strings.Builder
	// The explanation goes above each field rather than after it: a filled-in
	// value is as long as the operator wants, and trailing comments stop
	// lining up the moment one of them is a real sentence.
	writeField := func(comment, key, value string) {
		fmt.Fprintf(&b, "# %s\n", comment)
		if value == "" {
			fmt.Fprintf(&b, "# %s = \"\"\n", key)
			return
		}
		fmt.Fprintf(&b, "%s = %s\n", key, strconv.Quote(value))
	}
	writeField("One line, shown in server directories and NodeInfo.", "short_description", a.shortDescription)
	writeField("A paragraph, shown at the top of /about.", "description", a.description)
	writeField("Moderation address published to clients.", "contact_email", a.contactEmail)
	b.WriteString("# Shown on /about and on the registration form.\n")
	if len(a.rules) > 0 {
		quoted := make([]string, len(a.rules))
		for i, rule := range a.rules {
			quoted[i] = "  " + strconv.Quote(rule)
		}
		fmt.Fprintf(&b, "rules = [\n%s\n]\n", strings.Join(quoted, ",\n"))
	} else {
		b.WriteString("# rules = [\"Treat people with respect.\"]\n")
	}
	b.WriteString("# The longer \"about\" text and the terms of service are Markdown pages rather\n")
	b.WriteString("# than one-liners; write them at /admin/instance, which can also change every\n")
	b.WriteString("# field above without a restart.\n")
	b.WriteString("# The console stores its own copy and wins wherever it holds a value, falling\n")
	b.WriteString("# back to this file per field; `rules` are all-or-nothing once saved there.\n")
	return b.String()
}

// splitAndTrim turns a comma-separated prompt answer into a clean list.
func splitAndTrim(answer string) []string {
	if strings.TrimSpace(answer) == "" {
		return nil
	}
	parts := strings.Split(answer, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func validateInitLighthouses(answer string) error {
	addresses := splitAndTrim(answer)
	if len(addresses) == 0 {
		return fmt.Errorf("at least one lighthouse is required to join littleMesh")
	}
	for index, entry := range addresses {
		address, pin, pinned := strings.Cut(entry, "@")
		address = strings.TrimSpace(address)
		if pinned && !mesh.ValidNodeID(strings.ToLower(strings.TrimSpace(pin))) {
			return fmt.Errorf("%q has an invalid lighthouse node id", entry)
		}
		host, port := address, ""
		if h, p, err := net.SplitHostPort(address); err == nil {
			host = h
			port = p
		}
		host = strings.Trim(host, "[]")
		ip := net.ParseIP(host)
		if err := validateInitDomain(host); err != nil && ip == nil {
			return fmt.Errorf("%q is not a host name or address", entry)
		}
		if index == 0 && ip != nil && ip.To4() == nil {
			return fmt.Errorf("the first lighthouse needs a DNS name or IPv4 address so the mesh domain can be derived")
		}
		if index == 0 && port != "" && port != mesh.DefaultLighthousePort {
			return fmt.Errorf("the first lighthouse must use port %s so the mesh domain can be derived", mesh.DefaultLighthousePort)
		}
	}
	return nil
}

// validateInitReachLighthouses checks lighthouse addresses for an instance
// that keeps its own domain. Unlike validateInitLighthouses, no
// entry needs to derive a domain, so there is no port-7842/IPv4 restriction
// on the first one.
func validateInitReachLighthouses(answer string) error {
	addresses := splitAndTrim(answer)
	if len(addresses) == 0 {
		return fmt.Errorf("at least one lighthouse address is required")
	}
	for _, entry := range addresses {
		address, pin, pinned := strings.Cut(entry, "@")
		address = strings.TrimSpace(address)
		if pinned && !mesh.ValidNodeID(strings.ToLower(strings.TrimSpace(pin))) {
			return fmt.Errorf("%q has an invalid lighthouse node id", entry)
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			host = address
		}
		host = strings.Trim(host, "[]")
		if net.ParseIP(host) == nil {
			if err := validateInitDomain(host); err != nil {
				return fmt.Errorf("%q is not a host name or address", entry)
			}
		}
	}
	return nil
}

func validateInitDirectoryLighthouses(answer string) error {
	addresses := splitAndTrim(answer)
	if len(addresses) == 0 || len(addresses) > mesh.MaxDirectoryLighthouses {
		return fmt.Errorf("one to %d pinned public lighthouses are required", mesh.MaxDirectoryLighthouses)
	}
	for _, entry := range addresses {
		if _, err := mesh.DirectoryLocator(entry); err != nil {
			return err
		}
	}
	return nil
}

func validateInitDirectoryURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("must be an HTTPS base URL without credentials, query, or fragment")
	}
	return nil
}

func initDirectoryStatePath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "mesh-directory-client.db")
}

func exists(path string) bool { _, err := os.Lstat(path); return err == nil || !os.IsNotExist(err) }

func removeSQLiteFiles(path string) {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		_ = os.Remove(path + suffix)
	}
}

func printInitUsage(out io.Writer) {
	fmt.Fprintln(out, "Usage: littlefedi init [--config PATH]\n\nInteractively creates a fresh instance or littleMesh lighthouse.\nAn instance gets its first administrator, service actor, and VAPID keys; a lighthouse gets a relay identity.\nRefuses to overwrite an existing config or littlefedi.db.")
}

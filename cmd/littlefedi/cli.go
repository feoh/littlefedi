package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/accounts"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/app"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/auth"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/backup"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/federation"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mail"
	medialib "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

func runAdminCommand(args []string, cfg *config.Config) {
	if len(args) == 0 {
		printAdminUsage()
		return
	}

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
	// A mesh instance takes its domain from its node identity, so resolve that
	// before anything validates or uses cfg.WebDomain.
	if _, err := resolveMeshIdentity(ctx, store, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Invalid mesh configuration: %v\n", err)
		return
	}
	if err := validatePersistedWebDomain(ctx, store, cfg.WebDomain); err != nil {
		fmt.Fprintf(os.Stderr, "Invalid web domain configuration: %v\n", err)
		return
	}
	if cfg.SelfDestruct && adminCommandMutates(args) {
		fmt.Fprintln(os.Stderr, "Self-destruct mode is enabled; mutating admin commands are refused. Use `admin self-destruct` for delivery status.")
		return
	}

	mailer := mail.New(cfg.SMTP, cfg.WebDomain, cfg.LocalDomain)

	switch args[0] {
	case "self-destruct":
		if err := cmdSelfDestruct(ctx, store, cfg, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
	case "create-user":
		cmdCreateUser(ctx, store, cfg, args[1:])
	case "set-admin":
		cmdSetAdmin(ctx, store, args[1:])
	case "list-users":
		cmdListUsers(ctx, store)
	case "email-verification-report":
		cmdEmailVerificationReport(ctx, store)
	case "show-user":
		cmdShowUser(ctx, store, args[1:])
	case "reset-password":
		cmdResetPassword(ctx, store, args[1:])
	case "set-email":
		cmdSetEmail(ctx, store, args[1:])
	case "mfa-disable":
		cmdMFADisable(ctx, store, args[1:])
	case "keys":
		if err := cmdKeys(ctx, store, args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "delete-user":
		cmdDeleteUser(ctx, store, cfg, args[1:])
	case "approve":
		cmdApprove(ctx, store, mailer, args[1:])
	case "reject":
		cmdReject(ctx, store, mailer, args[1:])
	case "suspend":
		cmdSuspend(ctx, store, cfg, args[1:])
	case "unsuspend":
		cmdUnsuspend(ctx, store, cfg, args[1:])
	case "silence":
		cmdSilence(ctx, store, cfg, args[1:])
	case "unsilence":
		cmdUnsilence(ctx, store, cfg, args[1:])
	case "disable":
		cmdDisable(ctx, store, cfg, args[1:])
	case "enable":
		cmdEnable(ctx, store, cfg, args[1:])
	case "invite":
		cmdInvite(ctx, store, args[1:])
	case "domain-block":
		cmdDomainBlock(ctx, store, cfg, args[1:])
	case "domain-allow":
		cmdDomainAllow(ctx, store, args[1:])
	case "federation":
		cmdFederation(ctx, store, args[1:])
	case "commercial-bigtech":
		cmdCommercialBigTech(ctx, store, cfg, args[1:])
	case "media":
		cmdMedia(ctx, store, cfg, args[1:])
	case "blog":
		cmdBlog(ctx, store, cfg, args[1:])
	case "relay":
		if err := cmdRelay(ctx, store, cfg, args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "refresh-actors":
		cmdRefreshActors(ctx, store, cfg)
	default:
		printAdminUsage()
	}
}

func adminCommandMutates(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "self-destruct", "list-users", "show-user", "email-verification-report":
		return false
	case "keys":
		return len(args) < 2 || args[1] != "status"
	case "relay":
		return len(args) < 2 || (args[1] != "list" && args[1] != "status")
	case "media":
		return len(args) < 2 || (args[1] != "storage-status" && args[1] != "storage-manifest")
	default:
		return true
	}
}

func printAdminUsage() {
	fmt.Fprintf(os.Stderr, `Usage: littlefedi admin <command> [options]

Admin commands:
  self-destruct          Confirm decommissioning, or show delivery status when enabled.
  create-user --username <name> [--password <pw>] [--admin] [--email <addr>]
                          Create a new local user. If --password is omitted,
                          you will be prompted. If --admin is set, the user
                          is created as an administrator. Use --email to set
                          the user's email address.
  set-admin --username <name>
                          Toggle admin status for a user.
  list-users              List all local users.
  email-verification-report
                          List existing users that require_email would lock out.
  show-user --username <name>
                          Show detailed information about a local user.
  reset-password --username <name> [--password <pw>]
                          Reset a user's password. Prompts if --password omitted.
  set-email --username <name> --email <addr>
                          Set or change the email address of a local user.
  mfa-disable --username <name>
                          Disable MFA for recovery; requires local CLI access.
  keys <status|encrypt|decrypt>
                          Seal the ActivityPub private keys of local accounts at
                          rest, or return them to plaintext. Needs
                          LITTLEFEDI_FEDERATION_KEY_ENCRYPTION_KEY.
  delete-user --username <name> [--confirm]
                          Delete a local user. Tombstones, federates Delete(Person),
                          and removes all data. Requires --confirm.
  approve --username <name>
                          Approve a pending registration.
  reject --username <name>
                          Reject a pending registration (deletes the account).
  suspend --username <name>
                          Suspend a user (tombstone + federate Delete(Person)).
  unsuspend --username <name>
                          Clear suspension and reinstate a user.
  silence --username <name>
                          Silence a user (hide from public timelines).
  unsilence --username <name>
                          Remove silence from a user.
  disable --username <name>
                          Disable a local user's login without removing content.
  enable --username <name>
                          Re-enable a disabled local user.
  invite generate [--code <code>] [--uses <n>] [--expires <date>]
                          Generate an invite code. Uses: max number of uses
                          (0 = unlimited). Expires: YYYY-MM-DD.
  invite list             List all invite codes.
  invite revoke --code <code>
                          Revoke an invite code.
  domain-block create --domain <domain> [--severity silence|suspend|noop]
                          [--comment <text>] [--public] [--reject-media]
                          [--reject-reports] [--obfuscate]
                          Add an instance-wide domain block.
  domain-block list       List all domain blocks.
  domain-block delete --domain <domain>
                          Remove an instance-wide domain block.
  domain-block import --file <path>
                          Import text, generic CSV, or Mastodon CSV blocks.
  domain-allow create|list|delete|import
                          Manage limited-federation domain allows.
  federation mode open|allowlist
                          Change federation mode without restarting.
  commercial-bigtech enable|disable|refresh|status
                          Manage the official commercial Big Tech list.
  media prune             Remove orphaned media (DB records with no status) and
                          unreferenced media files (disk files with no DB record).
  media prune-orphans     Remove only orphaned media DB records.
  media prune-files       Remove only unreferenced media files from disk.
  media storage-migrate DESTINATION_CONFIG
                          Start a native, resumable owned-media migration.
  media storage-status    Show the active migration.
  media storage-cancel ID Request cancellation.
  media storage-resume ID Resume or retry a migration.
  media storage-manifest  Emit owned-media JSONL for rclone verification.
  relay add --actor <https-url>
                          Save and enable a LitePub relay actor.
  relay list             List relays and handshake states.
  relay status --actor <https-url>
  relay enable --actor <https-url>
  relay disable --actor <https-url>
  relay remove --actor <https-url>
  blog rebuild [--username <name>]
                          Regenerate a local account's static blog site now
                          (blog build only; requires [blog] enabled = true).
  blog rebuild-all       Regenerate every allowed-to-blog local account's
                          static site now (blog build only).
  refresh-actors          Queue a forced refresh of every known remote actor,
                          even if not yet due. Fetches run in the normal
                          background queue workers, not immediately.
`)
}

// LitePub relays

func cmdRelay(ctx context.Context, store storage.Storage, cfg *config.Config, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		printRelayUsage()
		return nil
	}
	switch args[0] {
	case "add", "list", "status", "enable", "disable", "remove":
	default:
		printRelayUsage()
		return nil
	}
	signer, err := federation.SetupInstanceActorWithDomains(ctx, store, cfg.LocalDomain, cfg.WebDomain)
	if err != nil {
		return fmt.Errorf("set up instance actor: %w", err)
	}
	federator := federation.NewFederatorWithDomains(store, cfg.LocalDomain, cfg.WebDomain, signer, cfg.Federation.UserAgent)
	manager := federation.NewRelayManager(store, federator, cfg.WebDomain)

	if args[0] == "list" {
		relays, err := manager.List(ctx)
		if err != nil {
			return fmt.Errorf("list relays: %w", err)
		}
		if len(relays) == 0 {
			fmt.Println("No relays configured.")
			return nil
		}
		fmt.Printf("%-60s %-22s %s\n", "Actor", "State", "Inbox")
		for _, relay := range relays {
			fmt.Printf("%-60s %-22s %s\n", relay.Account.ActorURI, relay.State, relay.Account.InboxURI)
		}
		return nil
	}

	actor, err := relayActorArg(args[1:])
	if err != nil {
		return err
	}
	switch args[0] {
	case "add":
		details, err := manager.Add(ctx, actor)
		if err != nil {
			return err
		}
		printRelayResult("Relay saved", details)
	case "status":
		details, err := manager.Status(ctx, actor)
		if err != nil {
			return err
		}
		printRelayResult("Relay status", details)
	case "enable":
		details, err := manager.Enable(ctx, actor)
		if err != nil {
			return err
		}
		printRelayResult("Relay enabled", details)
	case "disable":
		details, err := manager.Disable(ctx, actor)
		if err != nil {
			return err
		}
		printRelayResult("Relay disabled", details)
	case "remove":
		if err := manager.Remove(ctx, actor); err != nil {
			return fmt.Errorf("remove relay: %w", err)
		}
		fmt.Printf("Relay removed: %s\n", actor)
	default:
		printRelayUsage()
	}
	return nil
}

func relayActorArg(args []string) (string, error) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--actor" && i+1 < len(args) {
			if actor := strings.TrimSpace(args[i+1]); actor != "" {
				return actor, nil
			}
		}
	}
	return "", fmt.Errorf("--actor <https-url> is required")
}

func printRelayResult(prefix string, details *federation.RelayDetails) {
	fmt.Printf("%s: %s (%s)\n", prefix, details.Account.ActorURI, details.State)
}

func printRelayUsage() {
	fmt.Fprintln(os.Stderr, `Usage: littlefedi admin relay <command> [--actor <https-url>]

Commands: add, list, status, enable, disable, remove
The actor must be a remote HTTPS ActivityPub actor URL; it is not an inbox URL.`)
}

// refresh-actors

func cmdRefreshActors(ctx context.Context, store storage.Storage, cfg *config.Config) {
	refresher := federation.NewActorRefresher(
		store, nil,
		cfg.Federation.ActorRefreshInterval,
		cfg.Federation.ActorStaleAge,
		cfg.Federation.ActorRefreshOnDemandStaleAge,
		cfg.Federation.ActorRefreshLease,
		cfg.Federation.ActorRefreshBatchSize,
		cfg.Federation.ActorRefreshQueueHighWatermark,
	)
	queued, total, err := refresher.EnqueueAll(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error queueing actor refresh: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Queued %d of %d known remote actor(s) for refresh (rest already queued or leased). Workers process the queue in the background.\n", queued, total)
}

// create-user

func cmdCreateUser(ctx context.Context, store storage.Storage, cfg *config.Config, args []string) {
	username := ""
	password := ""
	email := ""
	isAdmin := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--username":
			if i+1 < len(args) {
				i++
				username = args[i]
			}
		case "--password":
			if i+1 < len(args) {
				i++
				password = args[i]
			}
		case "--email":
			if i+1 < len(args) {
				i++
				email = args[i]
			}
		case "--admin":
			isAdmin = true
		}
	}

	if username == "" {
		fmt.Fprint(os.Stderr, "Username: ")
		fmt.Scanln(&username)
		username = strings.TrimSpace(username)
	}

	if username == "" {
		fmt.Fprintln(os.Stderr, "Error: username is required")
		os.Exit(1)
	}

	// Require email when config says so (multi-user mode)
	if email == "" && cfg.IsMultiUser() && cfg.Auth.RequireEmail {
		fmt.Fprintln(os.Stderr, "Error: --email is required (email is mandatory on this instance)")
		os.Exit(1)
	}

	if email != "" {
		email = strings.ToLower(strings.TrimSpace(email))
		if err := accounts.ValidateEmail(email); err != nil {
			fmt.Fprintln(os.Stderr, "Error: invalid email address")
			os.Exit(1)
		}
		if _, err := store.GetAccountByEmail(ctx, email); err == nil {
			fmt.Fprintf(os.Stderr, "Error: email %q is already in use by another account\n", email)
			os.Exit(1)
		}
	}

	if password == "" {
		password = promptPassword("Password: ")
	} else {
		fmt.Fprintln(os.Stderr, "WARNING: Passing password via --password is visible in process listings. Use interactive prompt instead.")
	}

	if password == "" {
		fmt.Fprintln(os.Stderr, "Error: password is required")
		os.Exit(1)
	}

	account, err := accounts.BootstrapLocalAccount(ctx, store, cfg.WebDomain, username, password, isAdmin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating user: %v\n", err)
		os.Exit(1)
	}

	// Set email if provided (BootstrapLocalAccount doesn't set it)
	if email != "" {
		account.Email = email
		account.EmailVerified = true
		now := time.Now()
		account.EmailVerifiedAt = &now
		if err := store.UpdateAccount(ctx, account); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to set email: %v\n", err)
		}
	}

	fmt.Printf("User created: %s (admin=%v, approved=%v)", account.Username, account.IsAdmin, account.Approved)
	if email != "" {
		fmt.Printf(", email=%s", email)
	}
	fmt.Println()
}

// set-admin

func cmdSetAdmin(ctx context.Context, store storage.Storage, args []string) {
	username := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--username":
			if i+1 < len(args) {
				i++
				username = args[i]
			}
		}
	}

	if username == "" {
		fmt.Fprint(os.Stderr, "Username: ")
		fmt.Scanln(&username)
		username = strings.TrimSpace(username)
	}

	if username == "" {
		fmt.Fprintln(os.Stderr, "Error: username is required")
		os.Exit(1)
	}

	account, err := store.GetAccountByUsernameDomain(ctx, username, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: user %q not found\n", username)
		os.Exit(1)
	}

	newState := !account.IsAdmin
	if err := store.SetAdmin(ctx, account.ID, newState); err != nil {
		fmt.Fprintf(os.Stderr, "Error setting admin: %v\n", err)
		os.Exit(1)
	}

	if newState {
		fmt.Printf("User %q is now an admin\n", username)
	} else {
		fmt.Printf("User %q is no longer an admin\n", username)
	}
}

// list-users

func cmdListUsers(ctx context.Context, store storage.Storage) {
	accounts, err := store.GetLocalAccounts(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing users: %v\n", err)
		os.Exit(1)
	}

	if len(accounts) == 0 {
		fmt.Println("No local users found.")
		return
	}

	fmt.Printf("%-30s %-30s %-20s %-8s %-8s %-8s\n", "Username", "Email", "Actor URI", "Admin", "Approved", "Suspended")
	fmt.Println(strings.Repeat("-", 110))
	for _, a := range accounts {
		suspended := ""
		if a.SuspendedAt != nil {
			suspended = "yes"
		}
		fmt.Printf("%-30s %-30s %-20s %-8t %-8t %-8s\n",
			a.Username, a.Email, a.ActorURI, a.IsAdmin, a.Approved, suspended)
	}
	fmt.Printf("\nTotal: %d user(s)\n", len(accounts))
}

func cmdEmailVerificationReport(ctx context.Context, store storage.Storage) {
	local, err := store.GetLocalAccounts(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing users: %v\n", err)
		return
	}
	count := 0
	for _, account := range local {
		if account.CanAuthenticate(false) && !account.CanAuthenticate(true) {
			fmt.Printf("%s\t%s\tverified=%t\n", account.Username, account.Email, account.EmailVerified)
			count++
		}
	}
	if count == 0 {
		fmt.Println("All otherwise-active local accounts satisfy require_email.")
	}
}

func cmdMFADisable(ctx context.Context, store storage.Storage, args []string) {
	username := ""
	for i := range args {
		if args[i] == "--username" && i+1 < len(args) {
			username = args[i+1]
		}
	}
	account, err := store.GetAccountByUsernameDomain(ctx, strings.TrimSpace(username), nil)
	if err != nil || account == nil {
		fmt.Fprintf(os.Stderr, "Error: user %q not found\n", username)
		return
	}
	if err := store.DisableAccountMFA(ctx, account.ID); err != nil {
		fmt.Fprintf(os.Stderr, "Error disabling MFA: %v\n", err)
		return
	}
	if err := store.CreateModerationLog(ctx, &models.ModerationLog{AdminAccountID: account.ID, TargetType: "account", TargetID: account.ID, Action: "mfa_disable_cli", Note: "local operator recovery"}); err != nil {
		slog.Error("MFA audit failed", "account_id", account.ID, "action", "mfa_disable_cli", "error", err)
	}
	fmt.Printf("MFA disabled for %q; recovery codes, sessions, OAuth tokens and push subscriptions revoked.\n", account.Username)
}

// show-user

func cmdShowUser(ctx context.Context, store storage.Storage, args []string) {
	username := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--username":
			if i+1 < len(args) {
				i++
				username = args[i]
			}
		}
	}

	if username == "" {
		fmt.Fprint(os.Stderr, "Username: ")
		fmt.Scanln(&username)
		username = strings.TrimSpace(username)
	}

	if username == "" {
		fmt.Fprintln(os.Stderr, "Error: username is required")
		os.Exit(1)
	}

	account, err := store.GetAccountByUsernameDomain(ctx, username, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: user %q not found\n", username)
		os.Exit(1)
	}

	fmt.Printf("Username:       %s\n", account.Username)
	fmt.Printf("Display Name:   %s\n", account.DisplayName)
	fmt.Printf("Account ID:     %s\n", account.ID)
	fmt.Printf("Actor URI:      %s\n", account.ActorURI)
	fmt.Printf("Admin:          %v\n", account.IsAdmin)
	fmt.Printf("Approved:       %v\n", account.Approved)
	if account.ApprovedAt != nil {
		fmt.Printf("Approved At:    %s\n", account.ApprovedAt.Format(time.RFC3339))
	}
	fmt.Printf("Moderation:     %s\n", moderationLabel(account))
	if account.ModerationNote != "" {
		fmt.Printf("Mod. Note:      %s\n", account.ModerationNote)
	}
	if account.SuspendedAt != nil {
		fmt.Printf("Suspended At:   %s\n", account.SuspendedAt.Format(time.RFC3339))
	}
	fmt.Printf("Created:        %s\n", account.CreatedAt.Format(time.RFC3339))
	fmt.Printf("Updated:        %s\n", account.UpdatedAt.Format(time.RFC3339))
	if account.Email != "" {
		fmt.Printf("Email:          %s\n", account.Email)
	}
	fmt.Printf("Followers:      %d\n", account.FollowersCount)
	fmt.Printf("Following:      %d\n", account.FollowingCount)
	fmt.Printf("Statuses:       %d\n", account.StatusesCount)
}

func moderationLabel(a *models.Account) string {
	if a.ModerationState != "" {
		return a.ModerationState
	}
	if a.SuspendedAt != nil {
		return "suspended (legacy)"
	}
	return "active"
}

// reset-password

func cmdResetPassword(ctx context.Context, store storage.Storage, args []string) {
	username := ""
	password := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--username":
			if i+1 < len(args) {
				i++
				username = args[i]
			}
		case "--password":
			if i+1 < len(args) {
				i++
				password = args[i]
			}
		}
	}

	if username == "" {
		fmt.Fprint(os.Stderr, "Username: ")
		fmt.Scanln(&username)
		username = strings.TrimSpace(username)
	}

	if username == "" {
		fmt.Fprintln(os.Stderr, "Error: username is required")
		os.Exit(1)
	}

	account, err := store.GetAccountByUsernameDomain(ctx, username, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: user %q not found\n", username)
		os.Exit(1)
	}

	if password == "" {
		password = promptPassword("New password: ")
	} else {
		fmt.Fprintln(os.Stderr, "WARNING: Passing password via --password is visible in process listings.")
	}

	if password == "" {
		fmt.Fprintln(os.Stderr, "Error: password is required")
		os.Exit(1)
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error hashing password: %v\n", err)
		os.Exit(1)
	}

	account.PasswordHash = hash
	if err := store.UpdateAccount(ctx, account); err != nil {
		fmt.Fprintf(os.Stderr, "Error updating password: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Password updated for user %q\n", username)
}

// set-email

func cmdSetEmail(ctx context.Context, store storage.Storage, args []string) {
	username := ""
	email := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--username":
			if i+1 < len(args) {
				i++
				username = args[i]
			}
		case "--email":
			if i+1 < len(args) {
				i++
				email = args[i]
			}
		}
	}

	if username == "" {
		fmt.Fprintln(os.Stderr, "Error: --username is required")
		os.Exit(1)
	}
	if email == "" {
		fmt.Fprintln(os.Stderr, "Error: --email is required")
		os.Exit(1)
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if err := accounts.ValidateEmail(email); err != nil {
		fmt.Fprintln(os.Stderr, "Error: invalid email address")
		os.Exit(1)
	}

	account, err := store.GetAccountByUsernameDomain(ctx, username, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: user %q not found\n", username)
		os.Exit(1)
	}
	if existing, lookupErr := store.GetAccountByEmail(ctx, email); lookupErr == nil && existing.ID != account.ID {
		fmt.Fprintf(os.Stderr, "Error: email %q is already in use by another account\n", email)
		os.Exit(1)
	}

	account.Email = email
	account.EmailVerified = true
	now := time.Now()
	account.EmailVerifiedAt = &now
	if err := store.UpdateAccount(ctx, account); err != nil {
		fmt.Fprintf(os.Stderr, "Error updating email: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Email for user %q set to %s\n", username, email)
}

// delete-user

func cmdDeleteUser(ctx context.Context, store storage.Storage, cfg *config.Config, args []string) {
	username := ""
	confirmed := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--username":
			if i+1 < len(args) {
				i++
				username = args[i]
			}
		case "--confirm":
			confirmed = true
		}
	}

	if username == "" {
		fmt.Fprint(os.Stderr, "Username: ")
		fmt.Scanln(&username)
		username = strings.TrimSpace(username)
	}

	if username == "" {
		fmt.Fprintln(os.Stderr, "Error: username is required")
		os.Exit(1)
	}

	account, err := store.GetAccountByUsernameDomain(ctx, username, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: user %q not found\n", username)
		os.Exit(1)
	}

	if !confirmed {
		fmt.Fprintf(os.Stderr, "WARNING: This will disable user %q (%s), durably deliver Delete(Person), then permanently purge their data.\n", username, account.ID)
		fmt.Fprintf(os.Stderr, "Deletion cannot be cancelled after it starts. The signing key is retained only through the delivery deadline.\n")
		fmt.Fprintf(os.Stderr, "Add --confirm to proceed.\n")
		os.Exit(0)
	}

	admins, err := store.GetAdmins(ctx)
	if err != nil || len(admins) == 0 {
		fmt.Fprintln(os.Stderr, "Error: no admin user found to perform the deletion")
		os.Exit(1)
	}
	admin := admins[0]

	svc, wait := createCLIService(ctx, store, cfg)
	defer wait()

	run, created, err := svc.RequestAccountDeletion(ctx, admin, account)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error requesting account deletion: %v\n", err)
		os.Exit(1)
	}
	if created {
		fmt.Printf("Deletion requested for %q: %d destination(s), deadline %s.\n", username, run.DestinationCount, run.DeadlineAt.Format(time.RFC3339))
	} else {
		fmt.Printf("Deletion for %q is already %s (%d/%d delivered).\n", username, run.State, run.DeliveredCount, run.DestinationCount)
	}
}

// approve / reject

func cmdApprove(ctx context.Context, store storage.Storage, mailer *mail.Mailer, args []string) {
	username := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--username":
			if i+1 < len(args) {
				i++
				username = args[i]
			}
		}
	}

	if username == "" {
		fmt.Fprint(os.Stderr, "Username: ")
		fmt.Scanln(&username)
		username = strings.TrimSpace(username)
	}

	if username == "" {
		fmt.Fprintln(os.Stderr, "Error: username is required")
		os.Exit(1)
	}

	account, err := store.GetAccountByUsernameDomain(ctx, username, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: user %q not found\n", username)
		os.Exit(1)
	}

	if account.Approved {
		fmt.Fprintf(os.Stderr, "Error: user %q is already approved\n", username)
		os.Exit(1)
	}

	if err := store.ApproveAccount(ctx, account.ID); err != nil {
		fmt.Fprintf(os.Stderr, "Error approving user: %v\n", err)
		os.Exit(1)
	}

	if mailer != nil && account.Email != "" {
		_ = mailer.SendApprovalGranted(account.Locale, account.Email, account.Username)
	}

	fmt.Printf("User %q approved\n", username)
}

func cmdReject(ctx context.Context, store storage.Storage, mailer *mail.Mailer, args []string) {
	username := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--username":
			if i+1 < len(args) {
				i++
				username = args[i]
			}
		}
	}

	if username == "" {
		fmt.Fprint(os.Stderr, "Username: ")
		fmt.Scanln(&username)
		username = strings.TrimSpace(username)
	}

	if username == "" {
		fmt.Fprintln(os.Stderr, "Error: username is required")
		os.Exit(1)
	}

	account, err := store.GetAccountByUsernameDomain(ctx, username, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: user %q not found\n", username)
		os.Exit(1)
	}

	if account.Approved {
		fmt.Fprintf(os.Stderr, "Error: user %q is already approved - use delete-user to remove\n", username)
		os.Exit(1)
	}

	if err := store.RejectAccount(ctx, account.ID); err != nil {
		fmt.Fprintf(os.Stderr, "Error rejecting user: %v\n", err)
		os.Exit(1)
	}

	if mailer != nil && account.Email != "" {
		_ = mailer.SendApprovalRejected(account.Locale, account.Email, account.Username)
	}

	fmt.Printf("User %q rejected and removed\n", username)
}

// suspend / unsuspend

func cmdSuspend(ctx context.Context, store storage.Storage, cfg *config.Config, args []string) {
	moderateCLI(ctx, store, cfg, args, "suspend", "suspended")
}

func cmdUnsuspend(ctx context.Context, store storage.Storage, cfg *config.Config, args []string) {
	moderateCLI(ctx, store, cfg, args, "none", "unsuspended")
}

func cmdSilence(ctx context.Context, store storage.Storage, cfg *config.Config, args []string) {
	moderateCLI(ctx, store, cfg, args, "silence", "silenced")
}

func cmdUnsilence(ctx context.Context, store storage.Storage, cfg *config.Config, args []string) {
	moderateCLI(ctx, store, cfg, args, "none", "unsilenced")
}

func cmdDisable(ctx context.Context, store storage.Storage, cfg *config.Config, args []string) {
	moderateCLI(ctx, store, cfg, args, "disable", "disabled")
}

func cmdEnable(ctx context.Context, store storage.Storage, cfg *config.Config, args []string) {
	moderateCLI(ctx, store, cfg, args, "none", "enabled")
}

// moderateCLI is the shared implementation for all moderation-style commands.
// It parses --username, resolves the target account and the first admin,
// creates the app service, and calls ModerateAccount.
func moderateCLI(ctx context.Context, store storage.Storage, cfg *config.Config, args []string, action, description string) {
	username := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--username":
			if i+1 < len(args) {
				i++
				username = args[i]
			}
		}
	}

	if username == "" {
		fmt.Fprint(os.Stderr, "Username: ")
		fmt.Scanln(&username)
		username = strings.TrimSpace(username)
	}

	if username == "" {
		fmt.Fprintln(os.Stderr, "Error: username is required")
		os.Exit(1)
	}

	account, err := store.GetAccountByUsernameDomain(ctx, username, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: user %q not found\n", username)
		os.Exit(1)
	}

	admins, err := store.GetAdmins(ctx)
	if err != nil || len(admins) == 0 {
		fmt.Fprintln(os.Stderr, "Error: no admin user found to perform moderation")
		os.Exit(1)
	}

	svc, wait := createCLIService(ctx, store, cfg)
	defer wait()

	if err := svc.ModerateAccount(ctx, admins[0], account, action, "CLI admin command"); err != nil {
		fmt.Fprintf(os.Stderr, "Error moderating user: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("User %q %s\n", username, description)
}

// domain-block

func cmdDomainBlock(ctx context.Context, store storage.Storage, cfg *config.Config, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: littlefedi admin domain-block [create|list|delete|import]")
		os.Exit(1)
	}

	switch args[0] {
	case "create":
		cmdDomainBlockCreate(ctx, store, cfg, args[1:])
	case "list":
		cmdDomainBlockList(ctx, store)
	case "delete":
		cmdDomainBlockDelete(ctx, store, args[1:])
	case "import":
		path := flagValue(args[1:], "--file")
		file, err := os.Open(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening import: %v\n", err)
			return
		}
		defer file.Close()
		blocks, err := federation.ParseDomainBlockImport(file)
		if err == nil {
			var n int
			n, err = store.ImportDomainBlocks(ctx, blocks)
			if err == nil {
				fmt.Printf("Imported %d domain block(s)\n", n)
				federationAuditCLI(ctx, store, "domain_blocks", "import", "")
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error importing domain blocks: %v\n", err)
		}
	default:
		fmt.Fprintf(os.Stderr, "Unknown domain-block subcommand: %s\n", args[0])
		os.Exit(1)
	}
}

func cmdDomainBlockCreate(ctx context.Context, store storage.Storage, cfg *config.Config, args []string) {
	domain := ""
	severity := "silence"
	comment := ""
	public := false
	rejectMedia := false
	rejectReports := false
	obfuscate := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--domain":
			if i+1 < len(args) {
				i++
				domain = args[i]
			}
		case "--severity":
			if i+1 < len(args) {
				i++
				severity = args[i]
			}
		case "--comment":
			if i+1 < len(args) {
				i++
				comment = args[i]
			}
		case "--public":
			public = true
		case "--reject-media":
			rejectMedia = true
		case "--reject-reports":
			rejectReports = true
		case "--obfuscate":
			obfuscate = true
		}
	}

	if domain == "" {
		fmt.Fprint(os.Stderr, "Domain: ")
		fmt.Scanln(&domain)
		domain = strings.TrimSpace(domain)
	}

	if domain == "" {
		fmt.Fprintln(os.Stderr, "Error: domain is required")
		os.Exit(1)
	}

	switch severity {
	case "silence", "suspend", "noop":
	default:
		fmt.Fprintf(os.Stderr, "Error: severity must be silence, suspend, or noop (got %q)\n", severity)
		os.Exit(1)
	}

	block := &models.DomainBlock{
		Domain:        domain,
		Severity:      severity,
		Public:        public,
		Comment:       comment,
		RejectMedia:   rejectMedia,
		RejectReports: rejectReports,
		Obfuscate:     obfuscate,
	}

	if err := store.CreateDomainBlock(ctx, block); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating domain block: %v\n", err)
		os.Exit(1)
	}

	admins, err := store.GetAdmins(ctx)
	if err == nil && len(admins) > 0 {
		_ = store.CreateModerationLog(ctx, &models.ModerationLog{
			AdminAccountID: admins[0].ID,
			TargetType:     "domain",
			TargetID:       block.Domain,
			Action:         "block_" + severity,
			Note:           comment,
		})
	}

	fmt.Printf("Domain block created: %s (severity=%s, public=%v, reject_media=%v, reject_reports=%v, obfuscate=%v)\n",
		block.Domain, severity, public, rejectMedia, rejectReports, obfuscate)
}

func cmdDomainBlockList(ctx context.Context, store storage.Storage) {
	blocks, err := store.ListDomainBlocks(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing domain blocks: %v\n", err)
		os.Exit(1)
	}

	if len(blocks) == 0 {
		fmt.Println("No domain blocks found.")
		return
	}

	fmt.Printf("%-30s %-10s %-8s %-8s %-8s %-8s %s\n",
		"Domain", "Severity", "Public", "Rej.Med", "Rej.Rep", "Obfusc", "Comment")
	fmt.Println(strings.Repeat("-", 90))
	for _, b := range blocks {
		fmt.Printf("%-30s %-10s %-8t %-8t %-8t %-8t %s\n",
			b.Domain, b.Severity, b.Public, b.RejectMedia, b.RejectReports, b.Obfuscate, b.Comment)
	}
	fmt.Printf("\nTotal: %d domain block(s)\n", len(blocks))
}

func cmdDomainBlockDelete(ctx context.Context, store storage.Storage, args []string) {
	domain := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--domain":
			if i+1 < len(args) {
				i++
				domain = args[i]
			}
		}
	}

	if domain == "" {
		fmt.Fprint(os.Stderr, "Domain: ")
		fmt.Scanln(&domain)
		domain = strings.TrimSpace(domain)
	}

	if domain == "" {
		fmt.Fprintln(os.Stderr, "Error: domain is required")
		os.Exit(1)
	}

	block, err := store.GetDomainBlock(ctx, domain)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: no domain block found for %q\n", domain)
		os.Exit(1)
	}

	if err := store.DeleteDomainBlock(ctx, block.ID); err != nil {
		fmt.Fprintf(os.Stderr, "Error deleting domain block: %v\n", err)
		os.Exit(1)
	}

	admins, err := store.GetAdmins(ctx)
	if err == nil && len(admins) > 0 {
		_ = store.CreateModerationLog(ctx, &models.ModerationLog{
			AdminAccountID: admins[0].ID,
			TargetType:     "domain",
			TargetID:       domain,
			Action:         "unblock_domain",
			Note:           "CLI admin command",
		})
	}

	fmt.Printf("Domain block removed: %s\n", domain)
}

func flagValue(args []string, name string) string {
	for i := range args {
		if args[i] == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func federationAuditCLI(ctx context.Context, store storage.Storage, target, action, note string) {
	admins, err := store.GetAdmins(ctx)
	if err == nil && len(admins) > 0 {
		_ = store.CreateModerationLog(ctx, &models.ModerationLog{AdminAccountID: admins[0].ID, TargetType: "federation_policy", TargetID: target, Action: action, Note: note})
	}
}

func cmdFederation(ctx context.Context, store storage.Storage, args []string) {
	if len(args) != 2 || args[0] != "mode" {
		fmt.Fprintln(os.Stderr, "Usage: littlefedi admin federation mode open|allowlist")
		return
	}
	if err := store.SetFederationMode(ctx, args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	federationAuditCLI(ctx, store, "mode", "set_"+args[1], "")
	fmt.Printf("Federation mode: %s\n", args[1])
}

func cmdDomainAllow(ctx context.Context, store storage.Storage, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: littlefedi admin domain-allow create|list|delete|import")
		return
	}
	switch args[0] {
	case "create":
		allow := &models.DomainAllow{Domain: flagValue(args[1:], "--domain")}
		if err := store.CreateDomainAllow(ctx, allow); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return
		}
		federationAuditCLI(ctx, store, allow.Domain, "allow", "")
		fmt.Printf("Domain allowed: %s\n", allow.Domain)
	case "list":
		allows, err := store.ListDomainAllows(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return
		}
		for _, allow := range allows {
			fmt.Println(allow.Domain)
		}
	case "delete":
		domain := strings.ToLower(strings.TrimSuffix(flagValue(args[1:], "--domain"), "."))
		allows, err := store.ListDomainAllows(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return
		}
		for _, allow := range allows {
			if allow.Domain != domain {
				continue
			}
			if err := store.DeleteDomainAllow(ctx, allow.ID); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				return
			}
			federationAuditCLI(ctx, store, domain, "unallow", "")
			fmt.Printf("Domain allow removed: %s\n", domain)
			return
		}
		fmt.Fprintf(os.Stderr, "Error: no domain allow found for %q\n", domain)
	case "import":
		file, err := os.Open(flagValue(args[1:], "--file"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening import: %v\n", err)
			return
		}
		defer file.Close()
		allows, err := federation.ParseDomainAllowImport(file)
		if err == nil {
			var n int
			n, err = store.ImportDomainAllows(ctx, allows)
			if err == nil {
				fmt.Printf("Imported %d domain allow(s)\n", n)
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error importing domain allows: %v\n", err)
		} else {
			federationAuditCLI(ctx, store, "domain_allows", "import", "")
		}
	default:
		fmt.Fprintf(os.Stderr, "Unknown domain-allow subcommand: %s\n", args[0])
	}
}

func cmdCommercialBigTech(ctx context.Context, store storage.Storage, cfg *config.Config, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: littlefedi admin commercial-bigtech enable|disable|refresh|status")
		return
	}
	state, err := store.GetFederationPolicyState(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	switch args[0] {
	case "disable":
		err = store.SetCommercialBigTechEnabled(ctx, false)
	case "refresh", "enable":
		var syncer *federation.BigTechListSyncer
		syncer, err = federation.NewBigTechListSyncer(store, cfg.Federation.CommercialBigTechListURL)
		if err == nil {
			err = syncer.Refresh(ctx)
		}
		if err == nil && args[0] == "enable" {
			err = store.SetCommercialBigTechEnabled(ctx, true)
			if err == nil && cfg.BackgroundMaintenance {
				err = store.EnqueueJob(ctx, &models.Job{Kind: federation.CommercialBigTechRefreshJobKind, Payload: "{}", RunAt: time.Now().Add(24 * time.Hour), MaxAttempts: 3, Priority: -10})
			}
		}
	case "status":
		blocks, listErr := store.ListCommercialDomainBlocks(ctx)
		if listErr != nil {
			err = listErr
			break
		}
		fmt.Printf("enabled=%t entries=%d source=%s last_attempt=%v last_success=%v error=%s\n", state.CommercialBigTechEnabled, len(blocks), state.SourceURL, state.LastAttempt, state.LastSuccess, state.LastError)
		return
	default:
		fmt.Fprintf(os.Stderr, "Unknown commercial-bigtech subcommand: %s\n", args[0])
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	federationAuditCLI(ctx, store, "commercial_bigtech", args[0], "")
	fmt.Printf("Commercial Big Tech: %s\n", args[0])
}

// invite

func cmdInvite(ctx context.Context, store storage.Storage, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: littlefedi admin invite [generate|list|revoke]")
		os.Exit(1)
	}

	switch args[0] {
	case "generate":
		cmdInviteGenerate(ctx, store, args[1:])
	case "list":
		cmdInviteList(ctx, store)
	case "revoke":
		cmdInviteRevoke(ctx, store, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown invite subcommand: %s\n", args[0])
		os.Exit(1)
	}
}

func cmdInviteGenerate(ctx context.Context, store storage.Storage, args []string) {
	code := ""
	maxUses := 0
	expiresStr := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--code":
			if i+1 < len(args) {
				i++
				code = args[i]
			}
		case "--uses":
			if i+1 < len(args) {
				i++
				fmt.Sscanf(args[i], "%d", &maxUses)
			}
		case "--expires":
			if i+1 < len(args) {
				i++
				expiresStr = args[i]
			}
		}
	}

	var expiresAt *time.Time
	if expiresStr != "" {
		t, err := time.Parse("2006-01-02", expiresStr)
		if err == nil {
			expiresAt = &t
		} else {
			fmt.Fprintf(os.Stderr, "Warning: could not parse expiry date %q, ignoring\n", expiresStr)
		}
	}

	admins, err := store.GetAdmins(ctx)
	if err != nil || len(admins) == 0 {
		fmt.Fprintln(os.Stderr, "Error: no admin user found to own the invite. Create an admin first.")
		os.Exit(1)
	}

	invite, err := accounts.CreateInvite(ctx, store, admins[0].ID, code, maxUses, expiresAt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating invite: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Invite created: code=%q max_uses=%d\n", invite.Code, invite.MaxUses)
	if expiresAt != nil {
		fmt.Printf("  Expires: %s\n", expiresAt.Format("2006-01-02"))
	}
}

func cmdInviteList(ctx context.Context, store storage.Storage) {
	invites, err := store.ListInvites(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing invites: %v\n", err)
		os.Exit(1)
	}

	if len(invites) == 0 {
		fmt.Println("No invite codes found.")
		return
	}

	fmt.Printf("%-30s %-10s %-10s %-12s %s\n", "Code", "Max Uses", "Used", "Expires", "Created")
	fmt.Println(strings.Repeat("-", 80))
	for _, inv := range invites {
		expires := "never"
		if inv.ExpiresAt != nil {
			expires = inv.ExpiresAt.Format("2006-01-02")
		}
		maxUsesStr := fmt.Sprintf("%d", inv.MaxUses)
		if inv.MaxUses <= 0 {
			maxUsesStr = "unlimited"
		}
		fmt.Printf("%-30s %-10s %-10d %-12s %s\n",
			inv.Code, maxUsesStr, inv.UseCount, expires, inv.CreatedAt.Format("2006-01-02"))
	}
}

func cmdInviteRevoke(ctx context.Context, store storage.Storage, args []string) {
	code := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--code":
			if i+1 < len(args) {
				i++
				code = args[i]
			}
		}
	}

	if code == "" {
		fmt.Fprint(os.Stderr, "Invite code to revoke: ")
		fmt.Scanln(&code)
		code = strings.TrimSpace(code)
	}

	if code == "" {
		fmt.Fprintln(os.Stderr, "Error: invite code is required")
		os.Exit(1)
	}

	invite, err := store.GetInviteByCode(ctx, code)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: invite %q not found\n", code)
		os.Exit(1)
	}

	if err := store.DeleteInvite(ctx, invite.ID); err != nil {
		fmt.Fprintf(os.Stderr, "Error revoking invite: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Invite %q revoked\n", code)
}

// media

func cmdBlog(ctx context.Context, store storage.Storage, cfg *config.Config, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: littlefedi admin blog rebuild|rebuild-all|allow|disallow [--username <name>]")
		os.Exit(1)
	}
	if handleBlogCommand(ctx, store, cfg, args) {
		return
	}
	fmt.Fprintf(os.Stderr, "Unknown blog subcommand: %s\n", args[0])
	os.Exit(1)
}

func cmdMedia(ctx context.Context, store storage.Storage, cfg *config.Config, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: littlefedi admin media [prune|prune-orphans|prune-files]")
		os.Exit(1)
	}

	if handleMediaStorageCommand(ctx, store, cfg, args) {
		return
	}
	switch args[0] {
	case "prune":
		cmdMediaPrune(ctx, store, cfg)
	case "prune-orphans":
		cmdMediaPruneOrphans(ctx, store, cfg)
	case "prune-files":
		cmdMediaPruneFiles(ctx, store, cfg)
	default:
		fmt.Fprintf(os.Stderr, "Unknown media subcommand: %s\n", args[0])
		os.Exit(1)
	}
}

func cmdMediaPrune(ctx context.Context, store storage.Storage, cfg *config.Config) {
	fmt.Println("Pruning orphaned media attachments (DB records with no status)...")
	orphanedCount, err := medialib.RemoveOrphanedMedia(ctx, store, cfg.Media.Path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error removing orphaned media: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Removed %d orphaned media attachment(s)\n", orphanedCount)

	fmt.Println("Pruning unreferenced media files (disk files with no DB record)...")
	fileCount, err := medialib.RemoveUnreferencedMediaFromDisk(ctx, store, cfg.Media.Path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error removing unreferenced media files: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Removed %d unreferenced media directory(ies)\n", fileCount)

	fmt.Println("Media cleanup complete.")
}

func cmdMediaPruneOrphans(ctx context.Context, store storage.Storage, cfg *config.Config) {
	fmt.Println("Pruning orphaned media attachments (DB records with no status)...")
	orphanedCount, err := medialib.RemoveOrphanedMedia(ctx, store, cfg.Media.Path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error removing orphaned media: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Removed %d orphaned media attachment(s)\n", orphanedCount)
}

func cmdMediaPruneFiles(ctx context.Context, store storage.Storage, cfg *config.Config) {
	fmt.Println("Pruning unreferenced media files (disk files with no DB record)...")
	fileCount, err := medialib.RemoveUnreferencedMediaFromDisk(ctx, store, cfg.Media.Path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error removing unreferenced media files: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Removed %d unreferenced media directory(ies)\n", fileCount)
}

// backup commands

func runBackupCommand(args []string, cfg *config.Config) {
	if len(args) == 0 {
		printBackupUsage()
		return
	}

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
	if _, err := resolveMeshIdentity(ctx, store, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Invalid mesh configuration: %v\n", err)
		return
	}
	if err := validatePersistedWebDomain(ctx, store, cfg.WebDomain); err != nil {
		fmt.Fprintf(os.Stderr, "Invalid web domain configuration: %v\n", err)
		return
	}

	switch args[0] {
	case "now":
		includeMedia := cfg.Backup.IncludeMedia
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--media":
				includeMedia = true
			case "--no-media":
				includeMedia = false
			}
		}
		backupCfg := *cfg
		backupCfg.Backup.IncludeMedia = includeMedia
		path, err := backup.CreateBackup(ctx, store, &backupCfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Backup failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Backup complete: %s\n", path)
	case "restore":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: littlefedi backup restore <path>")
			os.Exit(1)
		}
		restorePath := args[1]
		if _, err := os.Stat(restorePath); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Backup directory not found: %s\n", restorePath)
			os.Exit(1)
		}
		if err := backup.RestoreBackup(ctx, restorePath, store, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "Restore failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Restore complete. Restart the server to use the restored database.")
	default:
		printBackupUsage()
	}
}

func printBackupUsage() {
	fmt.Fprintf(os.Stderr, `Usage: littlefedi backup <command> [options]

Backup commands:
  now [--media|--no-media]  Create a backup (config, database, optionally media).
  restore <path>            Restore a backup from the given directory.

Configuration (littlefedi.toml):
  [backup]
  enabled = true        # Enable periodic backups when the server is running
  path = "backups"      # Output directory for backups
  interval = "24h"      # How often to run periodic backups
  include_media = false # Include owned media in backups (cache is always excluded)
  retain = 7            # Number of backups to keep (0 = unlimited)
`)
}

// Helpers

func createCLIService(ctx context.Context, store storage.Storage, cfg *config.Config) (*app.Service, func()) {
	signer, err := federation.SetupInstanceActorWithDomains(ctx, store, cfg.LocalDomain, cfg.WebDomain)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error setting up instance actor: %v\n", err)
		os.Exit(1)
	}
	federator := federation.NewFederatorWithDomains(store, cfg.LocalDomain, cfg.WebDomain, signer, cfg.Federation.UserAgent)
	svc := app.NewWithDomains(store, federator, cfg.LocalDomain, cfg.WebDomain)
	svc.SetBlockSensitiveRelationshipNotifications(cfg.Instance.BlockSensitiveRelationshipNotifications)
	svc.SetImmediateDeliveries(true)
	return svc, svc.WaitForDeliveries
}

func promptPassword(prompt string) string {
	fmt.Fprint(os.Stderr, prompt)

	password, err := readPassword()
	if err != nil {
		var pw string
		fmt.Scanln(&pw)
		return strings.TrimSpace(pw)
	}
	fmt.Fprintln(os.Stderr)
	return strings.TrimSpace(password)
}

func readPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	if !isTerminal(fd) {
		return readLine(), nil
	}
	return readTerminalPassword(fd)
}

func isTerminal(fd int) bool {
	_, err := getTermState(fd)
	return err == nil
}

func getTermState(fd int) (interface{}, error) {
	return getTermios(fd)
}

func readLine() string {
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		return scanner.Text()
	}
	return ""
}

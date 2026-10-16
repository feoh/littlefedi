package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/app"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/content"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/federation"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

type postOptions struct {
	username       string
	file           string
	visibility     models.Visibility
	contentType    string
	summary        string
	sensitive      bool
	language       string
	inReplyToID    string
	quotedStatusID string
	mediaIDs       stringListFlag
	blog           bool
	blogSlug       string
}

type stringListFlag []string

func (s *stringListFlag) String() string { return strings.Join(*s, ",") }
func (s *stringListFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("value cannot be empty")
	}
	*s = append(*s, value)
	return nil
}

func hasHelpFlag(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

func printPostUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: littlefedi [--config <path>] post [options] [file]

Publish a post as a local user. With no file, or with file "-", content is
read from standard input.

Options:
  --username <name>          Author (optional in single-user instances)
  --file <path>              Read content from path instead of stdin
  --visibility <value>       public, unlisted, private, direct, local, or
                             local_unlisted (default: public)
  --markdown                 Treat input as Markdown
  --content-type <type>      plain, markdown, text/plain, or text/markdown
  --content-warning <text>   Add a content warning (alias: --cw)
  --sensitive                Mark attached media/content as sensitive
  --language <code>          Content language (default: en)
  --reply-to <status-id>     Reply to a status already known by this instance
  --quote <status-id>        Quote a status already known by this instance
  --media <attachment-id>    Attach an existing media ID; may be repeated
  --blog                     Flag this post as a blog post (requires -tags blog)
  --blog-slug <path>         Custom permalink for this blog post, e.g.
                             "2026/07/25/hello" (default: instance/account
                             slug format)
  -h, --help                 Show this help
`)
}

func parsePostOptions(args []string) (postOptions, error) {
	opts := postOptions{visibility: models.VisibilityPublic, contentType: content.ContentTypePlain, language: "en"}
	fs := flag.NewFlagSet("post", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var markdown bool
	fs.StringVar(&opts.username, "username", "", "author username")
	fs.StringVar(&opts.file, "file", "", "input file")
	visibility := fs.String("visibility", string(opts.visibility), "post visibility")
	fs.StringVar(&opts.contentType, "content-type", opts.contentType, "source content type")
	fs.BoolVar(&markdown, "markdown", false, "treat input as Markdown")
	fs.StringVar(&opts.summary, "content-warning", "", "content warning")
	fs.StringVar(&opts.summary, "cw", "", "content warning")
	fs.BoolVar(&opts.sensitive, "sensitive", false, "mark content as sensitive")
	fs.StringVar(&opts.language, "language", opts.language, "content language")
	fs.StringVar(&opts.inReplyToID, "reply-to", "", "status ID to reply to")
	fs.StringVar(&opts.quotedStatusID, "quote", "", "status ID to quote")
	fs.Var(&opts.mediaIDs, "media", "media attachment ID (repeatable)")
	fs.BoolVar(&opts.blog, "blog", false, "flag this post as a blog post (requires -tags blog)")
	fs.StringVar(&opts.blogSlug, "blog-slug", "", "custom permalink for this blog post")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}

	positional := fs.Args()
	if len(positional) > 1 {
		return opts, fmt.Errorf("expected at most one input file")
	}
	if len(positional) == 1 {
		if opts.file != "" {
			return opts, fmt.Errorf("input file specified both positionally and with --file")
		}
		opts.file = positional[0]
	}

	vis, err := parseVisibility(*visibility)
	if err != nil {
		return opts, err
	}
	opts.visibility = vis

	if opts.blogSlug != "" && !opts.blog {
		return opts, fmt.Errorf("--blog-slug requires --blog")
	}
	if opts.blog && opts.inReplyToID != "" {
		return opts, fmt.Errorf("--blog cannot be combined with --reply-to (blog posts must be top-level)")
	}

	switch strings.ToLower(strings.TrimSpace(opts.contentType)) {
	case "plain", content.ContentTypePlain:
		opts.contentType = content.ContentTypePlain
	case "markdown", "md", content.ContentTypeMarkdown:
		opts.contentType = content.ContentTypeMarkdown
	default:
		return opts, fmt.Errorf("unsupported content type %q", opts.contentType)
	}
	if markdown {
		opts.contentType = content.ContentTypeMarkdown
	}
	if strings.TrimSpace(opts.language) == "" {
		return opts, fmt.Errorf("language cannot be empty")
	}
	return opts, nil
}

func parseVisibility(value string) (models.Visibility, error) {
	switch models.Visibility(strings.ToLower(strings.TrimSpace(value))) {
	case models.VisibilityPublic:
		return models.VisibilityPublic, nil
	case models.VisibilityUnlisted:
		return models.VisibilityUnlisted, nil
	case models.VisibilityPrivate:
		return models.VisibilityPrivate, nil
	case models.VisibilityDirect:
		return models.VisibilityDirect, nil
	case models.VisibilityLocal:
		return models.VisibilityLocal, nil
	case models.VisibilityLocalUnlisted:
		return models.VisibilityLocalUnlisted, nil
	default:
		return "", fmt.Errorf("invalid visibility %q", value)
	}
}

func readPostContent(path string, stdin io.Reader) (string, error) {
	var (
		data []byte
		err  error
	)
	if path == "" || path == "-" {
		data, err = io.ReadAll(stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return "", fmt.Errorf("read post content: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

func resolvePostAccount(ctx context.Context, store storage.Storage, cfg *config.Config, username string) (*models.Account, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		username = strings.TrimSpace(cfg.SingleUser)
	}
	if username != "" {
		account, err := store.GetAccountByUsernameDomain(ctx, username, nil)
		if err != nil || account.IsServiceActor {
			return nil, fmt.Errorf("local user %q not found", username)
		}
		return validatePostAccount(account)
	}

	accounts, err := store.GetLocalAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("list local users: %w", err)
	}
	switch len(accounts) {
	case 0:
		return nil, fmt.Errorf("no local users found; specify --username after creating one")
	case 1:
		return validatePostAccount(accounts[0])
	default:
		return nil, fmt.Errorf("multiple local users found; specify --username")
	}
}

func validatePostAccount(account *models.Account) (*models.Account, error) {
	if account.SuspendedAt != nil || account.ModerationState == "suspended" || account.ModerationState == "disabled" {
		return nil, fmt.Errorf("user %q is suspended or disabled", account.Username)
	}
	if !account.Approved {
		return nil, fmt.Errorf("user %q is not approved", account.Username)
	}
	return account, nil
}

func runPostCommand(args []string, cfg *config.Config) {
	opts, err := parsePostOptions(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printPostUsage(os.Stderr)
			return
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n\n", err)
		printPostUsage(os.Stderr)
		os.Exit(2)
	}
	postURL, err := createPostFromCLI(context.Background(), cfg, opts, os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error posting: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(postURL)
}

func createPostFromCLI(ctx context.Context, cfg *config.Config, opts postOptions, stdin io.Reader) (string, error) {
	source, err := readPostContent(opts.file, stdin)
	if err != nil {
		return "", err
	}
	if source == "" && len(opts.mediaIDs) == 0 {
		return "", fmt.Errorf("post is empty")
	}
	if opts.blog {
		if !blogBuildSupported() {
			return "", fmt.Errorf("blog posts are not compiled in; rebuild littleFedi with -tags blog")
		}
		if !cfg.Blog.Enabled {
			return "", fmt.Errorf("blog publishing is disabled; set [blog] enabled = true in config")
		}
	}

	store, err := storage.NewBunStorage(cfg.DB.DSN, cfg.DB.MaxOpenConns, cfg.DB.MaxIdleConns, cfg.DB.LogQueries)
	if err != nil {
		return "", fmt.Errorf("connect to database: %w", err)
	}
	defer store.Close()
	if err := store.RunMigrations(ctx); err != nil {
		return "", fmt.Errorf("run migrations: %w", err)
	}
	if _, err := resolveMeshIdentity(ctx, store, cfg); err != nil {
		return "", err
	}
	if err := validatePersistedWebDomain(ctx, store, cfg.WebDomain); err != nil {
		return "", err
	}
	account, err := resolvePostAccount(ctx, store, cfg, opts.username)
	if err != nil {
		return "", err
	}
	if opts.blog && !cfg.Blog.AccountAllowed(account.BlogAllowed) {
		return "", fmt.Errorf("user %q is not permitted to publish to the blog", account.Username)
	}
	if opts.inReplyToID != "" {
		if _, err := store.GetStatus(ctx, opts.inReplyToID); err != nil {
			return "", fmt.Errorf("reply target %q not found", opts.inReplyToID)
		}
	}
	if opts.quotedStatusID != "" {
		if _, err := store.GetStatus(ctx, opts.quotedStatusID); err != nil {
			return "", fmt.Errorf("quoted status %q not found", opts.quotedStatusID)
		}
	}
	for _, mediaID := range opts.mediaIDs {
		attachment, err := store.GetMediaAttachment(ctx, mediaID)
		if err != nil {
			return "", fmt.Errorf("media attachment %q not found", mediaID)
		}
		if attachment.AccountID != account.ID {
			return "", fmt.Errorf("media attachment %q does not belong to user %q", mediaID, account.Username)
		}
	}

	signer, err := federation.SetupInstanceActorWithDomains(ctx, store, cfg.LocalDomain, cfg.WebDomain)
	if err != nil {
		return "", fmt.Errorf("set up federation signer: %w", err)
	}
	federator := federation.NewFederatorWithDomains(store, cfg.LocalDomain, cfg.WebDomain, signer, cfg.Federation.UserAgent)
	service := app.NewWithDomains(store, federator, cfg.LocalDomain, cfg.WebDomain)
	service.SetImmediateDeliveries(true)
	service.SetBlogAllAccounts(cfg.Blog.AllAccounts)
	var replyTo *string
	if opts.inReplyToID != "" {
		replyTo = &opts.inReplyToID
	}
	status, err := service.CreateStatus(ctx, account, app.StatusInput{
		Content:        source,
		ContentType:    opts.contentType,
		Summary:        opts.summary,
		Sensitive:      opts.sensitive,
		Visibility:     opts.visibility,
		Language:       opts.language,
		InReplyToID:    replyTo,
		QuotedStatusID: opts.quotedStatusID,
		MediaIDs:       []string(opts.mediaIDs),
		IsBlogPost:     opts.blog,
		BlogSlug:       opts.blogSlug,
	})
	if err != nil {
		return "", err
	}

	// The CLI is a one-shot process: nothing is left running afterward to
	// drain the durable job queue, so the "blog_rebuild" job CreateStatus
	// just enqueued would sit untouched forever. Run the rebuild directly
	// here instead, in parallel with WaitForDeliveries so the two don't
	// serialize, joining both before the process exits.
	var blogWG sync.WaitGroup
	if status.IsBlogPost {
		blogWG.Add(1)
		go func() {
			defer blogWG.Done()
			if err := rebuildBlogAfterPost(ctx, store, cfg, account); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: blog rebuild failed: %v\n", err)
			}
		}()
	}
	service.WaitForDeliveries()
	blogWG.Wait()
	return status.URL, nil
}

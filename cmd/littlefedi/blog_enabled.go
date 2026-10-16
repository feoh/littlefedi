//go:build blog

package main

import (
	"context"
	"fmt"
	"os"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/blog"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/queue"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

func blogBuildSupported() bool { return true }

func wrapBlogStorage(store storage.Storage, cfg *config.Config) storage.Storage {
	return blog.NewRebuildStorage(store, cfg.Blog)
}

// rebuildBlogAfterPost regenerates the account's site directly, bypassing
// the durable job queue. The `post` CLI command is a one-shot process with
// no worker pool running to drain the "blog_rebuild" job CreateStatus
// enqueues, so without this the site would only update once something else
// (the daemon, or `admin blog rebuild`) happened to run later.
func rebuildBlogAfterPost(ctx context.Context, store storage.Storage, cfg *config.Config, account *models.Account) error {
	return blog.Generate(ctx, store, cfg, account)
}

func registerBlogRebuildHandler(pool *queue.WorkerPool, store storage.Storage, cfg *config.Config) {
	if !cfg.Blog.Enabled {
		return
	}
	pool.RegisterHandler(blog.RebuildJobKind, func(ctx context.Context, payload string) error {
		req := blog.ParseRebuildRequest(payload)
		key := req.Key()
		blog.Start(key)
		for {
			err := blog.Rebuild(ctx, store, cfg, payload)
			if err != nil {
				blog.Finish(key, false)
				return err
			}
			if !blog.Finish(key, true) {
				return blog.NotifyRebuilt(ctx, store, cfg.WebDomain, req)
			}
		}
	})
}

func blogCommandUsername(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "--username" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func handleBlogCommand(ctx context.Context, store storage.Storage, cfg *config.Config, args []string) bool {
	switch args[0] {
	case "rebuild":
		if !cfg.Blog.Enabled {
			fmt.Fprintln(os.Stderr, "Blog publishing is disabled; set [blog] enabled = true in config")
			os.Exit(1)
		}
		account, err := resolvePostAccount(ctx, store, cfg, blogCommandUsername(args))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		if err := blog.Generate(ctx, store, cfg, account); err != nil {
			fmt.Fprintf(os.Stderr, "Error rebuilding blog: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Blog rebuilt for %s\n", account.Username)
		return true
	case "rebuild-all":
		if !cfg.Blog.Enabled {
			fmt.Fprintln(os.Stderr, "Blog publishing is disabled; set [blog] enabled = true in config")
			os.Exit(1)
		}
		accounts, err := store.GetLocalAccounts(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error listing local accounts: %v\n", err)
			os.Exit(1)
		}
		rebuilt, failed := 0, 0
		for _, account := range accounts {
			if !cfg.Blog.AccountAllowed(account.BlogAllowed) {
				continue
			}
			if err := blog.Generate(ctx, store, cfg, account); err != nil {
				fmt.Fprintf(os.Stderr, "Error rebuilding blog for %s: %v\n", account.Username, err)
				failed++
				continue
			}
			fmt.Printf("Blog rebuilt for %s\n", account.Username)
			rebuilt++
		}
		fmt.Printf("Rebuilt %d blog(s), %d failed\n", rebuilt, failed)
		if failed > 0 {
			os.Exit(1)
		}
		return true
	case "allow", "disallow":
		if cfg.Blog.AllAccounts {
			fmt.Fprintln(os.Stderr, "Every account may already blog: [blog] all_accounts is on, and the per-account permission is ignored while it is")
			os.Exit(1)
		}
		account, err := resolvePostAccount(ctx, store, cfg, blogCommandUsername(args))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		allowed := args[0] == "allow"
		if err := store.SetBlogAllowed(ctx, account.ID, allowed); err != nil {
			fmt.Fprintf(os.Stderr, "Error updating blog permission: %v\n", err)
			os.Exit(1)
		}
		verb := "allowed"
		if !allowed {
			verb = "disallowed"
		}
		fmt.Printf("Blog %s for %s\n", verb, account.Username)
		return true
	default:
		return false
	}
}

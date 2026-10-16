//go:build !blog

package main

import (
	"context"
	"fmt"
	"os"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/queue"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

func blogBuildSupported() bool { return false }

func wrapBlogStorage(store storage.Storage, _ *config.Config) storage.Storage { return store }

// rebuildBlogAfterPost is unreachable in practice: post.go only calls it when
// status.IsBlogPost is true, which requires --blog, which createPostFromCLI
// already refuses unless blogBuildSupported(). Exists so post.go compiles
// either way.
func rebuildBlogAfterPost(context.Context, storage.Storage, *config.Config, *models.Account) error {
	return nil
}

func registerBlogRebuildHandler(*queue.WorkerPool, storage.Storage, *config.Config) {}

func handleBlogCommand(_ context.Context, _ storage.Storage, _ *config.Config, args []string) bool {
	if args[0] != "rebuild" && args[0] != "rebuild-all" {
		return false
	}
	fmt.Fprintln(os.Stderr, "Blog support is not compiled in; rebuild littleFedi with -tags blog")
	os.Exit(1)
	return true
}

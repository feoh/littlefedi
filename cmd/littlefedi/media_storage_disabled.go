//go:build !s3

package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/queue"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

func registerMediaMigrationHandler(*queue.WorkerPool, storage.Storage) {}
func activeMediaConfig(_ context.Context, _ storage.Storage, current config.Media) config.Media {
	return current
}

func handleMediaStorageCommand(_ context.Context, _ storage.Storage, _ *config.Config, args []string) bool {
	if !strings.HasPrefix(args[0], "storage-") {
		return false
	}
	fmt.Fprintln(os.Stderr, "S3 media migration support is not compiled in; rebuild littleFedi with -tags s3")
	return true
}

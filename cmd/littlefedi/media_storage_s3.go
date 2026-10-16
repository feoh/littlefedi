//go:build s3

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mediamigration"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/queue"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

func registerMediaMigrationHandler(pool *queue.WorkerPool, store storage.Storage) {
	pool.RegisterHandler(mediamigration.JobKind, func(ctx context.Context, payload string) error {
		return mediamigration.Process(ctx, store, payload)
	})
}
func activeMediaConfig(ctx context.Context, store storage.Storage, current config.Media) config.Media {
	active, err := store.GetActiveMediaStorageMigration(ctx)
	if err != nil || active == nil {
		return current
	}
	if err = json.Unmarshal([]byte(active.DestinationConfig), &current); err != nil {
		slog.Error("decode active media migration destination", "error", err)
		return current
	}
	slog.Info("media migration active; directing new uploads to destination", "migration_id", active.ID)
	return current
}

func handleMediaStorageCommand(ctx context.Context, store storage.Storage, cfg *config.Config, args []string) bool {
	switch args[0] {
	case "storage-migrate":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "Usage: littlefedi admin media storage-migrate DESTINATION_CONFIG")
			os.Exit(1)
		}
		dst, err := config.Load(args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "destination config: %v\n", err)
			os.Exit(1)
		}
		m, err := mediamigration.Start(ctx, store, cfg.Media, dst.Media)
		if err != nil {
			fmt.Fprintf(os.Stderr, "start migration: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Media migration queued: %s\n", m.ID)
	case "storage-status":
		m, err := store.GetActiveMediaStorageMigration(ctx)
		if err != nil {
			fmt.Println("No active media migration")
			return true
		}
		fmt.Printf("%s state=%s copied=%d failed=%d cursor=%s error=%s\n", m.ID, m.State, m.Copied, m.Failed, m.Cursor, m.LastError)
	case "storage-cancel":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "Usage: littlefedi admin media storage-cancel ID")
			os.Exit(1)
		}
		if err := mediamigration.Cancel(ctx, store, args[1]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "storage-resume":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "Usage: littlefedi admin media storage-resume ID")
			os.Exit(1)
		}
		if err := mediamigration.Resume(ctx, store, args[1]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "storage-manifest":
		if err := mediamigration.WriteManifest(ctx, store, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		return false
	}
	return true
}

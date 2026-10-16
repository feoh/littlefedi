// Package app provides follow import logic for account migration.
package app

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/federation"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/push"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
	"github.com/google/uuid"
)

const (
	// importRateLimit is the minimum time between follow creations to avoid
	// overwhelming both the local instance and remote servers.
	importRateLimit = 2 * time.Second
	// importTimeout is the maximum time allowed for a full follow import.
	importTimeout = 30 * time.Minute
)

// MigrationOptions controls account-migration import behavior.
type MigrationOptions struct {
	MaxFollows int
	ImportRate time.Duration
}

func (o MigrationOptions) withDefaults() MigrationOptions {
	if o.ImportRate <= 0 {
		o.ImportRate = importRateLimit
	}
	return o
}

// FollowImporter handles importing follows from an old account to a new
// local account during migration.
type FollowImporter struct {
	store     storage.Storage
	federator *federation.Federator
	domain    string
	options   MigrationOptions
}

// NewFollowImporterWithOptions creates a new FollowImporter with explicit
// migration limits.
func NewFollowImporterWithOptions(store storage.Storage, federator *federation.Federator, domain string, options MigrationOptions) *FollowImporter {
	return &FollowImporter{
		store:     store,
		federator: federator,
		domain:    domain,
		options:   options.withDefaults(),
	}
}

// ImportFollows fetches the old account's following collection and
// creates follows from the local account to each followed account.
// Returns the number of follows successfully imported.
func (fi *FollowImporter) ImportFollows(ctx context.Context, localAccount *models.Account, oldActorURI string) (int, error) {
	if localAccount.Domain != nil {
		return 0, fmt.Errorf("follow import only supported for local accounts")
	}
	if oldActorURI == "" {
		return 0, fmt.Errorf("old actor URI is required")
	}

	slog.Info("migration: starting follow import",
		"local_user", localAccount.Username,
		"old_actor", oldActorURI,
	)

	oldActor, err := fi.federator.FetchActor(ctx, oldActorURI)
	if err != nil {
		return 0, fmt.Errorf("fetch old actor: %w", err)
	}

	followingURI := oldActor.FollowingURI
	if followingURI == "" {
		return 0, fmt.Errorf("old actor has no following collection")
	}

	followedURIs, err := fi.fetchFollowingCollection(ctx, followingURI)
	if err != nil {
		return 0, fmt.Errorf("fetch following collection: %w", err)
	}

	slog.Info("migration: fetched following list",
		"local_user", localAccount.Username,
		"followed_count", len(followedURIs),
	)

	maxImport := fi.options.MaxFollows
	if maxImport > 0 && len(followedURIs) > maxImport {
		slog.Warn("migration: truncating follow import to max",
			"total", len(followedURIs),
			"max", maxImport,
		)
		followedURIs = followedURIs[:maxImport]
	}

	var imported int
	var mu sync.Mutex
	limiter := time.NewTicker(fi.options.ImportRate)
	defer limiter.Stop()

	for _, followedURI := range followedURIs {
		select {
		case <-ctx.Done():
			return imported, ctx.Err()
		case <-limiter.C:
		}

		targetAccount, err := fi.federator.ResolveAccount(ctx, followedURI)
		if err != nil {
			slog.Warn("migration: failed to resolve followed account",
				"uri", followedURI,
				"error", err,
			)
			continue
		}

		// Don't follow ourselves
		if targetAccount.ID == localAccount.ID {
			continue
		}
		if targetAccount.Domain != nil {
			allowed, _, policyErr := fi.store.FederationDomainAllowed(ctx, *targetAccount.Domain)
			if policyErr != nil || !allowed {
				continue
			}
		}

		// Check if already following
		existing, err := fi.store.GetFollowByPair(ctx, localAccount.ID, targetAccount.ID)
		if err == nil && existing != nil {
			slog.Debug("migration: already following, skipping",
				"target", targetAccount.Username,
			)
			continue
		}

		// Create the follow
		follow := &models.Follow{
			ID:              uuid.New().String(),
			AccountID:       localAccount.ID,
			TargetAccountID: targetAccount.ID,
			URI:             fmt.Sprintf("https://%s/follows/%s", fi.domain, uuid.New().String()),
			State:           models.FollowAccepted,
			CreatedAt:       time.Now(),
			UpdatedAt:       time.Now(),
		}

		if targetAccount.Domain != nil {
			follow.State = models.FollowPending
		}

		if err := fi.store.CreateFollow(ctx, follow); err != nil {
			slog.Warn("migration: failed to create follow",
				"target", targetAccount.Username,
				"error", err,
			)
			continue
		}

		// If remote, send Follow activity
		if targetAccount.Domain != nil && fi.federator != nil {
			go federation.SendFollowActivity(context.Background(), fi.federator, localAccount, targetAccount, follow.URI)
		}

		mu.Lock()
		imported++
		mu.Unlock()

		slog.Debug("migration: imported follow",
			"target", targetAccount.Username,
			"imported_so_far", imported,
		)
	}

	slog.Info("migration: follow import completed",
		"local_user", localAccount.Username,
		"imported", imported,
		"total_available", len(followedURIs),
	)

	return imported, nil
}

// fetchFollowingCollection fetches the orderedItems from a following collection.
// Handles both OrderedCollection (with a first page) and OrderedCollectionPage.
func (fi *FollowImporter) fetchFollowingCollection(ctx context.Context, followingURI string) ([]string, error) {
	// Try to use the federator's FetchOutboxFirst (it works on any OrderedCollection)
	uris, err := fi.federator.FetchCollectionURIs(ctx, followingURI, fi.options.MaxFollows)
	if err != nil {
		return nil, fmt.Errorf("fetch following collection: %w", err)
	}
	return uris, nil
}

var ErrFollowImportLimit = errors.New("follows CSV exceeds the configured import limit")

// ParseFollowCSV reads Mastodon-compatible following CSV, including quoted
// fields, CRLF files, a UTF-8 BOM, optional leading @ signs, and duplicates.
func ParseFollowCSV(r io.Reader, limit int) ([]string, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	seen := make(map[string]bool)
	var handles []string
	firstRow := true
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse follows CSV: %w", err)
		}
		if len(record) == 0 {
			continue
		}
		handle := strings.TrimSpace(strings.TrimPrefix(record[0], "\ufeff"))
		if firstRow {
			firstRow = false
			if strings.EqualFold(handle, "Account address") {
				continue
			}
		}
		handle = strings.TrimPrefix(handle, "@")
		if handle == "" || seen[strings.ToLower(handle)] {
			continue
		}
		seen[strings.ToLower(handle)] = true
		if limit > 0 && len(handles) >= limit {
			return nil, fmt.Errorf("%w (%d)", ErrFollowImportLimit, limit)
		}
		handles = append(handles, handle)
	}
	return handles, nil
}

// ImportFollowHandles resolves each handle and creates a Follow from account
// to it, rate-limited and skipping self/duplicates/unresolvable handles. It
// is the shared body behind both CSV follow import and full-archive import.
func ImportFollowHandles(ctx context.Context, store storage.Storage, federator *federation.Federator, domain string, account *models.Account, handles []string, rate time.Duration) (imported, skipped int) {
	if rate <= 0 {
		rate = importRateLimit
	}
	limiter := time.NewTicker(rate)
	defer limiter.Stop()

	for _, handle := range handles {
		<-limiter.C

		handle = strings.TrimSpace(handle)
		if handle == "" {
			skipped++
			continue
		}

		var targetAccount *models.Account
		var resolveErr error
		if federator != nil {
			targetAccount, resolveErr = federator.ResolveAccount(ctx, handle)
		} else if !strings.Contains(handle, "@") {
			targetAccount, resolveErr = store.GetAccountByUsernameDomain(ctx, handle, nil)
		} else {
			skipped++
			continue
		}
		if resolveErr != nil {
			slog.Warn("import: follow resolve failed", "handle", handle, "error", resolveErr)
			skipped++
			continue
		}
		if targetAccount.ID == account.ID {
			skipped++
			continue
		}
		if targetAccount.Domain != nil {
			allowed, _, policyErr := store.FederationDomainAllowed(ctx, *targetAccount.Domain)
			if policyErr != nil || !allowed {
				skipped++
				continue
			}
		}

		if existing, _ := store.GetFollowByPair(ctx, account.ID, targetAccount.ID); existing != nil {
			skipped++
			continue
		}

		follow := &models.Follow{
			ID:              uuid.New().String(),
			AccountID:       account.ID,
			TargetAccountID: targetAccount.ID,
			URI:             fmt.Sprintf("https://%s/follows/%s", domain, uuid.New().String()),
			State:           models.FollowAccepted,
			CreatedAt:       time.Now(),
			UpdatedAt:       time.Now(),
		}
		if targetAccount.Domain != nil {
			follow.State = models.FollowPending
		}
		if err := store.CreateFollow(ctx, follow); err != nil {
			slog.Warn("import: follow create failed", "handle", handle, "error", err)
			skipped++
			continue
		}
		if targetAccount.Domain != nil && federator != nil {
			go federation.SendFollowActivity(context.Background(), federator, account, targetAccount, follow.URI)
		}
		imported++
	}
	return imported, skipped
}

// MigrateLocalFollowers moves this instance's own users from the old account
// to its destination. Remote follower servers receive the Move over AP, but
// local followers have no inbox delivery and must be handled directly.
func MigrateLocalFollowers(ctx context.Context, store storage.Storage, federator *federation.Federator, oldAccount, newAccount *models.Account) (int, error) {
	if newAccount.Domain != nil {
		allowed, _, err := store.FederationDomainAllowed(ctx, *newAccount.Domain)
		if err != nil || !allowed {
			return 0, err
		}
	}
	followers, err := store.GetFollowersToMigrate(ctx, oldAccount.ID, 0)
	if err != nil {
		return 0, err
	}
	migrated := 0
	for _, follower := range followers {
		if follower.Domain != nil {
			continue
		}
		existingFollow, _ := store.GetFollowByPair(ctx, follower.ID, newAccount.ID)
		if err := store.MigrateFollower(ctx, follower.ID, oldAccount.ID, newAccount.ID); err != nil {
			slog.Warn("migration: failed to move local follower", "follower_id", follower.ID, "error", err)
			continue
		}
		if existingFollow != nil {
			// The old relationship was still removed, but no new activity is needed.
		} else if newAccount.Domain != nil && federator != nil {
			if follow, err := store.GetFollowByPair(ctx, follower.ID, newAccount.ID); err == nil {
				if err := federation.SendFollowActivity(ctx, federator, follower, newAccount, follow.URI); err != nil {
					slog.Warn("migration: failed to send migrated Follow", "follower_id", follower.ID, "error", err)
				}
			}
		}
		notif := &models.Notification{Type: "move", AccountID: follower.ID, FromAccountID: oldAccount.ID}
		if err := store.UpsertNotification(ctx, notif); err != nil {
			slog.Warn("migration: failed to notify local follower", "follower_id", follower.ID, "error", err)
		} else {
			push.EnqueuePushNotification(ctx, store, notif)
		}
		migrated++
	}
	return migrated, nil
}

// Package mediacache coordinates background caching of remote media. It bridges
// the job queue, the storage layer, and the media.Cache engine: jobs are
// enqueued (eagerly via a background sweep, or lazily when media is first
// proxied) and processed here by fetching the file, storing it locally, and
// repointing the matching media_attachments rows at the local copy.
package mediacache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// JobKind is the queue job kind handled by Process.
const JobKind = "media_cache"

// FailureRetryInterval is how long a non-cacheable remote URL is skipped before
// it becomes eligible for another attempt (in case the remote recovers). It
// bounds how stale the negative cache can be.
const FailureRetryInterval = 24 * time.Hour

// Payload is the job payload: the remote media URL to cache.
type Payload struct {
	RemoteURL string `json:"remote_url"`
}

// EnqueueStore is the subset of storage needed to enqueue a cache job.
type EnqueueStore interface {
	EnqueueJob(ctx context.Context, job *models.Job) error
	// MediaCacheFailedSince reports whether the URL was recently found non-cacheable.
	MediaCacheFailedSince(ctx context.Context, url string, cutoff time.Time) (bool, error)
	// IsInstanceMediaCacheable reports whether the host is currently worth caching
	// from. A host with an open failure streak is skipped so a dead instance's
	// media does not refill the queue every sweep.
	IsInstanceMediaCacheable(ctx context.Context, domain string) (bool, error)
}

// Store is the subset of storage needed to process a cache job.
type Store interface {
	GetMediaAttachmentsByRemoteURL(ctx context.Context, remoteURL string) ([]*models.MediaAttachment, error)
	UpdateMediaAttachment(ctx context.Context, media *models.MediaAttachment) error
	GetAccountsByImageURL(ctx context.Context, url string) ([]*models.Account, error)
	UpdateAccount(ctx context.Context, account *models.Account) error
	GetEmojisByRemoteURL(ctx context.Context, remoteURL string) ([]*models.CustomEmoji, error)
	UpdateEmoji(ctx context.Context, emoji *models.CustomEmoji) error
	// RecordMediaCacheFailure marks a remote URL as non-cacheable (negative cache).
	RecordMediaCacheFailure(ctx context.Context, url, reason string) error
	// MarkInstanceUnreachable opens (or extends) the host's failure streak when a
	// media fetch fails transiently (network error / 5xx), so further media jobs
	// for it are paused instead of each retrying. The streak is cleared by a
	// successful delivery/fetch or inbound activity elsewhere, which resumes
	// caching - media successes themselves never run while a host is paused.
	MarkInstanceUnreachable(ctx context.Context, domain string) (bool, error)
}

// Cacher fetches and stores remote media. *media.Cache implements it.
type Cacher interface {
	Store(ctx context.Context, remoteURL string) (*media.StoreResult, error)
}

// Enqueue schedules a background job to cache the given remote media URL. URLs
// recently found non-cacheable are skipped so a permanently-dead remote (e.g. a
// 404 avatar still served via the live proxy) is not re-attempted every sweep.
// It reports whether a job was actually enqueued (false when the URL is empty or
// skipped via the negative cache) so callers can count real work accurately.
func Enqueue(ctx context.Context, store EnqueueStore, remoteURL string) (bool, error) {
	if remoteURL == "" {
		return false, nil
	}
	failed, err := store.MediaCacheFailedSince(ctx, remoteURL, time.Now().Add(-FailureRetryInterval))
	if err != nil {
		return false, fmt.Errorf("check media cache failure: %w", err)
	}
	if failed {
		return false, nil
	}
	// Skip hosts with an open failure streak: their media is still served live via
	// the proxy, and re-enqueueing it every sweep is what lets one dead instance
	// flood the queue.
	if host := urlHost(remoteURL); host != "" {
		if cacheable, err := store.IsInstanceMediaCacheable(ctx, host); err == nil && !cacheable {
			return false, nil
		}
	}
	payload, err := json.Marshal(Payload{RemoteURL: remoteURL})
	if err != nil {
		return false, fmt.Errorf("marshal media_cache payload: %w", err)
	}
	if err := store.EnqueueJob(ctx, &models.Job{
		Kind:    JobKind,
		Payload: string(payload),
		RunAt:   time.Now(),
	}); err != nil {
		return false, err
	}
	return true, nil
}

// Process handles a media_cache job: it fetches and stores the remote media,
// then repoints every media attachment referencing that remote URL at the local
// cache. It is idempotent - already-cached rows are left untouched.
func Process(ctx context.Context, store Store, cache Cacher, payload string) error {
	var p Payload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return fmt.Errorf("unmarshal media_cache payload: %w", err)
	}
	if p.RemoteURL == "" {
		return nil
	}

	rows, err := store.GetMediaAttachmentsByRemoteURL(ctx, p.RemoteURL)
	if err != nil {
		return fmt.Errorf("lookup media by remote URL: %w", err)
	}
	// Status attachments still pointing at the remote origin (not yet cached).
	var pendingMedia []*models.MediaAttachment
	for _, m := range rows {
		if m.URL == p.RemoteURL {
			pendingMedia = append(pendingMedia, m)
		}
	}

	// Accounts whose avatar/header is this URL and not yet cached.
	accounts, err := store.GetAccountsByImageURL(ctx, p.RemoteURL)
	if err != nil {
		return fmt.Errorf("lookup accounts by image URL: %w", err)
	}
	var pendingAccounts []*models.Account
	for _, a := range accounts {
		if a.Avatar == p.RemoteURL || a.Header == p.RemoteURL {
			pendingAccounts = append(pendingAccounts, a)
		}
	}

	// Custom emojis whose image is this URL and not yet cached.
	emojis, err := store.GetEmojisByRemoteURL(ctx, p.RemoteURL)
	if err != nil {
		return fmt.Errorf("lookup emojis by remote URL: %w", err)
	}
	var pendingEmojis []*models.CustomEmoji
	for _, e := range emojis {
		if e.URL == p.RemoteURL || e.StaticURL == p.RemoteURL {
			pendingEmojis = append(pendingEmojis, e)
		}
	}

	if len(pendingMedia) == 0 && len(pendingAccounts) == 0 && len(pendingEmojis) == 0 {
		// Nothing references this URL, or everything is already cached.
		return nil
	}

	res, err := cache.Store(ctx, p.RemoteURL)
	if err != nil {
		if errors.Is(err, media.ErrNotCacheable) {
			// Permanent (until the retry window elapses): keep the media at its
			// remote URL so it is still proxied and shown, but record a negative
			// cache entry so the eager sweep and lazy proxy hook stop re-enqueueing
			// it. This is the only marker for accounts, whose avatar/header has no
			// per-row CachedAt field. Also mark the attachment/emoji rows so those
			// sweeps skip them before they even reach Enqueue. Don't fail the job -
			// there's no point retrying now.
			if rerr := store.RecordMediaCacheFailure(ctx, p.RemoteURL, err.Error()); rerr != nil {
				return fmt.Errorf("record non-cacheable media %s: %w", p.RemoteURL, rerr)
			}
			now := time.Now()
			for _, m := range pendingMedia {
				m.CachedAt = &now
				if uerr := store.UpdateMediaAttachment(ctx, m); uerr != nil {
					return fmt.Errorf("mark non-cacheable media %s: %w", m.ID, uerr)
				}
			}
			for _, e := range pendingEmojis {
				e.CachedAt = &now
				if uerr := store.UpdateEmoji(ctx, e); uerr != nil {
					return fmt.Errorf("mark non-cacheable emoji %s: %w", e.ID, uerr)
				}
			}
			slog.Info("mediacache: media not cacheable, leaving it proxied",
				"remote_url", p.RemoteURL, "reason", err)
			return nil
		}
		// Transient failure (network error / 5xx): open the host's failure streak so
		// its other media jobs are paused instead of each retrying. Still return the
		// error so this individual job backs off and retries.
		if host := urlHost(p.RemoteURL); host != "" {
			if _, merr := store.MarkInstanceUnreachable(ctx, host); merr != nil {
				slog.Warn("mediacache: failed to mark host unreachable", "host", host, "error", merr)
			}
		}
		return fmt.Errorf("store remote media: %w", err)
	}

	now := time.Now()
	for _, m := range pendingMedia {
		m.URL = res.URL
		if m.PreviewURL == "" || m.PreviewURL == m.RemoteURL {
			m.PreviewURL = res.PreviewURL
			m.PreviewStorageKey = res.PreviewStorageKey
		}
		if m.Blurhash == "" {
			m.Blurhash = res.Blurhash
		}
		if res.Meta != "" {
			m.Meta = res.Meta
		}
		if m.MIMEType == "" {
			m.MIMEType = res.MIMEType
		}
		if m.FileSize == 0 {
			m.FileSize = res.FileSize
		}
		m.CachedAt = &now
		m.StorageBackend = res.StorageBackend
		m.StorageKey = res.StorageKey
		if err := store.UpdateMediaAttachment(ctx, m); err != nil {
			return fmt.Errorf("update cached media %s: %w", m.ID, err)
		}
	}

	for _, a := range pendingAccounts {
		// Repoint the served URL at the local cache; remember the origin so the
		// federator can preserve the cache across actor refreshes and so the
		// cache GC can map files back to rows.
		if a.Avatar == p.RemoteURL {
			a.Avatar = res.URL
			a.AvatarRemote = p.RemoteURL
		}
		if a.Header == p.RemoteURL {
			a.Header = res.URL
			a.HeaderRemote = p.RemoteURL
		}
		if err := store.UpdateAccount(ctx, a); err != nil {
			return fmt.Errorf("update cached account image %s: %w", a.ID, err)
		}
	}

	for _, e := range pendingEmojis {
		// Repoint the served URLs at the local cache; remote_url already holds the
		// origin so the cache GC can map the file back to this row.
		if e.URL == p.RemoteURL {
			e.URL = res.URL
		}
		if e.StaticURL == p.RemoteURL || e.StaticURL == "" {
			e.StaticURL = res.URL
		}
		e.CachedAt = &now
		if err := store.UpdateEmoji(ctx, e); err != nil {
			return fmt.Errorf("update cached emoji %s: %w", e.ID, err)
		}
	}

	slog.Debug("mediacache: cached remote media",
		"remote_url", p.RemoteURL, "media_rows", len(pendingMedia),
		"account_rows", len(pendingAccounts), "emoji_rows", len(pendingEmojis))
	return nil
}

// urlHost returns the host of a remote media URL, or "" if it can't be parsed.
func urlHost(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return ""
}

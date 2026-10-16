//go:build s3

// Package mediamigration implements resumable native object-store migrations.
package mediamigration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media/objectstore"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"github.com/google/uuid"
)

const JobKind = "media_storage_migrate"

type Store interface {
	ListOwnedMediaAfter(context.Context, string, string, int) ([]*models.MediaAttachment, error)
	CreateMediaStorageMigration(context.Context, *models.MediaStorageMigration) error
	GetMediaStorageMigration(context.Context, string) (*models.MediaStorageMigration, error)
	GetActiveMediaStorageMigration(context.Context) (*models.MediaStorageMigration, error)
	UpdateMediaStorageMigration(context.Context, *models.MediaStorageMigration) error
	UpdateMediaAttachment(context.Context, *models.MediaAttachment) error
	InvalidateRemoteMediaCache(context.Context) (int64, error)
	UpdateLocalMediaReferences(context.Context, string, string) error
	GetAllMedia(context.Context) ([]*models.MediaAttachment, error)
	EnqueueJob(context.Context, *models.Job) error
}
type Payload struct {
	ID string `json:"id"`
}

func WriteManifest(ctx context.Context, db Store, w io.Writer) error {
	rows, err := db.GetAllMedia(ctx)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	for _, m := range rows {
		if m.RemoteURL != "" {
			continue
		}
		key := m.StorageKey
		if key == "" {
			key = keyFromURL(m.URL, m.ID)
		}
		if err = enc.Encode(map[string]any{"key": key, "size": m.FileSize, "content_type": m.MIMEType}); err != nil {
			return err
		}
		if m.PreviewURL != "" && m.PreviewURL != m.URL {
			pk := m.PreviewStorageKey
			if pk == "" {
				pk = keyFromURL(m.PreviewURL, m.ID)
			}
			if err = enc.Encode(map[string]any{"key": pk, "content_type": m.MIMEType}); err != nil {
				return err
			}
		}
	}
	return nil
}

func Start(ctx context.Context, db Store, source, destination config.Media) (*models.MediaStorageMigration, error) {
	if active, err := db.GetActiveMediaStorageMigration(ctx); err == nil && active != nil {
		return nil, fmt.Errorf("media migration %s is already active", active.ID)
	}
	src, err := objectstore.New(source, false)
	if err != nil {
		return nil, err
	}
	if err = src.Check(ctx); err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	dst, err := objectstore.New(destination, false)
	if err != nil {
		return nil, err
	}
	if err = dst.Check(ctx); err != nil {
		return nil, fmt.Errorf("destination: %w", err)
	}
	sb, _ := json.Marshal(source)
	dbb, _ := json.Marshal(destination)
	now := time.Now().UTC()
	m := &models.MediaStorageMigration{ID: uuid.NewString(), CreatedAt: now, UpdatedAt: now, SourceConfig: string(sb), DestinationConfig: string(dbb), State: "queued"}
	if err = db.CreateMediaStorageMigration(ctx, m); err != nil {
		return nil, err
	}
	return m, enqueue(ctx, db, m.ID)
}
func enqueue(ctx context.Context, db Store, id string) error {
	b, _ := json.Marshal(Payload{ID: id})
	return db.EnqueueJob(ctx, &models.Job{ID: uuid.NewString(), Kind: JobKind, Payload: string(b), State: "queued", MaxAttempts: 10, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
}
func Cancel(ctx context.Context, db Store, id string) error {
	m, e := db.GetMediaStorageMigration(ctx, id)
	if e != nil {
		return e
	}
	m.CancelRequested = true
	return db.UpdateMediaStorageMigration(ctx, m)
}
func Resume(ctx context.Context, db Store, id string) error {
	m, e := db.GetMediaStorageMigration(ctx, id)
	if e != nil {
		return e
	}
	m.CancelRequested = false
	m.State = "queued"
	m.LastError = ""
	if e = db.UpdateMediaStorageMigration(ctx, m); e != nil {
		return e
	}
	return enqueue(ctx, db, id)
}

func Process(ctx context.Context, db Store, payload string) error {
	var p Payload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return err
	}
	m, err := db.GetMediaStorageMigration(ctx, p.ID)
	if err != nil {
		return err
	}
	if m.CancelRequested {
		m.State = "cancelled"
		return db.UpdateMediaStorageMigration(ctx, m)
	}
	var sc, dc config.Media
	if err = json.Unmarshal([]byte(m.SourceConfig), &sc); err != nil {
		return err
	}
	if err = json.Unmarshal([]byte(m.DestinationConfig), &dc); err != nil {
		return err
	}
	src, err := objectstore.New(sc, false)
	if err != nil {
		return err
	}
	dst, err := objectstore.New(dc, false)
	if err != nil {
		return err
	}
	m.State = "running"
	_ = db.UpdateMediaStorageMigration(ctx, m)
	rows, err := db.ListOwnedMediaAfter(ctx, m.Cursor, dst.ID(), 50)
	if err != nil {
		return err
	}
	for _, item := range rows {
		oldURL := item.URL
		if item.StorageKey == "" {
			item.StorageKey = keyFromURL(item.URL, item.ID)
		}
		if err = copyOne(ctx, src, dst, item.StorageKey, item.MIMEType); err != nil {
			m.Failed++
			m.LastError = err.Error()
			m.State = "paused"
			_ = db.UpdateMediaStorageMigration(ctx, m)
			return err
		}
		if item.PreviewURL != "" && item.PreviewURL != item.URL {
			if item.PreviewStorageKey == "" {
				item.PreviewStorageKey = keyFromURL(item.PreviewURL, item.ID)
			}
			if err = copyOne(ctx, src, dst, item.PreviewStorageKey, item.MIMEType); err != nil {
				return err
			}
		}
		item.StorageBackend = dst.ID()
		item.URL = dst.URL(item.StorageKey)
		if item.PreviewStorageKey != "" {
			item.PreviewURL = dst.URL(item.PreviewStorageKey)
		} else if item.Type != "video" {
			item.PreviewURL = item.URL
		} else {
			item.PreviewURL = ""
		}
		if err = db.UpdateMediaAttachment(ctx, item); err != nil {
			return err
		}
		if err = db.UpdateLocalMediaReferences(ctx, oldURL, item.URL); err != nil {
			return err
		}
		m.Cursor = item.ID
		m.Copied++
	}
	if len(rows) > 0 {
		_ = db.UpdateMediaStorageMigration(ctx, m)
		if len(rows) < 50 {
			m.Cursor = ""
			_ = db.UpdateMediaStorageMigration(ctx, m)
		}
		return enqueue(ctx, db, m.ID)
	}
	_, err = db.InvalidateRemoteMediaCache(ctx)
	if err != nil {
		return err
	}
	m.State = "completed"
	return db.UpdateMediaStorageMigration(ctx, m)
}
func copyOne(ctx context.Context, src, dst objectstore.Store, key, mime string) error {
	if key == "" {
		return fmt.Errorf("empty media key")
	}
	if d, e := dst.Stat(ctx, key); e == nil {
		if s, se := src.Stat(ctx, key); se == nil && s.Size == d.Size {
			return nil
		}
	}
	_, e := objectstore.Copy(ctx, src, dst, key, objectstore.PutOptions{ContentType: mime, CacheControl: "public, max-age=31536000, immutable"})
	return e
}
func keyFromURL(u, id string) string {
	if i := strings.Index(u, "/media/"); i >= 0 {
		return "media/" + strings.TrimPrefix(u[i+len("/media/"):], "/")
	}
	return path.Join("media", id, path.Base(u))
}

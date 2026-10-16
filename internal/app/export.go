package app

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media/objectstore"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

const AccountExportLifetime = 24 * time.Hour

var ErrAccountExportRateLimited = errors.New("account export is available once per hour")

type accountExportPayload struct {
	ExportID  string `json:"export_id"`
	AccountID string `json:"account_id"`
}

func RequestAccountExport(ctx context.Context, store storage.Storage, account *models.Account) (*models.AccountExport, bool, error) {
	if account == nil || account.Domain != nil || account.IsServiceActor || account.DeletionState != "" {
		return nil, false, fmt.Errorf("account export is unavailable")
	}
	if latest, err := store.GetLatestAccountExport(ctx, account.ID); err == nil {
		if latest.State == "queued" || (latest.State == "ready" && latest.DownloadedAt == nil && latest.ExpiresAt != nil && latest.ExpiresAt.After(time.Now())) {
			return latest, false, nil
		}
		if latest.CreatedAt.After(time.Now().Add(-time.Hour)) {
			return latest, false, ErrAccountExportRateLimited
		}
	}
	export := &models.AccountExport{AccountID: account.ID, State: "queued"}
	if err := store.CreateAccountExport(ctx, export); err != nil {
		return nil, false, err
	}
	payload, _ := json.Marshal(accountExportPayload{ExportID: export.ID, AccountID: account.ID})
	if err := store.EnqueueJob(ctx, &models.Job{Kind: models.AccountExportJobKind, Payload: string(payload), MaxAttempts: 3, State: "queued"}); err != nil {
		_ = store.DeleteAccountExport(ctx, export.ID, account.ID)
		return nil, false, err
	}
	return export, true, nil
}

func HandleAccountExportJob(store storage.Storage, mediaStore objectstore.Store) func(context.Context, string) error {
	return func(ctx context.Context, body string) error {
		var payload accountExportPayload
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			return fmt.Errorf("decode account export: %w", err)
		}
		export, err := store.GetAccountExport(ctx, payload.ExportID, payload.AccountID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if export.State == "ready" {
			return enqueueAccountExportCleanup(ctx, store, export)
		}
		if export.State == "expired" {
			return nil
		}
		if mediaStore == nil {
			return fmt.Errorf("account export media store is unavailable")
		}
		if err := buildAccountExport(ctx, store, mediaStore, export); err != nil {
			_ = store.RecordAccountExportError(ctx, export.ID, err.Error())
			return err
		}
		return nil
	}
}

func buildAccountExport(ctx context.Context, store storage.Storage, mediaStore objectstore.Store, export *models.AccountExport) error {
	snapshot, err := store.BuildAccountExportSnapshot(ctx, export.AccountID)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "littlefedi-export-*.zip")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	zw := zip.NewWriter(tmp)
	if err := writeExportJSON(zw, "manifest.json", map[string]any{
		"format": "littlefedi-account-export", "version": 1, "generated_at": time.Now().UTC(),
		"account_file": "account.json", "media_index": "media/index.json",
	}); err != nil {
		_ = zw.Close()
		_ = tmp.Close()
		return err
	}
	if err := writeExportJSON(zw, "account.json", snapshot); err != nil {
		_ = zw.Close()
		_ = tmp.Close()
		return err
	}
	if err := writeExportJSON(zw, "media/index.json", snapshot.Media); err != nil {
		_ = zw.Close()
		_ = tmp.Close()
		return err
	}
	seen := make(map[string]bool, len(snapshot.Media))
	for _, media := range snapshot.Media {
		if media == nil || media.StorageKey == "" || seen[media.StorageKey] {
			continue
		}
		seen[media.StorageKey] = true
		reader, object, err := mediaStore.Open(ctx, media.StorageKey)
		if err != nil {
			_ = zw.Close()
			_ = tmp.Close()
			return fmt.Errorf("open owned media %s: %w", media.ID, err)
		}
		name := path.Base(strings.ReplaceAll(media.StorageKey, "\\", "/"))
		if name == "." || name == "/" || name == "" {
			name = "media"
		}
		header := &zip.FileHeader{Name: "media/" + media.ID + "/" + name, Method: zip.Deflate}
		header.SetMode(0o600)
		header.SetModTime(object.ModTime)
		writer, createErr := zw.CreateHeader(header)
		if createErr == nil {
			_, createErr = io.Copy(writer, reader)
		}
		closeErr := reader.Close()
		if createErr != nil {
			_ = zw.Close()
			_ = tmp.Close()
			return createErr
		}
		if closeErr != nil {
			_ = zw.Close()
			_ = tmp.Close()
			return closeErr
		}
	}
	if err := zw.Close(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	info, err := os.Stat(tmpName)
	if err != nil {
		return err
	}
	reader, err := os.Open(tmpName)
	if err != nil {
		return err
	}
	defer reader.Close()
	key := "exports/" + export.AccountID + "/" + export.ID + ".zip"
	if _, err := mediaStore.Put(ctx, key, reader, info.Size(), objectstore.PutOptions{ContentType: "application/zip", CacheControl: "private, no-store"}); err != nil {
		return err
	}
	expiresAt := time.Now().UTC().Add(AccountExportLifetime)
	if err := store.CompleteAccountExport(ctx, export.ID, key, info.Size(), expiresAt); err != nil {
		_ = mediaStore.Delete(ctx, key)
		return err
	}
	export.ExpiresAt = &expiresAt
	return enqueueAccountExportCleanup(ctx, store, export)
}

func enqueueAccountExportCleanup(ctx context.Context, store storage.Storage, export *models.AccountExport) error {
	if export.ExpiresAt == nil {
		return fmt.Errorf("account export expiry is missing")
	}
	payload, _ := json.Marshal(accountExportPayload{ExportID: export.ID, AccountID: export.AccountID})
	return store.EnqueueJob(ctx, &models.Job{Kind: models.AccountExportCleanupJobKind, Payload: string(payload), RunAt: *export.ExpiresAt, MaxAttempts: 10, State: "queued"})
}

func writeExportJSON(zw *zip.Writer, name string, value any) error {
	header := &zip.FileHeader{Name: name, Method: zip.Deflate}
	header.SetMode(0o600)
	w, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func HandleAccountExportCleanupJob(store storage.Storage, mediaStore objectstore.Store) func(context.Context, string) error {
	return func(ctx context.Context, body string) error {
		var payload accountExportPayload
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			return err
		}
		export, err := store.GetAccountExport(ctx, payload.ExportID, payload.AccountID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if export.State == "expired" {
			return nil
		}
		if export.ExpiresAt != nil && time.Now().Before(*export.ExpiresAt) {
			return fmt.Errorf("account export cleanup ran %s early", time.Until(*export.ExpiresAt).String())
		}
		if export.StorageKey != "" {
			if mediaStore == nil {
				return fmt.Errorf("account export media store is unavailable")
			}
			if err := mediaStore.Delete(ctx, export.StorageKey); err != nil {
				return err
			}
		}
		return store.ExpireAccountExport(ctx, export.ID)
	}
}

package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

const (
	manifestFile   = "manifest.json"
	configFileName = "config.toml"
)

func databaseFileName(dialect string) string {
	switch dialect {
	case "sqlite":
		return "database.db"
	case "pg":
		return "database.sql"
	default:
		return "database.dump"
	}
}

type manifest struct {
	Version        int       `json:"version"`
	Timestamp      time.Time `json:"timestamp"`
	DatabaseDriver string    `json:"database_driver"`
	MediaIncluded  bool      `json:"media_included"`
	MediaCount     int       `json:"media_count"`
	ConfigFile     string    `json:"config_file"`
	DatabaseFile   string    `json:"database_file"`
}

func CreateBackup(ctx context.Context, store storage.Storage, cfg *config.Config) (string, error) {
	if cfg.Backup.IncludeMedia && cfg.Media.OwnedBackend() != "local" {
		return "", fmt.Errorf("backup: owned media uses %s; use the storage provider's backup", cfg.Media.OwnedBackend())
	}

	root := cfg.Backup.ResolvedPath()
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", fmt.Errorf("backup: creating root %s: %w", root, err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		return "", fmt.Errorf("backup: securing root %s: %w", root, err)
	}
	backupDir := filepath.Join(root, time.Now().UTC().Format("2006-01-02T150405Z"))
	if err := os.Mkdir(backupDir, 0700); err != nil {
		return "", fmt.Errorf("backup: creating directory %s: %w", backupDir, err)
	}

	slog.Info("backup: starting", "path", backupDir, "include_media", cfg.Backup.IncludeMedia)

	dbFileName := databaseFileName(detectDialect(cfg.DB.DSN))
	man := manifest{
		Version:        1,
		Timestamp:      time.Now().UTC(),
		DatabaseDriver: detectDialect(cfg.DB.DSN),
		MediaIncluded:  cfg.Backup.IncludeMedia,
		ConfigFile:     configFileName,
		DatabaseFile:   dbFileName,
	}

	if err := dumpConfig(backupDir, cfg); err != nil {
		cleanupBackup(backupDir)
		return "", fmt.Errorf("backup: config: %w", err)
	}

	if err := dumpDatabase(ctx, backupDir, dbFileName, store); err != nil {
		cleanupBackup(backupDir)
		return "", fmt.Errorf("backup: database: %w", err)
	}

	if cfg.Backup.IncludeMedia {
		count, err := copyOwnedMedia(ctx, backupDir, cfg.Media.Path, store)
		if err != nil {
			cleanupBackup(backupDir)
			return "", fmt.Errorf("backup: media: %w", err)
		}
		man.MediaCount = count
	}

	if err := writeManifest(backupDir, man); err != nil {
		cleanupBackup(backupDir)
		return "", fmt.Errorf("backup: manifest: %w", err)
	}

	pruneOldBackups(cfg.Backup)

	slog.Info("backup: completed", "path", backupDir, "media_count", man.MediaCount)
	return backupDir, nil
}

func RestoreBackup(ctx context.Context, path string, store storage.Storage, cfg *config.Config) error {
	rootInfo, err := os.Lstat(path)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return fmt.Errorf("restore: backup path must be a real directory")
	}
	man, err := readManifest(path)
	if err != nil {
		return fmt.Errorf("restore: reading manifest: %w", err)
	}
	if man.MediaIncluded && cfg.Media.OwnedBackend() != "local" {
		return fmt.Errorf("restore: media restore requires local owned media, configured backend is %s", cfg.Media.OwnedBackend())
	}

	slog.Info("restore: starting", "path", path, "driver", man.DatabaseDriver, "media_included", man.MediaIncluded)

	if filepath.Base(man.DatabaseFile) != man.DatabaseFile || man.DatabaseFile != databaseFileName(man.DatabaseDriver) {
		return fmt.Errorf("restore: invalid database file in manifest")
	}
	dbPath := filepath.Join(path, man.DatabaseFile)
	f, err := openRegularFile(dbPath)
	if err != nil {
		return fmt.Errorf("restore: opening database dump %s: %w", dbPath, err)
	}
	defer f.Close()

	if err := store.ImportDatabase(ctx, f, man.DatabaseDriver); err != nil {
		return fmt.Errorf("restore: importing database: %w", err)
	}

	if man.MediaIncluded {
		mediaSrc := filepath.Join(path, "media")
		if info, statErr := os.Lstat(mediaSrc); statErr == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			if err := restoreMedia(mediaSrc, cfg.Media.Path); err != nil {
				return fmt.Errorf("restore: media: %w", err)
			}
		} else if statErr == nil {
			return fmt.Errorf("restore: media path must be a real directory")
		} else if !os.IsNotExist(statErr) {
			return fmt.Errorf("restore: inspect media path: %w", statErr)
		}
	}

	slog.Info("restore: completed", "path", path)
	return nil
}

func dumpConfig(dir string, cfg *config.Config) error {
	path := filepath.Join(dir, configFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	en := toml.NewEncoder(f)
	return en.Encode(cfg)
}

func dumpDatabase(ctx context.Context, dir, fileName string, store storage.Storage) error {
	path := filepath.Join(dir, fileName)
	return store.ExportDatabaseToFile(ctx, path)
}

func copyOwnedMedia(ctx context.Context, backupDir, mediaPath string, store storage.Storage) (int, error) {
	mediaDest := filepath.Join(backupDir, "media")
	count := 0
	cursor := ""
	for {
		rows, err := store.ListOwnedMediaAfter(ctx, cursor, "", 200)
		if err != nil {
			return count, fmt.Errorf("listing media: %w", err)
		}
		for _, m := range rows {
			src := filepath.Join(mediaPath, m.ID)
			if _, err := os.Stat(src); os.IsNotExist(err) {
				slog.Warn("backup: media dir missing on disk", "id", m.ID, "path", src)
				continue
			}
			dst := filepath.Join(mediaDest, m.ID)
			if err := copyDir(src, dst); err != nil {
				slog.Warn("backup: failed to copy media", "id", m.ID, "error", err)
				continue
			}
			count++
		}
		if len(rows) < 200 {
			return count, nil
		}
		cursor = rows[len(rows)-1].ID
	}
}

func writeManifest(dir string, man manifest) error {
	path := filepath.Join(dir, manifestFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(man)
}

func readManifest(dir string) (manifest, error) {
	var man manifest
	path := filepath.Join(dir, manifestFile)
	f, err := openRegularFile(path)
	if err != nil {
		return man, err
	}
	defer f.Close()

	if err := json.NewDecoder(f).Decode(&man); err != nil {
		return man, err
	}
	if man.Version != 1 {
		return man, fmt.Errorf("unsupported backup manifest version %d", man.Version)
	}
	return man, nil
}

func detectDialect(dsn string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return "pg"
	}
	return "sqlite"
}

func cleanupBackup(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("backup: failed to clean up partial backup", "path", dir, "error", err)
	}
}

func pruneOldBackups(backupCfg config.Backup) {
	if backupCfg.Retain <= 0 {
		return
	}

	entries, err := os.ReadDir(backupCfg.ResolvedPath())
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("backup: reading backup directory for rotation", "error", err)
		}
		return
	}

	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}

	if len(dirs) <= backupCfg.Retain {
		return
	}

	sort.Strings(dirs)
	toRemove := dirs[:len(dirs)-backupCfg.Retain]
	for _, name := range toRemove {
		path := filepath.Join(backupCfg.ResolvedPath(), name)
		if err := os.RemoveAll(path); err != nil {
			slog.Warn("backup: failed to remove old backup", "path", path, "error", err)
		} else {
			slog.Info("backup: removed old backup", "path", path)
		}
	}
}

func restoreMedia(src, dest string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("reading media backup: %w", err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		srcDir := filepath.Join(src, e.Name())
		dstDir := filepath.Join(dest, e.Name())
		if err := copyDir(srcDir, dstDir); err != nil {
			return fmt.Errorf("copying media %s: %w", e.Name(), err)
		}
	}
	return nil
}

func copyDir(src, dst string) error {
	srcInfo, err := os.Lstat(src)
	if err != nil || srcInfo.Mode()&os.ModeSymlink != 0 || !srcInfo.IsDir() {
		return fmt.Errorf("source is not a real directory: %s", src)
	}
	if info, err := os.Lstat(dst); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("destination is not a real directory: %s", dst)
		}
	} else if !os.IsNotExist(err) {
		return err
	} else if err := os.MkdirAll(dst, 0o750); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, e := range entries {
		if e.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("backup contains symbolic link %s", e.Name())
		}
		srcPath := filepath.Join(src, e.Name())
		dstPath := filepath.Join(dst, e.Name())

		if e.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}

		if err := copyFile(srcPath, dstPath); err != nil {
			return err
		}
	}
	return nil
}

const backupTimeLayout = "2006-01-02T150405Z"

// LastBackupTime returns the timestamp of the most recent completed backup,
// or zero time and false if no backup has ever been created.
func LastBackupTime(backupPath string) (time.Time, bool) {
	entries, err := os.ReadDir(backupPath)
	if err != nil {
		return time.Time{}, false
	}

	var latest time.Time
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Check that manifest exists (backup completed successfully).
		manifestPath := filepath.Join(backupPath, e.Name(), manifestFile)
		if _, err := os.Stat(manifestPath); err != nil {
			continue
		}
		t, err := time.Parse(backupTimeLayout, e.Name())
		if err != nil {
			continue
		}
		if t.After(latest) {
			latest = t
		}
	}
	if latest.IsZero() {
		return time.Time{}, false
	}
	return latest, true
}

func copyFile(src, dst string) error {
	in, err := openRegularFile(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := out.Name()
	defer os.Remove(tmpPath)
	if err := out.Chmod(0o600); err != nil {
		_ = out.Close()
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, dst)
}

func openRegularFile(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %s", path)
	}
	return os.Open(path)
}

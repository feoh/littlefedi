package mesh

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func openDirectorySQLiteWithDriver(path, driver string, maxConnections int) (*sql.DB, error) {
	path = strings.TrimSpace(path)
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("mesh directory: resolve database path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return nil, fmt.Errorf("mesh directory: create database directory: %w", err)
	}
	parameters := url.Values{"mode": {"rwc"}}
	for _, pragma := range []string{
		"journal_mode(WAL)",
		"busy_timeout(10000)",
		"synchronous(FULL)",
		"foreign_keys(1)",
	} {
		parameters.Add("_pragma", pragma)
	}
	dsn := (&url.URL{Scheme: "file", Path: absolute, RawQuery: parameters.Encode()}).String()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("mesh directory: open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(maxConnections)
	db.SetMaxIdleConns(maxConnections)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("mesh directory: open SQLite database: %w", err)
	}
	if err := os.Chmod(absolute, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("mesh directory: secure SQLite database: %w", err)
	}
	return db, nil
}

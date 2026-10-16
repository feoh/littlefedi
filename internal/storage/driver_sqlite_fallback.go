//go:build !postgres && !(darwin || (freebsd && (386 || amd64 || arm || arm64)) || (linux && (386 || amd64 || arm || arm64 || loong64 || ppc64le || riscv64 || s390x)) || (netbsd && amd64) || windows || (openbsd && (amd64 || arm64)))

package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log/slog"
	"net/url"
	"os/exec"
	"strconv"
	"strings"

	"github.com/ncruces/go-sqlite3"
	sqlite3driver "github.com/ncruces/go-sqlite3/driver"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
)

// Every ncruces connection owns a separate WASM arena. WithMaxMemory bounds
// usable memory to the same 32 MiB ceiling as the driver's 32-bit build and
// test suite. The current driver still makes its initial 256 MiB reservation
// before applying this limit. SQLiteMaxPoolSize caps the pool at two
// connections, or one where the OS charges that reservation against swap.
const (
	sqliteConnectionMemoryLimitBytes int64 = 32 << 20
	sqliteWASMDriver                       = true
	sqliteMmapDefaultBytes                 = 0
	sqliteCacheDefaultPages                = ""
)

type memoryLimitedSQLiteConnector struct{ driver.Connector }

func (c memoryLimitedSQLiteConnector) Connect(ctx context.Context) (driver.Conn, error) {
	return c.Connector.Connect(sqlite3.WithMaxMemory(ctx, sqliteConnectionMemoryLimitBytes))
}

func openStorageDB(dsn string) (*sql.DB, *bun.DB, string, error) {
	if isPostgresDSN(dsn) {
		return nil, nil, "", fmt.Errorf("storage: PostgreSQL support is not included in the default SQLite binary; rebuild with -tags postgres")
	}
	connector, err := (&sqlite3driver.SQLite{}).OpenConnector(normalizeSQLiteDSN(dsn))
	if err != nil {
		return nil, nil, "", fmt.Errorf("storage: open sqlite: %w", err)
	}
	sqldb := sql.OpenDB(memoryLimitedSQLiteConnector{connector})
	return sqldb, bun.NewDB(sqldb, sqlitedialect.New()), "sqlite", nil
}

func sqliteBackup(ctx context.Context, _ *sql.DB, dsn, path string) error {
	// A second fallback connection reserves another 256 MiB WASM arena, while
	// using the pooled connection would stop all database traffic until EOF.
	source := strings.TrimPrefix(strings.SplitN(dsn, "?", 2)[0], "file:")
	if strings.HasPrefix(dsn, "file:") {
		var err error
		source, err = url.PathUnescape(source)
		if err != nil {
			return err
		}
	}
	// VACUUM INTO rather than ".backup": the shell's backup command copies 100
	// pages per sqlite3_backup_step, and SQLite restarts the whole copy whenever
	// another connection writes the source in between. On a federating instance
	// those writes never stop arriving, so a large database can loop for hours
	// without ever producing a file. VACUUM INTO reads one snapshot start to
	// finish and writes a compacted copy. The caller gives it a new path inside
	// a private temporary directory, then applies 0600 before publishing it.
	shell, err := sqlite3Shell(ctx)
	if err != nil {
		return err
	}
	if shell.version[0] > 3 || shell.version[0] == 3 && shell.version[1] >= 27 {
		output, err := exec.CommandContext(ctx, shell.command, source, "VACUUM INTO "+quoteSQLiteString(path)).CombinedOutput()
		if err != nil {
			return fmt.Errorf("sqlite3 vacuum into: %w: %s", err, strings.TrimSpace(string(output)))
		}
		return nil
	}
	// Older shells parse the statement as a bare VACUUM and reject the rest, so
	// they get .backup. It still completes on a quiet instance, which beats
	// leaving the host with no backup.
	slog.Warn("storage: sqlite3 is older than 3.27 and has no VACUUM INTO; using .backup, which restarts on every write to the database",
		"sqlite3", shell.command, "fix", "install a newer sqlite3 or set LITTLEFEDI_SQLITE3")
	output, err := exec.CommandContext(ctx, shell.command, source, ".backup "+strconv.Quote(path)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sqlite3 backup: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// quoteSQLiteString renders s as a SQL string literal. VACUUM INTO takes its
// destination as an expression, not as a bindable dot-command argument.
func quoteSQLiteString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

//go:build !postgres

package storage

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// SQLiteArenaConstrained reports whether the compiled SQLite driver runs
// inside a WASM memory arena hard-capped at 32MB. Unbounded sort queries can
// exhaust that arena, so callers clamp risky config values when this is true.
func SQLiteArenaConstrained() bool {
	return sqliteWASMDriver
}

// SQLiteMaxPoolSize caps the connection pool for the compiled driver.
//
// The WASM fallback reserves 256MB of address space per connection before its
// own memory limit takes effect. Where the kernel overcommits, that reservation
// costs nothing: it never becomes resident, and the per-connection arena stays
// bounded by sqliteConnectionMemoryLimitBytes. illumos is the exception, since
// it charges the reservation against max-swap and a second connection can take
// down an otherwise idle 512MB zone.
//
// Two connections are worth having wherever they are affordable. With WAL a
// reader does not block on a writer, so the single-connection pool is what
// makes an interactive request queue behind whatever background write got there
// first, and that wait is the bulk of what the slow-statement log reports on
// small hosts.
// Zero and negative sizes are capped too, and not passed through. database/sql
// reads a non-positive SetMaxOpenConns as "unlimited", the configuration
// accepts the value without complaint, and on this driver an unlimited pool is
// an unlimited number of 32MB arenas, which this build cannot survive.
func SQLiteMaxPoolSize(requested int) int {
	if !sqliteWASMDriver {
		return requested
	}
	return clampWASMPoolSize(requested, sqliteReservationCharged)
}

// clampWASMPoolSize is split out from SQLiteMaxPoolSize so the arithmetic can
// be tested on any platform, rather than only on the ones that compile the
// fallback driver.
func clampWASMPoolSize(requested int, reservationCharged bool) int {
	limit := 2
	if reservationCharged {
		limit = 1
	}
	if requested > limit || requested <= 0 {
		return limit
	}
	return requested
}

// SQLiteArenaBytes reports how much memory the compiled driver reserves for
// database work, given the configured pool size. It is zero on the native
// builds, where SQLite allocates through the C heap rather than a fixed arena.
//
// The WASM arena is an anonymous mmap, committed a page at a time as the engine
// grows into it, so it is resident memory on the host but invisible to
// debug.SetMemoryLimit. Anything sizing this process against the host has to
// subtract it from what the Go heap may claim, never add it to the limit.
//
// requested is passed through SQLiteMaxPoolSize first, because the driver caps
// the pool regardless of what the configuration asked for.
func SQLiteArenaBytes(requested int) int64 {
	if !sqliteWASMDriver {
		return 0
	}
	return sqliteConnectionMemoryLimitBytes * int64(SQLiteMaxPoolSize(requested))
}

// vacuumIntoSupported takes an sqlite3 --version line ("3.26.0 2018-12-01 ...")
// and reports whether that release has VACUUM INTO, added in 3.27.0. NetBSD 10
// ships 3.26.0 in base and misses it by one release. Kept out of the fallback
// driver file so the parsing can be tested on any platform.
func vacuumIntoSupported(version string) bool {
	parsed, ok := sqliteVersion(version)
	return ok && (parsed[0] > 3 || (parsed[0] == 3 && parsed[1] >= 27))
}

const sqlite3CommandEnv = "LITTLEFEDI_SQLITE3"

type sqliteShell struct {
	command string
	version [3]int
}

// sqlite3Shell selects the newest usable sqlite3 visible on PATH. pkgsrc's
// /usr/pkg/bin is checked as well because NetBSD base commonly appears first.
// LITTLEFEDI_SQLITE3 wins when an operator needs a specific one.
func sqlite3Shell(ctx context.Context) (sqliteShell, error) {
	if command := os.Getenv(sqlite3CommandEnv); command != "" {
		version, err := sqlite3Version(ctx, command)
		if err != nil {
			return sqliteShell{}, err
		}
		return sqliteShell{command, version}, nil
	}

	var shells []sqliteShell
	for _, command := range sqlite3Candidates(os.Getenv("PATH")) {
		version, err := sqlite3Version(ctx, command)
		if err == nil {
			shells = append(shells, sqliteShell{command, version})
		}
	}
	if shell, ok := newestSQLiteShell(shells); ok {
		return shell, nil
	}
	return sqliteShell{}, fmt.Errorf("no usable sqlite3 command found (set %s to its path)", sqlite3CommandEnv)
}

func sqlite3Candidates(path string) []string {
	seen := map[string]bool{}
	var candidates []string
	for _, dir := range append(filepath.SplitList(path), "/usr/pkg/bin") {
		command := filepath.Join(dir, "sqlite3")
		if !seen[command] {
			seen[command] = true
			candidates = append(candidates, command)
		}
	}
	return candidates
}

func sqlite3Version(ctx context.Context, command string) ([3]int, error) {
	out, err := exec.CommandContext(ctx, command, "--version").Output()
	if err != nil {
		return [3]int{}, fmt.Errorf("sqlite3 command %q: %w", command, err)
	}
	version, ok := sqliteVersion(string(out))
	if !ok {
		return [3]int{}, fmt.Errorf("sqlite3 command %q returned an invalid version %q", command, strings.TrimSpace(string(out)))
	}
	return version, nil
}

func sqliteVersion(output string) ([3]int, bool) {
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return [3]int{}, false
	}
	parts := strings.Split(fields[0], ".")
	if len(parts) < 2 || len(parts) > 3 {
		return [3]int{}, false
	}
	var version [3]int
	for i, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return [3]int{}, false
		}
		version[i] = value
	}
	return version, true
}

func newestSQLiteShell(shells []sqliteShell) (sqliteShell, bool) {
	var newest sqliteShell
	for _, shell := range shells {
		if newest.command == "" || shell.version[0] > newest.version[0] || shell.version[0] == newest.version[0] && (shell.version[1] > newest.version[1] || shell.version[1] == newest.version[1] && shell.version[2] > newest.version[2]) {
			newest = shell
		}
	}
	return newest, newest.command != ""
}

func normalizeSQLiteDSN(dsn string) string {
	if strings.HasPrefix(dsn, "file:") {
		return dsn
	}
	parts := strings.SplitN(dsn, "?", 2)
	if len(parts) != 2 {
		return dsn
	}
	values, err := url.ParseQuery(parts[1])
	if err != nil {
		return dsn
	}
	legacy := []struct {
		key, pragma string
	}{
		{"_journal_mode", "journal_mode"},
		{"_busy_timeout", "busy_timeout"},
		{"_foreign_keys", "foreign_keys"},
	}
	for _, item := range legacy {
		for _, value := range values[item.key] {
			values.Add("_pragma", item.pragma+"("+value+")")
		}
		values.Del(item.key)
	}
	addSQLitePerfDefaults(values)
	return (&url.URL{Scheme: "file", OmitHost: true, Path: filepath.ToSlash(parts[0]), RawQuery: values.Encode()}).String()
}

// addSQLitePerfDefaults injects performance pragmas that are safe defaults for
// low-power devices (e.g. a Raspberry Pi Zero W, where SD-card fsyncs dominate)
// unless the operator has already set them explicitly. It never overrides an
// existing value.
func addSQLitePerfDefaults(values url.Values) {
	// Index the pragmas already requested by name -> (lowercased) argument.
	present := make(map[string]string, len(values["_pragma"]))
	for _, p := range values["_pragma"] {
		name, arg := p, ""
		if i := strings.IndexByte(p, '('); i >= 0 {
			name = p[:i]
			arg = strings.TrimSuffix(p[i+1:], ")")
		}
		present[strings.ToLower(strings.TrimSpace(name))] = strings.ToLower(strings.TrimSpace(arg))
	}

	// temp_store(MEMORY): keep sort / GROUP BY scratch b-trees in RAM instead of
	// spilling temp files to disk. Safe in any journal mode.
	if _, ok := present["temp_store"]; !ok {
		values.Add("_pragma", "temp_store(MEMORY)")
	}

	// synchronous(NORMAL): drop the per-commit fsync. Under WAL this is still
	// crash-safe (only an OS/power loss can cost the last transaction, never
	// corrupt the database), so gate it on WAL being the active journal mode.
	if _, ok := present["synchronous"]; !ok && present["journal_mode"] == "wal" {
		values.Add("_pragma", "synchronous(NORMAL)")
	}

	// mmap_size / cache_size defaults are driver-gated (zero/empty on the WASM
	// fallback build, whose 32-bit arena cannot afford them).
	if _, ok := present["mmap_size"]; !ok && sqliteMmapDefaultBytes > 0 {
		values.Add("_pragma", fmt.Sprintf("mmap_size(%d)", sqliteMmapDefaultBytes))
	}
	if _, ok := present["cache_size"]; !ok && sqliteCacheDefaultPages != "" {
		values.Add("_pragma", "cache_size("+sqliteCacheDefaultPages+")")
	}
}

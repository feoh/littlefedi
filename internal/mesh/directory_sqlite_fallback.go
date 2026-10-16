//go:build !(darwin || (freebsd && (386 || amd64 || arm || arm64)) || (linux && (386 || amd64 || arm || arm64 || loong64 || ppc64le || riscv64 || s390x)) || (netbsd && amd64) || windows || (openbsd && (amd64 || arm64)))

package mesh

import (
	"database/sql"

	_ "github.com/ncruces/go-sqlite3/driver"
)

func openDirectorySQLite(path string) (*sql.DB, error) {
	// The portable WASM driver uses one bounded connection; add a
	// native driver for this platform only if directory load justifies it.
	return openDirectorySQLiteWithDriver(path, "sqlite3", 1)
}

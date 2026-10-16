//go:build darwin || (freebsd && (386 || amd64 || arm || arm64)) || (linux && (386 || amd64 || arm || arm64 || loong64 || ppc64le || riscv64 || s390x)) || (netbsd && amd64) || windows || (openbsd && (amd64 || arm64))

package mesh

import (
	"database/sql"

	_ "modernc.org/sqlite"
)

func openDirectorySQLite(path string) (*sql.DB, error) {
	return openDirectorySQLiteWithDriver(path, "sqlite", 8)
}

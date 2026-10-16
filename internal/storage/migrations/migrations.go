// Package migrations holds all database migrations.
// Migrations use bun's migration system.
package migrations

import "github.com/uptrace/bun/migrate"

// Migrations is the collection of all database migrations.
var Migrations = migrate.NewMigrations()

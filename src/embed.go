// Package dbfiles embeds the SQL that defines and seeds the database, so the
// server binary carries its own migrations. The files stay where they are and
// remain runnable with psql; see README.md in this directory.
package dbfiles

import "embed"

// FS holds migrations/*.sql and seed/*.sql.
//
//go:embed migrations/*.sql seed/*.sql
var FS embed.FS

const (
	MigrationsDir = "migrations"
	PresetsSeed   = "seed/presets.sql"
)

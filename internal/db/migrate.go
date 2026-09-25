package db

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	dbfiles "github.com/AIShiteru-LMS/AIShiteru-Core/src"
)

// Migrator applies the migrations embedded in the binary. Each file carries
// its own BEGIN/COMMIT and is sent as one simple-protocol batch, so a file
// either applies whole or not at all; golang-migrate's dirty flag covers the
// remaining gap between a file committing and its version being recorded.
type Migrator struct {
	m *migrate.Migrate
}

// NewMigrator opens its own connection; Close releases it.
func NewMigrator(databaseURL string) (*Migrator, error) {
	src, err := iofs.New(dbfiles.FS, dbfiles.MigrationsDir)
	if err != nil {
		return nil, fmt.Errorf("embedded migrations: %w", err)
	}
	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	drv, err := pgxmigrate.WithInstance(sqlDB, &pgxmigrate.Config{})
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("migration driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", drv)
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Migrator{m: m}, nil
}

func (g *Migrator) Close() error {
	srcErr, dbErr := g.m.Close()
	return errors.Join(srcErr, dbErr)
}

// Up applies every pending migration. Already current is not an error.
//
// A database already past the newest migration the binary carries is left as
// it is: a newer release has migrated it, and this binary is older — a
// rollback. There is nothing here to apply, and golang-migrate would fail
// looking for the file of a version it does not know. serve starts against a
// schema that is ahead (that is what a rolling deploy looks like), so the
// `migrate up` a deploy runs first must not be what stops a rollback. It
// cannot tell a rollback from a migration taken out of main, which
// CONTRIBUTING.md rules out; the migrate command says the schema is ahead.
func (g *Migrator) Up() error {
	v, dirty, err := g.Version()
	if err != nil {
		return err
	}
	if !dirty {
		latest, err := LatestEmbedded()
		if err != nil {
			return err
		}
		if v > latest {
			return nil
		}
	}
	return ignoreNoChange(g.m.Up())
}

// Down reverts every applied migration and destroys all data.
func (g *Migrator) Down() error { return ignoreNoChange(g.m.Down()) }

// Steps moves n migrations up (n > 0) or down (n < 0).
func (g *Migrator) Steps(n int) error { return ignoreNoChange(g.m.Steps(n)) }

// Force records version without running anything. It is how a database first
// built with psql -f is adopted, and how a dirty flag is cleared after the
// cause has been fixed by hand. Version 0 records that no migration has run,
// as Version reports it: no migration is numbered 0, and a row saying 0 would
// leave the next Up looking for one.
func (g *Migrator) Force(version int) error {
	if version == 0 {
		version = database.NilVersion
	}
	return g.m.Force(version)
}

// Version reports the applied version; 0 means no migration has run.
func (g *Migrator) Version() (version uint, dirty bool, err error) {
	version, dirty, err = g.m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	return version, dirty, err
}

// LatestEmbedded is the highest migration version the binary carries.
func LatestEmbedded() (uint, error) {
	src, err := iofs.New(dbfiles.FS, dbfiles.MigrationsDir)
	if err != nil {
		return 0, err
	}
	defer func() { _ = src.Close() }()
	v, err := src.First()
	if err != nil {
		return 0, err
	}
	for {
		next, err := src.Next(v)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
			return v, nil
		}
		if err != nil {
			return 0, err
		}
		v = next
	}
}

func ignoreNoChange(err error) error {
	if errors.Is(err, migrate.ErrNoChange) {
		return nil
	}
	return err
}

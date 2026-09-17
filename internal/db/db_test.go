package db_test

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testdb"
	dbfiles "github.com/AIShiteru-LMS/AIShiteru-Core/src"
)

// Every migration must be reversible, and reversing must leave nothing behind
// that stops it applying again.
func TestMigrateUpDownUp(t *testing.T) {
	pool, url := testdb.NewEmpty(t)
	ctx := context.Background()

	latest, err := db.LatestEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	if latest == 0 {
		t.Fatal("no embedded migrations")
	}

	m, err := db.NewMigrator(url)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	assertVersion := func(want uint) {
		t.Helper()
		got, dirty, err := m.Version()
		if err != nil {
			t.Fatal(err)
		}
		if got != want || dirty {
			t.Fatalf("version = %d (dirty=%v), want %d", got, dirty, want)
		}
	}
	countTables := func() int {
		t.Helper()
		var n int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name <> 'schema_migrations'`).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	assertVersion(0)
	if err := m.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}
	assertVersion(latest)
	tables := countTables()
	if tables == 0 {
		t.Fatal("up created no tables")
	}

	if err := m.Up(); err != nil {
		t.Fatalf("second up should be a no-op: %v", err)
	}

	if err := m.Down(); err != nil {
		t.Fatalf("down: %v", err)
	}
	assertVersion(0)
	if n := countTables(); n != 0 {
		t.Fatalf("down left %d tables behind", n)
	}
	var leftovers int
	err = pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
		  WHERE n.nspname = 'public' AND t.typtype = 'e') +
		(SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		  WHERE n.nspname = 'public')`).Scan(&leftovers)
	if err != nil {
		t.Fatal(err)
	}
	if leftovers != 0 {
		t.Fatalf("down left %d enum types or functions behind", leftovers)
	}

	if err := m.Up(); err != nil {
		t.Fatalf("up after down: %v", err)
	}
	assertVersion(latest)
	if n := countTables(); n != tables {
		t.Fatalf("second up created %d tables, first created %d", n, tables)
	}
}

func TestEveryMigrationHasBothDirections(t *testing.T) {
	entries, err := fs.ReadDir(dbfiles.FS, dbfiles.MigrationsDir)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string][2]bool{}
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".up.sql"):
			s := seen[strings.TrimSuffix(name, ".up.sql")]
			s[0] = true
			seen[strings.TrimSuffix(name, ".up.sql")] = s
		case strings.HasSuffix(name, ".down.sql"):
			s := seen[strings.TrimSuffix(name, ".down.sql")]
			s[1] = true
			seen[strings.TrimSuffix(name, ".down.sql")] = s
		default:
			t.Errorf("%s is neither .up.sql nor .down.sql", name)
		}
	}
	for base, s := range seen {
		if !s[0] || !s[1] {
			t.Errorf("%s: up=%v down=%v, want both", base, s[0], s[1])
		}
	}
}

func TestSeedIsIdempotent(t *testing.T) {
	pool := testdb.New(t) // the template is already seeded once
	ctx := context.Background()
	q := dbq.New(pool)

	before, err := q.CountBuiltinPresets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before != 6 {
		t.Fatalf("built-in presets = %d, want 6", before)
	}
	if err := db.Seed(ctx, pool); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	after, err := q.CountBuiltinPresets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("re-seeding changed the preset count: %d -> %d", before, after)
	}
}

func TestInTx(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	insert := func(tx pgx.Tx, name string) error {
		_, err := tx.Exec(ctx, `INSERT INTO department (name) VALUES ($1)`, name)
		return err
	}
	count := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM department`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if err := db.InTx(ctx, pool, func(tx pgx.Tx) error { return insert(tx, "kept") }); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("after commit: %d rows, want 1", n)
	}

	boom := errors.New("boom")
	err := db.InTx(ctx, pool, func(tx pgx.Tx) error {
		if err := insert(tx, "discarded"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if n := count(); n != 1 {
		t.Fatalf("after rollback: %d rows, want 1", n)
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic did not propagate")
			}
		}()
		_ = db.InTx(ctx, pool, func(tx pgx.Tx) error {
			_ = insert(tx, "panicked")
			panic("boom")
		})
	}()
	if n := count(); n != 1 {
		t.Fatalf("after panic: %d rows, want 1", n)
	}
}

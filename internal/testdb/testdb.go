// Package testdb gives each test its own PostgreSQL database.
//
// TEST_DATABASE_URL must point at a maintenance database (usually "postgres")
// on a server where the role may CREATE DATABASE. The first test to run builds
// a template with every migration and the seed applied; each test then gets a
// copy, which is a file-level clone and takes a few tens of milliseconds.
//
// The template's name includes a hash of the embedded SQL, so editing a
// migration produces a new template instead of reusing a stale one.
package testdb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AIShie-Education/AIShie-Core/internal/db"
	dbfiles "github.com/AIShie-Education/AIShie-Core/src"
)

const envURL = "TEST_DATABASE_URL"

// templateLockKey serialises template creation across test binaries, which
// `go test ./...` runs in parallel. Arbitrary, but fixed.
const templateLockKey = 0x41495348 // "AISH"

var (
	templateOnce sync.Once
	templateName string
	templateErr  error
)

// New returns a pool on a fresh database with all migrations and the seed
// applied. The database is dropped when the test ends.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	pool, _ := create(t, true)
	return pool
}

// NewWithURL is New, along with the database's URL, for tests of what opens
// a connection of its own: the operator's command line.
func NewWithURL(t testing.TB) (*pgxpool.Pool, string) {
	t.Helper()
	return create(t, true)
}

// NewEmpty returns a pool on a fresh database with nothing in it, along with
// its URL, for tests of the migrations themselves.
func NewEmpty(t testing.TB) (*pgxpool.Pool, string) {
	t.Helper()
	return create(t, false)
}

func create(t testing.TB, fromTemplate bool) (*pgxpool.Pool, string) {
	t.Helper()
	adminURL := os.Getenv(envURL)
	if adminURL == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s is not set", envURL)
		}
		t.Skipf("%s is not set; skipping database test", envURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to %s: %v", envURL, err)
	}
	defer admin.Close(ctx)

	name := "aishie_t_" + randomSuffix()
	stmt := "CREATE DATABASE " + pgx.Identifier{name}.Sanitize()
	if fromTemplate {
		tmpl, err := ensureTemplate(ctx, admin, adminURL)
		if err != nil {
			t.Fatalf("build template database: %v", err)
		}
		stmt += " TEMPLATE " + pgx.Identifier{tmpl}.Sanitize()
	}
	if _, err := admin.Exec(ctx, stmt); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	testURL := withDatabase(adminURL, name)
	pool, err := db.Open(ctx, testURL)
	if err != nil {
		dropDatabase(adminURL, name)
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropDatabase(adminURL, name)
	})
	return pool, testURL
}

func ensureTemplate(ctx context.Context, admin *pgx.Conn, adminURL string) (string, error) {
	templateOnce.Do(func() {
		templateName, templateErr = buildTemplate(ctx, admin, adminURL)
	})
	return templateName, templateErr
}

func buildTemplate(ctx context.Context, admin *pgx.Conn, adminURL string) (string, error) {
	sum, err := sqlFingerprint()
	if err != nil {
		return "", err
	}
	name := "aishie_tmpl_" + sum

	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", int64(templateLockKey)); err != nil {
		return "", fmt.Errorf("template lock: %w", err)
	}
	defer admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", int64(templateLockKey))

	var exists bool
	if err := admin.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		return "", err
	}
	if exists {
		return name, nil
	}

	// Build under a scratch name and rename at the end, so a crash half-way
	// never leaves a broken template behind under the real name.
	building := name + "_building"
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{building}.Sanitize()); err != nil {
		return "", err
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{building}.Sanitize()); err != nil {
		return "", err
	}
	buildURL := withDatabase(adminURL, building)
	if err := migrateAndSeed(ctx, buildURL); err != nil {
		dropDatabase(adminURL, building)
		return "", err
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf("ALTER DATABASE %s RENAME TO %s",
		pgx.Identifier{building}.Sanitize(), pgx.Identifier{name}.Sanitize())); err != nil {
		dropDatabase(adminURL, building)
		return "", err
	}
	return name, nil
}

func migrateAndSeed(ctx context.Context, url string) error {
	m, err := db.NewMigrator(url)
	if err != nil {
		return err
	}
	if err := m.Up(); err != nil {
		_ = m.Close()
		return fmt.Errorf("migrate up: %w", err)
	}
	if err := m.Close(); err != nil {
		return err
	}
	pool, err := db.Open(ctx, url)
	if err != nil {
		return err
	}
	// A template cannot be copied while anyone is connected to it.
	defer pool.Close()
	return db.Seed(ctx, pool)
}

// sqlFingerprint hashes every embedded SQL file, names included.
func sqlFingerprint() (string, error) {
	var paths []string
	err := fs.WalkDir(dbfiles.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		b, err := dbfiles.FS.ReadFile(p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

func dropDatabase(adminURL, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return
	}
	defer admin.Close(ctx)
	// WITH (FORCE) is PostgreSQL 13+, the project's minimum.
	_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
}

func withDatabase(rawURL, name string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic(fmt.Sprintf("%s: %v", envURL, err))
	}
	u.Path = "/" + name
	return u.String()
}

func randomSuffix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

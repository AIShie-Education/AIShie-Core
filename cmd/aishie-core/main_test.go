package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/config"
	"github.com/AIShie-Education/AIShie-Core/internal/testdb"
)

// A rollback's `migrate up` changes nothing, and a revert that took a
// migration out looks the same; the deploy log is where either shows.
func TestSchemaReportSaysASchemaIsAhead(t *testing.T) {
	for _, c := range []struct {
		version, latest uint
		dirty           bool
		want            string
	}{
		{3, 3, false, "schema version 3 (embedded latest 3)"},
		{2, 3, false, "schema version 2 (embedded latest 3)"},
		{4, 3, false, "schema version 4 (embedded latest 3) AHEAD"},
		{4, 3, true, "schema version 4 (embedded latest 3) DIRTY"},
		{3, 3, true, "schema version 3 (embedded latest 3) DIRTY"},
	} {
		got := schemaReport(c.version, c.latest, c.dirty)
		if !strings.HasPrefix(got, c.want) {
			t.Errorf("schemaReport(%d, %d, %v) = %q, want it to start with %q", c.version, c.latest, c.dirty, got, c.want)
		}
		if !strings.Contains(c.want, "AHEAD") && strings.Contains(got, "AHEAD") {
			t.Errorf("schemaReport(%d, %d, %v) = %q, which is not ahead", c.version, c.latest, c.dirty, got)
		}
	}
}

// captureStdout runs f and returns what it wrote to standard output.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	defer func() { os.Stdout = saved }()
	f()
	os.Stdout = saved
	_ = w.Close()
	return <-done
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Bootstrap makes root with a password and an email or a login ID to sign
// in with, and prints no API token: nothing on standard output, which the
// deploy discards, and on standard error the two actors and what root signs
// in with. Without a password or a name to sign in with it makes nothing.
func TestBootstrapPrintsNoToken(t *testing.T) {
	pool, url := testdb.NewWithURL(t)
	cfg := config.Config{DatabaseURL: url}
	run := func(stdin string, args ...string) (stdout, stderr string, err error) {
		t.Helper()
		var e bytes.Buffer
		stdout = captureStdout(t, func() { err = bootstrap(cfg, args, strings.NewReader(stdin), &e) })
		return stdout, e.String(), err
	}

	for name, c := range map[string]struct {
		stdin string
		args  []string
	}{
		"no password":             {"", []string{"--name", "Root", "--email", "root@example.edu"}},
		"an empty password":       {"\n", []string{"--name", "Root", "--email", "root@example.edu", "--password-stdin"}},
		"a short password":        {"short\n", []string{"--name", "Root", "--email", "root@example.edu", "--password-stdin"}},
		"nothing to sign in with": {"a long enough password\n", []string{"--name", "Root", "--password-stdin"}},
		"no name":                 {"a long enough password\n", []string{"--email", "root@example.edu", "--password-stdin"}},
		"an email that is none":   {"a long enough password\n", []string{"--name", "Root", "--email", "root", "--password-stdin"}},
	} {
		if stdout, _, err := run(c.stdin, c.args...); err == nil || stdout != "" {
			t.Errorf("%s: %v, stdout %q", name, err, stdout)
		}
	}
	if n := count(t, pool, `SELECT count(*) FROM actor`); n != 0 {
		t.Fatalf("refused bootstraps made %d actors", n)
	}

	stdout, stderr, err := run("a long enough password\n", "--name", "Root", "--email", "root@example.edu", "--password-stdin")
	if err != nil {
		t.Fatal(err)
	}
	var root, system uuid.UUID
	if err := pool.QueryRow(context.Background(), `SELECT id FROM actor WHERE platform_role = 'root'`).Scan(&root); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT id FROM actor WHERE kind = 'system'`).Scan(&system); err != nil {
		t.Fatal(err)
	}
	if stdout != "" || strings.Contains(stderr, "ais_") || !strings.Contains(stderr, root.String()) || !strings.Contains(stderr, system.String()) ||
		!strings.Contains(stderr, "Sign in at your site with root@example.edu and that password") {
		t.Fatalf("stdout %q, stderr %q", stdout, stderr)
	}
	if n := count(t, pool, `SELECT count(*) FROM credential WHERE kind = 'api_token'`); n != 0 {
		t.Fatalf("bootstrap made %d API tokens", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM credential WHERE actor_id = $1 AND kind = 'password' AND revoked_at IS NULL`, root); n != 1 {
		t.Fatal("root has no password")
	}
	if sess, err := auth.NewAuthenticator(pool, time.Hour).Login(context.Background(), "ROOT@example.edu", "a long enough password"); err != nil || sess.ActorID != root {
		t.Fatalf("root signing in: %+v %v", sess, err)
	}
	if _, _, err := run("another long password\n", "--name", "Usurper", "--email", "usurper@example.edu", "--password-stdin"); !errors.Is(err, auth.ErrAlreadyBootstrapped) {
		t.Fatalf("a second bootstrap: %v", err)
	}
}

// Root may sign in with a login ID alone, a staff number, as anyone may.
func TestBootstrapTakesALoginID(t *testing.T) {
	pool, url := testdb.NewWithURL(t)
	var stderr bytes.Buffer
	var err error
	stdout := captureStdout(t, func() {
		err = bootstrap(config.Config{DatabaseURL: url}, []string{"--name", "Root", "--login-id", "T0001", "--password-stdin"},
			strings.NewReader("a long enough password\r\n"), &stderr)
	})
	if err != nil || stdout != "" || !strings.Contains(stderr.String(), "Sign in at your site with T0001 and that password") {
		t.Fatalf("%v; stdout %q; stderr %q", err, stdout, stderr.String())
	}
	if _, err := auth.NewAuthenticator(pool, time.Hour).Login(context.Background(), "t0001", "a long enough password"); err != nil {
		t.Fatalf("root signing in with the login ID: %v", err)
	}
}

// The command line issues an agent a token, and a person none: people sign
// in. Nor the system actor.
func TestTokenIssueIsForAgents(t *testing.T) {
	pool, url := testdb.NewWithURL(t)
	cfg := config.Config{DatabaseURL: url}
	res, err := auth.Bootstrap(context.Background(), pool, auth.BootstrapInput{DisplayName: "Root", Email: "root@example.edu",
		LoginID: "T0001", Password: "a long enough password"})
	if err != nil {
		t.Fatal(err)
	}
	agent := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(context.Background(), `INSERT INTO actor (id, kind, display_name, email, created_by_actor_id)
		VALUES ($1, 'agent', 'grader', 'grader@example.edu', $2)`, agent, res.RootID); err != nil {
		t.Fatal(err)
	}
	issue := func(who string) (string, string, error) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		err := token(cfg, []string{"issue", "--actor", who, "--label", "cli", "--days", "30"}, &stdout, &stderr)
		return stdout.String(), stderr.String(), err
	}
	for _, who := range []string{res.RootID.String(), "root@example.edu", "T0001"} {
		stdout, _, err := issue(who)
		e, ok := apperr.As(err)
		if !ok || e.Code != apperr.Forbidden || e.Details["reason"] != auth.ReasonTokensForAgents || stdout != "" {
			t.Fatalf("a token for root, as %s: %v; stdout %q", who, err, stdout)
		}
	}
	if stdout, _, err := issue(res.SystemID.String()); err == nil || stdout != "" {
		t.Fatalf("a token for the system actor: %v; stdout %q", err, stdout)
	}
	if n := count(t, pool, `SELECT count(*) FROM credential WHERE kind = 'api_token'`); n != 0 {
		t.Fatalf("%d tokens made for those who may hold none", n)
	}
	for _, who := range []string{agent.String(), "GRADER@example.edu"} {
		stdout, stderr, err := issue(who)
		tok := strings.TrimSpace(stdout)
		if err != nil || !strings.HasPrefix(tok, "ais_") || strings.Contains(stderr, tok) || !strings.Contains(stderr, agent.String()) {
			t.Fatalf("a token for the agent, as %s: %v; stdout %q; stderr %q", who, err, stdout, stderr)
		}
		p, err := auth.NewAuthenticator(pool, time.Hour).Authenticate(context.Background(), tok)
		if err != nil || p.ActorID != agent || p.ExpiresAt == nil {
			t.Fatalf("the agent's token: %+v %v", p, err)
		}
	}
}

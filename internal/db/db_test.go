package db_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/db"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/testdb"
	dbfiles "github.com/AIShie-Education/AIShie-Core/src"
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

// Rolling back to an older binary is running its `migrate up` against a
// schema a newer release has already moved on: there is nothing to apply, and
// it is not an error. A dirty schema still is, whatever its version.
func TestMigrateUpLeavesANewerSchemaAlone(t *testing.T) {
	pool, url := testdb.NewEmpty(t)
	ctx := context.Background()
	latest, err := db.LatestEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	m, err := db.NewMigrator(url)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}

	// What a newer release leaves behind: its migration applied, and a
	// version this binary has no file for.
	ahead := latest + 1
	if _, err := pool.Exec(ctx, `UPDATE schema_migrations SET version = $1`, ahead); err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil {
		t.Fatalf("up against a newer schema: %v", err)
	}
	if v, dirty, err := m.Version(); err != nil || v != ahead || dirty {
		t.Fatalf("version = %d (dirty=%v, %v), want %d left as it was", v, dirty, err, ahead)
	}

	if _, err := pool.Exec(ctx, `UPDATE schema_migrations SET dirty = true`); err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err == nil {
		t.Fatal("up against a dirty schema said nothing")
	}
}

// A first migration that fails has applied nothing, and leaves version 1
// recorded as dirty. Forcing 0 once the cause is fixed must record what is
// true, that no migration has run, and the migrations then apply from there.
func TestForcingZeroRecoversFromAFailedFirstMigration(t *testing.T) {
	pool, url := testdb.NewEmpty(t)
	ctx := context.Background()

	latest, err := db.LatestEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	// A migrator of its own for each step, as each command of the CLI has.
	migrator := func() *db.Migrator {
		t.Helper()
		m, err := db.NewMigrator(url)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	assertVersion := func(m *db.Migrator, want uint, wantDirty bool) {
		t.Helper()
		got, dirty, err := m.Version()
		if err != nil {
			t.Fatal(err)
		}
		if got != want || dirty != wantDirty {
			t.Fatalf("version = %d (dirty=%v), want %d (dirty=%v)", got, dirty, want, wantDirty)
		}
	}

	// 0001 creates this type, and fails if it is there already.
	if _, err := pool.Exec(ctx, `CREATE TYPE autonomy_level AS ENUM ('x')`); err != nil {
		t.Fatal(err)
	}
	failed := migrator()
	if err := failed.Up(); err == nil {
		t.Fatal("up succeeded over a type that was in its way")
	}
	failed.Close()
	if _, err := pool.Exec(ctx, `DROP TYPE autonomy_level`); err != nil {
		t.Fatal(err)
	}

	m := migrator()
	defer m.Close()
	assertVersion(m, 1, true)
	if err := m.Force(0); err != nil {
		t.Fatalf("force 0: %v", err)
	}
	assertVersion(m, 0, false)
	if v, dirty, err := db.SchemaVersion(ctx, pool); err != nil || v != 0 || dirty {
		t.Fatalf("SchemaVersion = %d (dirty=%v) %v, want 0", v, dirty, err)
	}
	if err := m.Up(); err != nil {
		t.Fatalf("up after force 0: %v", err)
	}
	assertVersion(m, latest, false)
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
	if before != 8 {
		t.Fatalf("built-in presets = %d, want 8", before)
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

// Migration 0007 gives every seat that is not removed, and every
// department's own preset, the new permissions of the built-in preset of the
// same roster role; an assistant keeps 'denied', and a removed seat is
// history. Going down with a delegate seat present removes it and cancels its
// proposal, as a removal would.
func TestAgentOwnershipBackfillsAndComesOffCleanly(t *testing.T) {
	pool, url := testdb.NewEmpty(t)
	ctx := context.Background()
	m, err := db.NewMigrator(url)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Steps(6); err != nil {
		t.Fatalf("up to 0006: %v", err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO actor (id, kind, display_name, platform_role) VALUES ('00000000-0000-0000-0000-000000000001', 'human', 'root', 'root')`)
	exec(`INSERT INTO term (id, name, starts_on, ends_on) VALUES ('00000000-0000-0000-0000-000000000002', 'T', '2026-09-01', '2026-12-20')`)
	exec(`INSERT INTO department (id, name) VALUES ('00000000-0000-0000-0000-000000000003', 'D')`)
	exec(`INSERT INTO course (id, dept_id, term_id, code, title, created_by_actor_id) VALUES
		('00000000-0000-0000-0000-000000000004', '00000000-0000-0000-0000-000000000003', '00000000-0000-0000-0000-000000000002', 'C', 'C',
		 '00000000-0000-0000-0000-000000000001')`)
	roles := []string{"student", "ta", "instructor", "observer", "assistant"}
	for i, role := range roles {
		exec(`INSERT INTO actor (id, kind, display_name) VALUES ($1, 'human', $2)`, seatID(10+i), role)
		exec(`INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope)
			VALUES ($1, '00000000-0000-0000-0000-000000000004', $1, $2, '00000000-0000-0000-0000-000000000001', 'all', 'all')`, seatID(10+i), role)
		exec(`INSERT INTO permission_preset (dept_id, name, role, student_scope, assignment_scope)
			VALUES ('00000000-0000-0000-0000-000000000003', $1, $1, 'all', 'all')`, role)
	}
	exec(`INSERT INTO actor (id, kind, display_name) VALUES ($1, 'human', 'gone')`, seatID(20))
	exec(`INSERT INTO course_member (id, course_id, actor_id, role, status, added_by_actor_id, student_scope, assignment_scope)
		VALUES ($1, '00000000-0000-0000-0000-000000000004', $1, 'instructor', 'removed', '00000000-0000-0000-0000-000000000001', 'all', 'all')`, seatID(20))
	if err := m.Steps(1); err != nil {
		t.Fatalf("up to 0007: %v", err)
	}

	want := map[string][3]string{
		"student":    {"confirm_required", "autonomous", "denied"},
		"ta":         {"confirm_required", "autonomous", "denied"},
		"instructor": {"autonomous", "autonomous", "autonomous"},
		"observer":   {"denied", "denied", "denied"},
		"assistant":  {"denied", "denied", "denied"},
	}
	levels := func(sql string, args ...any) [3]string {
		t.Helper()
		var l [3]string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&l[0], &l[1], &l[2]); err != nil {
			t.Fatal(err)
		}
		return l
	}
	for i, role := range roles {
		if got := levels(`SELECT perm_agent_delegate::text, perm_conversation_ask::text, perm_conversation_answer::text
			FROM course_member WHERE id = $1`, seatID(10+i)); got != want[role] {
			t.Errorf("a %s's seat: %v, want %v", role, got, want[role])
		}
		if got := levels(`SELECT perm_agent_delegate::text, perm_conversation_ask::text, perm_conversation_answer::text
			FROM permission_preset WHERE dept_id IS NOT NULL AND name = $1`, role); got != want[role] {
			t.Errorf("a department's %s preset: %v, want %v", role, got, want[role])
		}
	}
	if got := levels(`SELECT perm_agent_delegate::text, perm_conversation_ask::text, perm_conversation_answer::text
		FROM course_member WHERE id = $1`, seatID(20)); got != want["observer"] {
		t.Errorf("a removed instructor's seat was given %v", got)
	}

	// Down, over a delegate with a proposal waiting.
	student := seatID(10)
	exec(`INSERT INTO actor (id, kind, display_name, owner_actor_id) VALUES ($1, 'agent', 'bot', $2)`, seatID(30), student)
	exec(`INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope, principal_member_id)
		VALUES ($1, '00000000-0000-0000-0000-000000000004', $1, 'assistant', $2, 'listed', 'all', $2)`, seatID(30), student)
	exec(`INSERT INTO action (id, actor_id, course_id, member_id, action_type, target_type, payload_hash, idempotency_key, authz_result, status)
		VALUES ($1, $2, '00000000-0000-0000-0000-000000000004', $2, 'document.create', 'document', repeat('0', 64), 'k', 'confirm_required', 'proposed')`,
		seatID(40), seatID(30))
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down from 0007 with a delegate seated: %v", err)
	}
	var status, result string
	if err := pool.QueryRow(ctx, `SELECT status, result->'error'->'details'->>'reason' FROM action WHERE id = $1`, seatID(40)).Scan(&status, &result); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" || result != "member_removed" {
		t.Fatalf("the delegate's proposal after going down: %s, %s", status, result)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM course_member WHERE id = $1`, seatID(30)).Scan(&status); err != nil || status != "removed" {
		t.Fatalf("the delegate's seat after going down: %s %v", status, err)
	}
	if err := m.Up(); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

func seatID(n int) string { return fmt.Sprintf("00000000-0000-0000-0000-%012d", n) }

// Migration 0013 gives member_invite, which makes join links, to every seat
// a person holds, not removed, at its level of member_manage, and denies it
// to every seat an agent holds, whatever it manages; a preset gets its
// member_manage level unless it is for students or agents (role student or
// assistant), which get none. Going down takes the column off both tables
// and leaves the seats as they were.
func TestMemberInviteStartsWhereMemberManageIsForPeopleOnly(t *testing.T) {
	pool, url := testdb.NewEmpty(t)
	ctx := context.Background()
	m, err := db.NewMigrator(url)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Steps(12); err != nil {
		t.Fatalf("up to 0012: %v", err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO actor (id, kind, display_name, platform_role) VALUES ('00000000-0000-0000-0000-000000000001', 'human', 'root', 'root')`)
	exec(`INSERT INTO term (id, name, starts_on, ends_on) VALUES ('00000000-0000-0000-0000-000000000002', 'T', '2026-09-01', '2026-12-20')`)
	exec(`INSERT INTO department (id, name) VALUES ('00000000-0000-0000-0000-000000000003', 'D')`)
	exec(`INSERT INTO course (id, dept_id, term_id, code, title, created_by_actor_id) VALUES
		('00000000-0000-0000-0000-000000000004', '00000000-0000-0000-0000-000000000003', '00000000-0000-0000-0000-000000000002', 'C', 'C',
		 '00000000-0000-0000-0000-000000000001')`)
	type seat struct {
		kind, role, status, manage, want string
		owned                            bool
	}
	seats := []seat{
		{"human", "instructor", "active", "autonomous", "autonomous", false},
		{"human", "ta", "active", "confirm_required", "confirm_required", false},
		{"human", "ta", "paused", "pending_review", "pending_review", false},
		{"human", "student", "active", "denied", "denied", false},
		{"human", "assistant", "active", "autonomous", "autonomous", false}, // a person is a person, whatever their role
		{"agent", "instructor", "active", "autonomous", "denied", false},    // an enrolment bot nobody owns
		{"agent", "assistant", "active", "autonomous", "denied", true},      // a delegate, whatever its row says
		{"human", "instructor", "removed", "autonomous", "denied", false},   // history
	}
	for i, s := range seats {
		id := seatID(10 + i)
		exec(`INSERT INTO actor (id, kind, display_name) VALUES ($1, $2, $3)`, id, s.kind, s.role)
		var principal *string
		if s.owned {
			// Owned by the instructor, seated as their delegate.
			exec(`UPDATE actor SET owner_actor_id = $2 WHERE id = $1`, id, seatID(10))
			p := seatID(10)
			principal = &p
		}
		exec(`INSERT INTO course_member (id, course_id, actor_id, role, status, added_by_actor_id, student_scope, assignment_scope,
				perm_member_manage, principal_member_id)
			VALUES ($1, '00000000-0000-0000-0000-000000000004', $1, $2, $3, '00000000-0000-0000-0000-000000000001', 'all', 'all', $4, $5)`,
			id, s.role, s.status, s.manage, principal)
	}
	presets := map[string][2]string{ // name: role, member_manage
		"student": {"student", "denied"}, "observer": {"observer", "denied"}, "ta": {"ta", "denied"},
		"instructor": {"instructor", "autonomous"}, "tutor": {"assistant", "denied"}, "grader": {"assistant", "denied"},
		"delegate": {"assistant", "denied"}, "course_tutor": {"assistant", "denied"},
	}
	for name, p := range presets {
		exec(`INSERT INTO permission_preset (name, role, student_scope, assignment_scope, perm_member_manage) VALUES ($1, $2, 'all', 'all', $3)`,
			name, p[0], p[1])
	}
	deptPresets := map[string][3]string{ // name: role, member_manage, member_invite wanted
		"head_ta":        {"ta", "autonomous", "autonomous"},
		"lab_lead":       {"observer", "pending_review", "pending_review"},
		"co_instructor":  {"instructor", "confirm_required", "confirm_required"},
		"class_rep":      {"student", "autonomous", "denied"},
		"enrolment_bot":  {"assistant", "autonomous", "denied"},
		"plain_student":  {"student", "denied", "denied"},
		"plain_observer": {"observer", "denied", "denied"},
	}
	for name, p := range deptPresets {
		exec(`INSERT INTO permission_preset (dept_id, name, role, student_scope, assignment_scope, perm_member_manage)
			VALUES ('00000000-0000-0000-0000-000000000003', $1, $2, 'all', 'all', $3)`, name, p[0], p[1])
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("up to 0013: %v", err)
	}

	invite := func(sql string, args ...any) string {
		t.Helper()
		var l string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&l); err != nil {
			t.Fatal(err)
		}
		return l
	}
	for i, s := range seats {
		if got := invite(`SELECT perm_member_invite::text FROM course_member WHERE id = $1`, seatID(10+i)); got != s.want {
			t.Errorf("a %s %s's %s seat managing members at %s: member_invite %s, want %s", s.kind, s.role, s.status, s.manage, got, s.want)
		}
	}
	for name := range presets {
		want := "denied"
		if name == "instructor" {
			want = "autonomous"
		}
		if got := invite(`SELECT perm_member_invite::text FROM permission_preset WHERE dept_id IS NULL AND name = $1`, name); got != want {
			t.Errorf("the built-in %s: member_invite %s, want %s", name, got, want)
		}
	}
	for name, p := range deptPresets {
		if got := invite(`SELECT perm_member_invite::text FROM permission_preset WHERE dept_id IS NOT NULL AND name = $1`, name); got != p[2] {
			t.Errorf("a department's %s (%s, member_manage %s): member_invite %s, want %s", name, p[0], p[1], got, p[2])
		}
	}

	if err := m.Steps(-1); err != nil {
		t.Fatalf("down from 0013: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND column_name = 'perm_member_invite'`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("down from 0013 left %d columns (%v)", left, err)
	}
	if got := invite(`SELECT perm_member_manage::text FROM course_member WHERE id = $1`, seatID(10)); got != "autonomous" {
		t.Fatalf("down from 0013 changed a seat: member_manage %s", got)
	}
	if err := m.Up(); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// Migration 0025 makes a runtime agent of each agent whose site chat
// credential is live, taking that credential as the runtime's token and
// revoking the agent's others, and an mcp agent of every other, its tokens
// as they were. The release before keeps working: an agent it registers is
// an mcp agent, and a token it would issue a runtime agent's owner is
// refused. Going down and up again comes back to the same.
func TestAgentHostingBackfillsAndKeepsThePreviousReleaseWorking(t *testing.T) {
	pool, url := testdb.NewEmpty(t)
	ctx := context.Background()
	m, err := db.NewMigrator(url)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	// Up to the migration before it, as many steps as there are before it
	// in order: golang-migrate counts steps, not versions, which may skip.
	entries, err := fs.ReadDir(dbfiles.FS, dbfiles.MigrationsDir)
	if err != nil {
		t.Fatal(err)
	}
	before, found := 0, false
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "_agent_hosting.up.sql") {
			found = true
			break
		}
		if strings.HasSuffix(e.Name(), ".up.sql") {
			before++
		}
	}
	if !found {
		t.Fatal("no agent hosting migration")
	}
	if err := m.Steps(before); err != nil {
		t.Fatalf("up to the one before 0025: %v", err)
	}
	exec := func(sql string, args ...any) error {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		return err
	}
	must := func(sql string, args ...any) {
		t.Helper()
		if err := exec(sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	owner := seatID(1)
	must(`INSERT INTO actor (id, kind, display_name) VALUES ($1, 'human', 'Mei')`, owner)
	// 2 hosted by a runtime, declared with a live token (12), beside her
	// laptop's (13) · 3 declared with a revoked token (14), and an editor's
	// (15) · 4 never declared (16)
	for _, a := range []int{2, 3, 4} {
		must(`INSERT INTO actor (id, kind, display_name, owner_actor_id) VALUES ($1, 'agent', $2, $3)`, seatID(a), fmt.Sprint("agent ", a), owner)
	}
	token := func(id, agent int, revoked bool) {
		t.Helper()
		must(`INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, revoked_at)
			VALUES ($1, $2, 'api_token', 'h', $3, CASE WHEN $4 THEN now() END)`, seatID(id), seatID(agent), fmt.Sprint("pfx", id), revoked)
	}
	token(12, 2, false)
	token(13, 2, false)
	token(14, 3, true)
	token(15, 3, false)
	token(16, 4, false)
	must(`UPDATE actor SET site_chat_credential_id = $1 WHERE id = $2`, seatID(12), seatID(2))
	must(`UPDATE actor SET site_chat_credential_id = $1 WHERE id = $2`, seatID(14), seatID(3))
	if err := m.Steps(1); err != nil {
		t.Fatalf("up to 0025: %v", err)
	}

	state := func() string {
		t.Helper()
		var got string
		if err := pool.QueryRow(ctx, `SELECT string_agg(x, ' ' ORDER BY x) FROM (
			SELECT right(id::text, 1) || '=' || hosting AS x FROM actor WHERE kind = 'agent'
			UNION ALL
			SELECT right(id::text, 2) || ':' || coalesce(issued_to_service, '-') || ':' || CASE WHEN revoked_at IS NULL THEN 'live' ELSE 'revoked' END
			FROM credential) s`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	want := "12:agent_runtime:live 13:-:revoked 14:-:revoked 15:-:live 16:-:live 2=runtime 3=mcp 4=mcp"
	if got := state(); got != want {
		t.Fatalf("after 0025:\n got %s\nwant %s", got, want)
	}

	// The release before registers an agent naming no hosting: an mcp
	// agent, whose owner issues it tokens.
	must(`INSERT INTO actor (id, kind, display_name, owner_actor_id) VALUES ($1, 'agent', 'new', $2)`, seatID(5), owner)
	must(`INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix) VALUES ($1, $2, 'api_token', 'h', 'pfx17')`, seatID(17), seatID(5))
	// And is refused a token for a runtime agent's owner, a hosting
	// changed, or a second token for the runtime.
	for what, sql := range map[string]string{
		"an owner's token for a runtime agent": `INSERT INTO credential (actor_id, kind, secret_hash, token_prefix) VALUES ('` + seatID(2) + `', 'api_token', 'h', 'pfx18')`,
		"a hosting changed":                    `UPDATE actor SET hosting = 'runtime' WHERE id = '` + seatID(4) + `'`,
		"a second runtime token": `INSERT INTO credential (actor_id, kind, secret_hash, token_prefix, issued_to_service)
			VALUES ('` + seatID(2) + `', 'api_token', 'h', 'pfx19', 'agent_runtime')`,
	} {
		if err := exec(sql); err == nil {
			t.Errorf("%s was taken", what)
		}
	}

	if err := m.Steps(-1); err != nil {
		t.Fatalf("down from 0025: %v", err)
	}
	var declared string
	if err := pool.QueryRow(ctx, `SELECT site_chat_credential_id::text FROM actor WHERE id = $1`, seatID(2)).Scan(&declared); err != nil ||
		declared != seatID(12) {
		t.Fatalf("after going down, the runtime agent's site chat credential: %s %v", declared, err)
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("up again: %v", err)
	}
	// The same, and the agent the release before registered, an mcp agent
	// still, with its token.
	want = "12:agent_runtime:live 13:-:revoked 14:-:revoked 15:-:live 16:-:live 17:-:live 2=runtime 3=mcp 4=mcp 5=mcp"
	if got := state(); got != want {
		t.Fatalf("up again:\n got %s\nwant %s", got, want)
	}
}

// Migration 0031's down refuses while any submission is a group's: the
// schema before cannot hold a group's work, and a down migration does not
// delete students' work. It changes nothing, and leaves the version marked
// dirty one below, which `migrate force` puts back; once the group
// assignment is deleted for good, it goes down, and up again.
func TestGroupWorkKeepsMigration0031FromGoingDown(t *testing.T) {
	pool, url := testdb.NewEmpty(t)
	ctx := context.Background()
	m, err := db.NewMigrator(url)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}
	latest, err := db.LatestEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	if latest != 31 {
		// Only the newest migration's down is run here.
		t.Skipf("the newest migration is %d, not 0031", latest)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO actor (id, kind, display_name) VALUES ($1, 'human', 'Lin'), ($2, 'human', 'Wei')`, seatID(1), seatID(2))
	exec(`INSERT INTO term (id, name, starts_on, ends_on) VALUES ($1, 'T', '2026-09-01', '2026-12-20')`, seatID(3))
	exec(`INSERT INTO department (id, name) VALUES ($1, 'D')`, seatID(4))
	exec(`INSERT INTO course (id, dept_id, term_id, code, title, created_by_actor_id) VALUES ($1, $2, $3, 'C', 'C', $4)`,
		seatID(5), seatID(4), seatID(3), seatID(1))
	exec(`INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope) VALUES
		($1, $3, $4, 'instructor', $4, 'all', 'all'), ($2, $3, $5, 'student', $4, 'listed', 'all')`,
		seatID(11), seatID(12), seatID(5), seatID(1), seatID(2))
	exec(`INSERT INTO action (id, actor_id, course_id, member_id, action_type, target_type, payload_hash, idempotency_key, authz_result, status,
			executed_at)
		VALUES ($1, $2, $3, $4, 'group.set_members', 'group_set', repeat('0', 64), 'k', 'autonomous', 'executed', now()),
		       ($5, $2, $3, $4, 'assignment.delete', 'assignment', repeat('0', 64), 'k2', 'autonomous', 'executed', now())`,
		seatID(21), seatID(1), seatID(5), seatID(11), seatID(22))
	exec(`INSERT INTO group_set (id, course_id, name, created_by_member_id) VALUES ($1, $2, 'Projects', $3)`, seatID(31), seatID(5), seatID(11))
	exec(`INSERT INTO course_group (id, course_id, set_id, name, created_by_member_id) VALUES ($1, $2, $3, 'Team', $4)`,
		seatID(32), seatID(5), seatID(31), seatID(11))
	exec(`INSERT INTO group_membership (course_id, set_id, group_id, member_id, joined_at, joined_by_member_id, joined_how, joined_action_id)
		VALUES ($1, $2, $3, $4, now(), $5, 'assigned', $6)`, seatID(5), seatID(31), seatID(32), seatID(12), seatID(11), seatID(21))
	exec(`INSERT INTO assignment (id, course_id, title, points_possible, published_at, group_set_id) VALUES ($1, $2, 'Project', 10, now(), $3)`,
		seatID(41), seatID(5), seatID(31))
	exec(`INSERT INTO submission (id, assignment_id, course_id, group_id, body) VALUES ($1, $2, $3, $4, 'Our draft')`,
		seatID(51), seatID(41), seatID(5), seatID(32))

	err = m.Steps(-1)
	if err == nil || !strings.Contains(err.Error(), "group work exists") {
		t.Fatalf("down over a group's work: %v", err)
	}
	var tables int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'
		AND table_name IN ('group_set', 'submission_member')`).Scan(&tables); err != nil || tables != 2 {
		t.Fatalf("the refused down changed the schema: %d %v", tables, err)
	}
	// Its connection is left in the failed transaction: a new one reads the
	// version, as `migrate version` would.
	m.Close()
	if m, err = db.NewMigrator(url); err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if version, dirty, err := m.Version(); err != nil || !dirty || version != 30 {
		t.Fatalf("after the refused down: version %d, dirty %v, %v", version, dirty, err)
	}
	if err := m.Force(31); err != nil {
		t.Fatal(err)
	}
	// The group assignment deleted for good, as assignment.delete deletes it.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`INSERT INTO assignment_deletion (assignment_id, course_id, title, was_published, action_id, deleted_by_actor_id,
			deleted_by_member_id, deleted_at, submissions, grades, files, proposals, totals)
		 VALUES ('` + seatID(41) + `', '` + seatID(5) + `', 'Project', true, '` + seatID(22) + `', '` + seatID(1) + `', '` + seatID(11) + `', now(), 1, 0, 0, 0, 0)`,
		`DELETE FROM submission WHERE assignment_id = '` + seatID(41) + `'`,
		`DELETE FROM assignment WHERE id = '` + seatID(41) + `'`,
	} {
		if _, err := tx.Exec(ctx, sql); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down once the group work is gone: %v", err)
	}
	if err := m.Up(); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

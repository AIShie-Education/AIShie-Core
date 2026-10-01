package db_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
)

// An administrator relies on the appointment nearest to what a call is
// about: their own at the department, or the first above it. An ended one is
// none, and a sibling's department is not theirs.
func TestDepartmentAuthorityIsTheNearestLiveAppointment(t *testing.T) {
	w := testkit.NewDeptTree(t)
	ctx := context.Background()
	authority := func(actor, dept uuid.UUID) uuid.UUID {
		t.Helper()
		r, err := w.Q.DepartmentAuthority(ctx, dbq.DepartmentAuthorityParams{ActorID: actor, DeptID: dept})
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil
		}
		if err != nil {
			t.Fatal(err)
		}
		locked, err := w.Q.LockDepartmentAuthority(ctx, dbq.LockDepartmentAuthorityParams{ActorID: actor, DeptID: dept})
		if err != nil || dbq.DepartmentAuthorityRow(locked) != r {
			t.Fatalf("locked, the authority is %+v (%v), unlocked %+v", locked, err, r)
		}
		return r.AppointmentID
	}

	for _, c := range []struct {
		name        string
		actor, dept uuid.UUID
		want        uuid.UUID
	}{
		{"Ada at her own department", w.Ada, w.F, w.AdaF},
		{"Ada two levels beneath it", w.Ada, w.D, w.AdaF},
		{"Ada not above it", w.Ada, w.U, uuid.Nil},
		{"Bob beneath his", w.Bob, w.D, w.BobS},
		{"Bob not at a sibling of his", w.Bob, w.S2, uuid.Nil},
		{"Bob not above his", w.Bob, w.F, uuid.Nil},
		{"Carol not in another faculty", w.Carol, w.D, uuid.Nil},
		{"Dan nowhere", w.Dan, w.D, uuid.Nil},
	} {
		if got := authority(c.actor, c.dept); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}

	adaS := w.Appoint(w.S, w.Ada, w.Root)
	if got := authority(w.Ada, w.D); got != adaS {
		t.Fatalf("with an appointment nearer than Engineering's, Ada relies on %v, want %v", got, adaS)
	}
	w.Exec(`UPDATE department_admin SET removed_at = now(), removed_by_actor_id = $2 WHERE id = $1`, adaS, w.Root)
	if got := authority(w.Ada, w.D); got != w.AdaF {
		t.Fatalf("once it ended, Ada relies on %v, want Engineering's", got)
	}
	w.Exec(`UPDATE department_admin SET removed_at = now(), removed_by_actor_id = $2 WHERE id = $1`, w.AdaF, w.Root)
	if got := authority(w.Ada, w.D); got != uuid.Nil {
		t.Fatalf("with every appointment ended, Ada relies on %v", got)
	}
}

// A write relies on its appointment to the end of its transaction: ending
// the appointment waits for it.
func TestLockDepartmentAuthorityHoldsTheAppointment(t *testing.T) {
	w := testkit.NewDeptTree(t)
	ctx := context.Background()
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := dbq.New(tx).LockDepartmentAuthority(ctx, dbq.LockDepartmentAuthorityParams{ActorID: w.Bob, DeptID: w.D}); err != nil {
		t.Fatal(err)
	}
	_, err = w.Pool.Exec(ctx, `SELECT 1 FROM department_admin WHERE id = $1 FOR UPDATE NOWAIT`, w.BobS)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("the appointment relied on is not held: %v", err)
	}
	// Only that one: the department rows are not locked, nor is Chan's
	// appointment at the same department.
	for _, sql := range []string{
		`SELECT 1 FROM department_admin WHERE id = $1 FOR UPDATE NOWAIT`,
		`SELECT 1 FROM department WHERE id = (SELECT dept_id FROM department_admin WHERE id = $1) FOR UPDATE NOWAIT`,
	} {
		if _, err := w.Pool.Exec(ctx, sql, w.ChanS); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}

// course.list's query: every course for a platform administrator; for a
// department administrator, those of the departments they administer and
// beneath them, and never another's.
func TestListCoursesIsWhatTheCallerAdministers(t *testing.T) {
	w := testkit.NewDeptTree(t)
	ctx := context.Background()
	names := map[uuid.UUID]string{w.CD: "CD", w.CArch: "CArch", w.CS: "CS", w.CS2: "CS2", w.CS3: "CS3"}
	list := func(actor uuid.UUID, platform bool, within *uuid.UUID) []string {
		t.Helper()
		rows, err := w.Q.ListCourses(ctx, dbq.ListCoursesParams{ActorID: actor, Platform: platform, WithinDeptID: within, MaxRows: 50})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, r := range rows {
			got = append(got, names[r.ID])
		}
		slices.Sort(got)
		return got
	}
	for _, c := range []struct {
		name     string
		actor    uuid.UUID
		platform bool
		within   *uuid.UUID
		want     []string
	}{
		{"Ada, at Engineering", w.Ada, false, nil, []string{"CArch", "CD", "CS", "CS2"}},
		{"Ada, within Computing", w.Ada, false, &w.S, []string{"CArch", "CD", "CS"}},
		{"Ada, within History", w.Ada, false, &w.S3, nil},
		{"Bob, at Computing", w.Bob, false, nil, []string{"CArch", "CD", "CS"}},
		{"Carol, at Humanities", w.Carol, false, nil, []string{"CS3"}},
		{"Dan, nowhere", w.Dan, false, nil, nil},
		{"a platform administrator", w.Admin, true, nil, []string{"CArch", "CD", "CS", "CS2", "CS3"}},
		{"a platform administrator, within Humanities", w.Admin, true, &w.F2, []string{"CS3"}},
	} {
		if got := list(c.actor, c.platform, c.within); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	w.Exec(`UPDATE department_admin SET removed_at = now(), removed_by_actor_id = $2 WHERE id = $1`, w.BobS, w.Root)
	if got := list(w.Bob, false, nil); got != nil {
		t.Fatalf("Bob, his appointment ended, still lists %v", got)
	}
}

// What an administrator covers, and where they may reshape and staff the
// tree: strictly beneath an appointment of theirs.
func TestAdministeredDepartments(t *testing.T) {
	w := testkit.NewDeptTree(t)
	type flags struct{ strictly, appointed bool }
	covered := func(actor uuid.UUID) map[uuid.UUID]flags {
		t.Helper()
		rows, err := w.Q.AdministeredDepartments(context.Background(), actor)
		if err != nil {
			t.Fatal(err)
		}
		out := map[uuid.UUID]flags{}
		for _, r := range rows {
			out[r.ID] = flags{r.Strictly, r.Appointed}
		}
		return out
	}
	if got, want := covered(w.Bob), map[uuid.UUID]flags{w.S: {false, true}, w.D: {true, false}}; !mapsEqual(got, want) {
		t.Errorf("Bob: %v, want Computing appointed and AI strictly", got)
	}
	want := map[uuid.UUID]flags{w.F: {false, true}, w.S: {true, false}, w.D: {true, false}, w.S2: {true, false}}
	if got := covered(w.Ada); !mapsEqual(got, want) {
		t.Errorf("Ada: %v, want Engineering appointed and all beneath it strictly", got)
	}
	// Appointed at Computing too, she is appointed there and still above it.
	w.Appoint(w.S, w.Ada, w.Root)
	want[w.S] = flags{true, true}
	if got := covered(w.Ada); !mapsEqual(got, want) {
		t.Errorf("Ada, appointed at Computing too: %v", got)
	}
	if got := covered(w.Dan); len(got) != 0 {
		t.Errorf("Dan covers %v", got)
	}
}

func mapsEqual[K comparable, V comparable](a, b map[K]V) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// The small walks the tree tools make before they change it.
func TestTreeWalks(t *testing.T) {
	w := testkit.NewDeptTree(t)
	ctx := context.Background()
	for dept, want := range map[uuid.UUID]int32{w.U: 1, w.F: 2, w.S: 3, w.D: 4, w.S3: 3} {
		if got, err := w.Q.DepartmentDepth(ctx, dept); err != nil || got != want {
			t.Errorf("depth of %v: %d (%v), want %d", dept, got, err, want)
		}
	}
	for dept, want := range map[uuid.UUID]int32{w.U: 4, w.F: 3, w.S: 2, w.D: 1, uuid.New(): 0} {
		if got, err := w.Q.SubtreeHeight(ctx, dept); err != nil || got != want {
			t.Errorf("height of %v: %d (%v), want %d", dept, got, err, want)
		}
	}
	for _, c := range []struct {
		dept, other uuid.UUID
		want        bool
	}{{w.F, w.D, true}, {w.S, w.S, true}, {w.S, w.S2, false}, {w.D, w.F, false}} {
		if got, err := w.Q.InSubtree(ctx, dbq.InSubtreeParams{DeptID: c.dept, OtherID: c.other}); err != nil || got != c.want {
			t.Errorf("%v in the subtree of %v: %v (%v)", c.other, c.dept, got, err)
		}
	}
	for _, c := range []struct {
		parent *uuid.UUID
		name   string
		id     uuid.UUID
		want   bool
	}{
		{&w.F, "DESIGN", uuid.Nil, true},   // in any case
		{&w.F, "Design", w.S2, false},      // its own name
		{&w.F2, "Design", uuid.Nil, false}, // another parent's child
		{nil, "university", uuid.Nil, true},
		{nil, "Engineering", uuid.Nil, false}, // not at the top
	} {
		got, err := w.Q.SiblingNameTaken(ctx, dbq.SiblingNameTakenParams{ParentID: c.parent, Name: c.name, ID: c.id})
		if err != nil || got != c.want {
			t.Errorf("%q under %v taken: %v (%v), want %v", c.name, c.parent, got, err, c.want)
		}
	}

	// The tree: each department before those beneath it, siblings by name.
	rows, err := w.Q.DepartmentTree(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var order []uuid.UUID
	for _, r := range rows {
		order = append(order, r.ID)
	}
	if want := []uuid.UUID{w.U, w.F, w.S, w.D, w.S2, w.F2, w.S3}; !slices.Equal(order, want) {
		t.Fatalf("the tree in order: %v, want %v", order, want)
	}
	sub, err := w.Q.DepartmentTree(ctx, &w.S)
	if err != nil || len(sub) != 2 || sub[0].ID != w.S || sub[0].Depth != 3 || sub[1].ID != w.D || sub[1].Depth != 4 {
		t.Fatalf("Computing's subtree: %+v (%v)", sub, err)
	}
}

// What deciding whether a department administrator may invite a person
// needs to know, and the lookup that finds the person.
func TestInvitableByAndLookupByEmail(t *testing.T) {
	w := testkit.NewDeptTree(t)
	ctx := context.Background()
	byEmail := func(email string) (dbq.LookupActorBySignInNameRow, error) {
		return w.Q.LookupActorBySignInName(ctx, dbq.LookupActorBySignInNameParams{Email: &email})
	}
	found, err := byEmail("KEN@Example.EDU")
	if err != nil || found.ID != w.Ken || found.Kind != "human" || found.HasPassword || found.HasSso || found.InviteExpiresAt != nil {
		t.Fatalf("Ken by his email in another case: %+v (%v)", found, err)
	}
	if _, err := byEmail("ken@example"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a part of an email found someone: %v", err)
	}
	// And by his login ID, whole and in any case, once he has one.
	if _, err := w.Pool.Exec(ctx, `UPDATE actor SET login_id = 'UNI2023007' WHERE id = $1`, w.Ken); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"uni2023007", "UNI2023007"} {
		if got, err := w.Q.LookupActorBySignInName(ctx, dbq.LookupActorBySignInNameParams{LoginID: &id}); err != nil || got.ID != w.Ken {
			t.Fatalf("Ken by his login ID %q: %+v (%v)", id, got, err)
		}
	}
	for _, part := range []string{"UNI", "2023007", "ken@example.edu"} {
		if _, err := w.Q.LookupActorBySignInName(ctx, dbq.LookupActorBySignInNameParams{LoginID: &part}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("%q, not his whole login ID, found someone: %v", part, err)
		}
	}

	of := func(issuer, actor uuid.UUID) dbq.InvitableByRow {
		t.Helper()
		r, err := w.Q.InvitableBy(ctx, dbq.InvitableByParams{IssuerID: issuer, ActorID: actor})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// Ken is a student in CS and CS2, both Engineering's, and CD, AI's.
	if r := of(w.Ada, w.Ken); !r.IsPerson || r.CanSignIn || r.HoldsRole || r.Administers || r.OwnsAgents || r.SeatsOutside != 0 || r.IssuerPlatform {
		t.Errorf("Ken, for Ada: %+v", r)
	}
	// Bob's reach stops at Computing: Ken's seat in CS2 is outside it.
	if r := of(w.Bob, w.Ken); r.SeatsOutside != 1 {
		t.Errorf("Ken, for Bob: %+v, want one seat outside", r)
	}
	if r := of(w.Carol, w.Ken); r.SeatsOutside != 3 {
		t.Errorf("Ken, for Carol: %+v, want every seat outside", r)
	}
	if r := of(w.Carol, w.Ada); !r.Administers || !r.OwnsAgents {
		t.Errorf("Ada, for Carol: %+v", r)
	}
	if r := of(w.Admin, w.Robo); r.IsPerson || !r.IssuerPlatform {
		t.Errorf("Robo, for a platform administrator: %+v", r)
	}
	w.Exec(`INSERT INTO credential (actor_id, kind, secret_hash) VALUES ($1, 'password', 'h')`, w.Ken)
	if r := of(w.Ada, w.Ken); !r.CanSignIn {
		t.Errorf("Ken, with a password: %+v", r)
	}
	if r := of(w.Ada, w.Admin); !r.HoldsRole {
		t.Errorf("a platform administrator, for Ada: %+v", r)
	}
}

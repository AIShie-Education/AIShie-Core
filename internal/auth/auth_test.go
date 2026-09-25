package auth_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/auth"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testdb"
)

var tokenShape = regexp.MustCompile(`^ais_[a-z2-7]{12}_[A-Za-z0-9_-]{43}$`)

func TestTokenShape(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		tok, err := auth.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if !tokenShape.MatchString(tok.Full) {
			t.Fatalf("token %q does not look like ais_<prefix>_<secret>", tok.Full)
		}
		if !strings.Contains(tok.Full, "_"+tok.Prefix+"_") || !strings.HasPrefix(tok.Hash, "sha256:") || strings.Contains(tok.Hash, tok.Full) {
			t.Fatalf("prefix %q / hash %q do not belong to %q", tok.Prefix, tok.Hash, tok.Full)
		}
		if seen[tok.Prefix] {
			t.Fatal("a prefix repeated")
		}
		seen[tok.Prefix] = true
	}
}

func TestPasswordHashing(t *testing.T) {
	phc, err := auth.HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=65536,t=3,p=4$") || strings.Contains(phc, "correct horse") {
		t.Fatalf("stored form: %s", phc)
	}
	again, _ := auth.HashPassword("correct horse battery")
	if again == phc {
		t.Fatal("two hashes of one password are identical: the salt is not random")
	}
	for pw, want := range map[string]bool{"correct horse battery": true, "correct horse batterz": false, "": false} {
		if got, err := auth.VerifyPassword(pw, phc); err != nil || got != want {
			t.Errorf("VerifyPassword(%q) = %v, %v; want %v", pw, got, err, want)
		}
	}
	for _, weak := range []string{"short", strings.Repeat("x", auth.MaxPasswordLen+1)} {
		if _, err := auth.HashPassword(weak); !errors.Is(err, auth.ErrWeakPassword) {
			t.Errorf("HashPassword(%d chars): %v", len(weak), err)
		}
	}
	// Parameters argon2 would panic on, or that would exhaust the server,
	// are an error, not a crash: a stored hash is trusted only so far.
	salted := "$" + strings.Repeat("A", 22) + "$" + strings.Repeat("A", 43)
	for _, bad := range []string{"", "plaintext", "$bcrypt$x$y$z$w", "$argon2id$v=19$m=999999999,t=3,p=4$AAAA$AAAA",
		"$argon2id$v=19$m=65536,t=0,p=4" + salted, "$argon2id$v=19$m=65536,t=3,p=0" + salted,
		"$argon2id$v=19$m=8,t=3,p=4" + salted, "$argon2id$v=19$m=65536,t=3,p=255" + salted} {
		if ok, err := auth.VerifyPassword("x", bad); ok || err == nil {
			t.Errorf("VerifyPassword against %q = %v, %v; want an error", bad, ok, err)
		}
	}
}

func TestBootstrapAndAuthenticate(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	q := dbq.New(pool)

	res, err := auth.Bootstrap(ctx, pool, auth.BootstrapInput{DisplayName: "Root", Email: "root@example.edu", Password: "a long enough password"})
	if err != nil {
		t.Fatal(err)
	}
	root, err := q.GetActor(ctx, res.RootID)
	if err != nil || root.PlatformRole == nil || *root.PlatformRole != "root" || root.CreatedByActorID != nil {
		t.Fatalf("root actor: %+v %v", root, err)
	}
	system, err := q.GetActor(ctx, res.SystemID)
	if err != nil || system.Kind != "system" || system.CreatedByActorID == nil || *system.CreatedByActorID != res.RootID {
		t.Fatalf("system actor: %+v %v", system, err)
	}
	if _, err := auth.Bootstrap(ctx, pool, auth.BootstrapInput{DisplayName: "Usurper"}); !errors.Is(err, auth.ErrAlreadyBootstrapped) {
		t.Fatalf("second bootstrap: %v", err)
	}

	a := auth.NewAuthenticator(pool, time.Hour)
	p, err := a.Authenticate(ctx, res.Token.Full)
	if err != nil || p.ActorID != res.RootID || p.Kind != auth.KindAPIToken {
		t.Fatalf("authenticate with the bootstrap token: %+v %v", p, err)
	}
	// Nothing that is stored is enough to authenticate with.
	var stored string
	if err := pool.QueryRow(ctx, `SELECT secret_hash FROM credential WHERE id = $1`, p.CredentialID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == res.Token.Full || strings.Contains(stored, res.Token.Full[20:]) {
		t.Fatal("the token is stored in the clear")
	}

	tampered := res.Token.Full[:len(res.Token.Full)-1] + "A"
	if tampered == res.Token.Full {
		tampered = res.Token.Full[:len(res.Token.Full)-1] + "B"
	}
	for name, presented := range map[string]string{
		"nothing": "", "not ours": "ghp_abcdefghijklmnop", "right prefix, wrong secret": tampered,
		"unknown prefix": "ais_aaaaaaaaaaaa_" + strings.Repeat("A", 43), "the hash itself": stored,
		"a prefix that is not UTF-8": "ais_\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff_" + strings.Repeat("A", 43),
	} {
		if _, err := a.Authenticate(ctx, presented); !apperr.Is(err, apperr.Unauthenticated) {
			t.Errorf("%s: err = %v, want unauthenticated", name, err)
		}
	}

	// Revocation and expiry apply on the very next use.
	if _, err := pool.Exec(ctx, `UPDATE credential SET expires_at = now() - interval '1 second' WHERE id = $1`, p.CredentialID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(ctx, res.Token.Full); !apperr.Is(err, apperr.Unauthenticated) {
		t.Fatalf("expired token: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE credential SET expires_at = NULL, revoked_at = now() WHERE id = $1`, p.CredentialID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(ctx, res.Token.Full); !apperr.Is(err, apperr.Unauthenticated) {
		t.Fatalf("revoked token: %v", err)
	}
}

// A credential notes when it was last used, for its holder's list of tokens,
// and writes that at most once a minute however busy it is.
func TestAUseIsNotedAtMostOnceAMinute(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	res, err := auth.Bootstrap(ctx, pool, auth.BootstrapInput{DisplayName: "Root"})
	if err != nil {
		t.Fatal(err)
	}
	a := auth.NewAuthenticator(pool, time.Hour)
	t0 := time.Now().Truncate(time.Second)
	for _, step := range []struct {
		at, noted time.Time
	}{
		{t0, t0},
		{t0.Add(30 * time.Second), t0},
		{t0.Add(61 * time.Second), t0.Add(61 * time.Second)},
	} {
		a.SetClock(func() time.Time { return step.at })
		p, err := a.Authenticate(ctx, res.Token.Full)
		if err != nil {
			t.Fatal(err)
		}
		var noted *time.Time
		if err := pool.QueryRow(ctx, `SELECT last_used_at FROM credential WHERE id = $1`, p.CredentialID).Scan(&noted); err != nil {
			t.Fatal(err)
		}
		if noted == nil || !noted.Equal(step.noted) {
			t.Fatalf("used at %s: last_used_at = %v, want %s", step.at, noted, step.noted)
		}
	}
}

func TestLogin(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	res, err := auth.Bootstrap(ctx, pool, auth.BootstrapInput{DisplayName: "Root", Email: "Root@Example.edu", Password: "a long enough password"})
	if err != nil {
		t.Fatal(err)
	}
	a := auth.NewAuthenticator(pool, 2*time.Hour)

	sess, err := a.Login(ctx, "root@example.EDU", "a long enough password") // email is case-insensitive
	if err != nil || sess.ActorID != res.RootID {
		t.Fatalf("login: %+v %v", sess, err)
	}
	if d := time.Until(sess.ExpiresAt); d < 119*time.Minute || d > 121*time.Minute {
		t.Fatalf("session lasts %s, want 2h", d)
	}
	p, err := a.Authenticate(ctx, sess.Token)
	if err != nil || p.Kind != auth.KindSession || p.ActorID != res.RootID {
		t.Fatalf("authenticate with the session: %+v %v", p, err)
	}
	if err := a.Logout(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(ctx, sess.Token); !apperr.Is(err, apperr.Unauthenticated) {
		t.Fatalf("session after logout: %v", err)
	}

	// Wrong password and unknown email get the same answer.
	_, wrongPw := a.Login(ctx, "root@example.edu", "not the password!")
	_, noSuch := a.Login(ctx, "nobody@example.edu", "a long enough password")
	if !apperr.Is(wrongPw, apperr.Unauthenticated) || !apperr.Is(noSuch, apperr.Unauthenticated) || wrongPw.Error() != noSuch.Error() {
		t.Fatalf("wrong password: %v; unknown email: %v", wrongPw, noSuch)
	}
	// So does an email the database could not hold.
	for _, email := range []string{"root@example.edu\xff", "root@example.edu\x00"} {
		if _, err := a.Login(ctx, email, "a long enough password"); err == nil || err.Error() != noSuch.Error() {
			t.Errorf("%q: %v", email, err)
		}
	}
	// A suspended actor cannot start a session.
	if _, err := pool.Exec(ctx, `UPDATE actor SET status = 'suspended' WHERE id = $1`, res.RootID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Login(ctx, "root@example.edu", "a long enough password"); !apperr.Is(err, apperr.Unauthenticated) {
		t.Fatalf("suspended actor logging in: %v", err)
	}
}

func TestSetPasswordReplacesTheOldOne(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	res, err := auth.Bootstrap(ctx, pool, auth.BootstrapInput{DisplayName: "Root", Email: "root@example.edu", Password: "the first password"})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.SetPassword(ctx, dbq.New(pool), res.RootID, "the second password", time.Now()); err != nil {
		t.Fatal(err)
	}
	a := auth.NewAuthenticator(pool, time.Hour)
	if _, err := a.Login(ctx, "root@example.edu", "the first password"); !apperr.Is(err, apperr.Unauthenticated) {
		t.Fatalf("old password still works: %v", err)
	}
	if _, err := a.Login(ctx, "root@example.edu", "the second password"); err != nil {
		t.Fatalf("new password: %v", err)
	}
	if err := auth.SetPassword(ctx, dbq.New(pool), res.RootID, "short", time.Now()); !apperr.Is(err, apperr.InvalidArgument) {
		t.Fatalf("weak password: %v", err)
	}
}

// The secret is base64url, whose alphabet includes the separator. Every token
// must authenticate, not just the ones that happen to have no underscore.
func TestTokensWithUnderscoresInTheSecretAuthenticate(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	res, err := auth.Bootstrap(ctx, pool, auth.BootstrapInput{DisplayName: "Root"})
	if err != nil {
		t.Fatal(err)
	}
	a := auth.NewAuthenticator(pool, time.Hour)
	withUnderscore := 0
	for range 60 {
		tok, _, err := auth.IssueToken(ctx, dbq.New(pool), res.RootID, "t", nil, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(tok.Full, "_") > 2 {
			withUnderscore++
		}
		if _, err := a.Authenticate(ctx, tok.Full); err != nil {
			t.Fatalf("token %q does not authenticate: %v", tok.Full, err)
		}
	}
	if withUnderscore == 0 {
		t.Skip("no token in this run had an underscore in its secret; nothing was exercised")
	}
}

// The system actor is who the sweeps act as, and its authority is not to be
// borrowed: no token is issued for it, and the database takes no credential
// for it however one is written.
func TestTheSystemActorIsNeverIssuedAToken(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	res, err := auth.Bootstrap(ctx, pool, auth.BootstrapInput{DisplayName: "Root"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := auth.IssueToken(ctx, dbq.New(pool), res.SystemID, "sweeps", nil, time.Now()); !apperr.Is(err, apperr.Forbidden) {
		t.Fatalf("a token for the system actor: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM credential WHERE actor_id = $1`, res.SystemID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the system actor holds %d credentials", n)
	}

	tok, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO credential (actor_id, kind, secret_hash, token_prefix) VALUES ($1, 'api_token', $2, $3)`,
		res.SystemID, tok.Hash, tok.Prefix)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("the database took a token for the system actor: %v", err)
	}
}

// A token the system actor was given before the database refused them
// authenticates nobody.
func TestATokenOfTheSystemActorsAuthenticatesNobody(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	res, err := auth.Bootstrap(ctx, pool, auth.BootstrapInput{DisplayName: "Root"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Written as it was before migration 0004.
	exec(`ALTER TABLE credential DISABLE TRIGGER credential_not_for_system_actor`)
	exec(`INSERT INTO credential (actor_id, kind, secret_hash, token_prefix) VALUES ($1, 'api_token', $2, $3)`, res.SystemID, tok.Hash, tok.Prefix)
	exec(`ALTER TABLE credential ENABLE TRIGGER credential_not_for_system_actor`)
	if p, err := auth.NewAuthenticator(pool, time.Hour).Authenticate(ctx, tok.Full); !apperr.Is(err, apperr.Unauthenticated) {
		t.Fatalf("the system actor's token authenticated: %+v %v", p, err)
	}
}

// An invitation sets a person's password once, signs them in, and is good
// for nothing else.
func TestInvitations(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	q := dbq.New(pool)
	res, err := auth.Bootstrap(ctx, pool, auth.BootstrapInput{DisplayName: "Root", Email: "root@example.edu"})
	if err != nil {
		t.Fatal(err)
	}
	yuki := uuid.Must(uuid.NewV7())
	email := "Yuki@example.edu"
	if err := q.InsertActor(ctx, dbq.InsertActorParams{ID: yuki, Kind: "human", DisplayName: "Yuki", Email: &email,
		CreatedByActorID: &res.RootID, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	a := auth.NewAuthenticator(pool, time.Hour)
	invite := func(valid time.Duration) string {
		t.Helper()
		now := time.Now()
		tok, _, err := auth.IssueInvite(ctx, q, yuki, "test", now.Add(valid), now)
		if err != nil {
			t.Fatal(err)
		}
		return tok.Full
	}
	refused := func(what string, err error) {
		t.Helper()
		if !apperr.Is(err, apperr.Unauthenticated) {
			t.Fatalf("%s: %v, want unauthenticated", what, err)
		}
	}

	inv := invite(time.Hour)
	if !regexp.MustCompile(`^aisinv_[a-z2-7]{12}_[A-Za-z0-9_-]{43}$`).MatchString(inv) {
		t.Fatalf("invitation %q does not look like aisinv_<prefix>_<secret>", inv)
	}
	// It is no bearer token, and a bearer token is no invitation.
	_, err = a.Authenticate(ctx, inv)
	refused("the invitation as a bearer token", err)
	_, err = a.AcceptInvite(ctx, res.Token.Full, "a long enough password")
	refused("a token as an invitation", err)
	forged := []byte(inv)
	forged[30] ^= 'A' ^ 'B' // 'A' <-> 'B'; anything else becomes a character outside the alphabet
	_, err = a.AcceptInvite(ctx, string(forged), "a long enough password")
	refused("a forged invitation", err)
	// A weak password is refused, and the invitation still works.
	if _, err := a.AcceptInvite(ctx, inv, "short"); !apperr.Is(err, apperr.InvalidArgument) {
		t.Fatalf("weak password: %v", err)
	}
	acc, err := a.AcceptInvite(ctx, inv, "the first password")
	if err != nil || acc.ActorID != yuki || acc.Email != email {
		t.Fatalf("accept: %+v %v", acc, err)
	}
	if p, err := a.Authenticate(ctx, acc.Token); err != nil || p.ActorID != yuki || p.Kind != auth.KindSession {
		t.Fatalf("the session it started: %+v %v", p, err)
	}
	if _, err := a.Login(ctx, "yuki@example.edu", "the first password"); err != nil {
		t.Fatalf("signing in with the password it set: %v", err)
	}
	// Once.
	_, err = a.AcceptInvite(ctx, inv, "another long password")
	refused("an invitation used twice", err)
	if _, err := a.Login(ctx, "yuki@example.edu", "the first password"); err != nil {
		t.Fatalf("the password after a second try: %v", err)
	}

	// Only the newest works, and taken up, it replaces the password.
	older, newer := invite(time.Hour), invite(time.Hour)
	_, err = a.AcceptInvite(ctx, older, "another long password")
	refused("a replaced invitation", err)
	if _, err := a.AcceptInvite(ctx, newer, "the second password"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Login(ctx, "yuki@example.edu", "the first password"); !apperr.Is(err, apperr.Unauthenticated) {
		t.Fatalf("the old password after a second invitation: %v", err)
	}

	// Not once a password has been set some other way: it was for choosing
	// one.
	withdrawn := invite(time.Hour)
	if err := auth.SetPassword(ctx, q, yuki, "the third password", time.Now()); err != nil {
		t.Fatal(err)
	}
	_, err = a.AcceptInvite(ctx, withdrawn, "another long password")
	refused("an invitation after a password was set otherwise", err)

	// All or nothing: if the session cannot be started, the password is not
	// set and the invitation still works.
	whole := invite(time.Hour)
	for _, stmt := range []string{
		`CREATE FUNCTION no_sessions() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'no sessions today'; END $$`,
		`CREATE TRIGGER no_sessions BEFORE INSERT ON credential FOR EACH ROW WHEN (NEW.kind = 'session') EXECUTE FUNCTION no_sessions()`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.AcceptInvite(ctx, whole, "a fourth long password"); err == nil {
		t.Fatal("accepted with no session started")
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER no_sessions ON credential`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Login(ctx, "yuki@example.edu", "the third password"); err != nil {
		t.Fatalf("the password after a half-taken invitation: %v", err)
	}
	if _, err := a.AcceptInvite(ctx, whole, "a fourth long password"); err != nil {
		t.Fatalf("the invitation after a half-taken try: %v", err)
	}

	// Not once it has expired, nor for someone suspended since.
	late := invite(time.Hour)
	a.SetClock(func() time.Time { return time.Now().Add(2 * time.Hour) })
	_, err = a.AcceptInvite(ctx, late, "a third long password")
	refused("an expired invitation", err)
	a.SetClock(time.Now)
	suspended := invite(time.Hour)
	if _, err := pool.Exec(ctx, `UPDATE actor SET status = 'suspended' WHERE id = $1`, yuki); err != nil {
		t.Fatal(err)
	}
	_, err = a.AcceptInvite(ctx, suspended, "a third long password")
	refused("an invitation for someone suspended", err)
	if _, err := pool.Exec(ctx, `UPDATE actor SET status = 'active' WHERE id = $1`, yuki); err != nil {
		t.Fatal(err)
	}

	// Two tries at once: one sets the password, the other finds it used.
	race := invite(time.Hour)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := a.AcceptInvite(ctx, race, fmt.Sprintf("racing password %d", i))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	won := 0
	for err := range errs {
		switch {
		case err == nil:
			won++
		case !apperr.Is(err, apperr.Unauthenticated):
			t.Fatalf("a losing try: %v", err)
		}
	}
	var live int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM credential WHERE actor_id = $1 AND kind = 'password' AND revoked_at IS NULL`, yuki).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if won != 1 || live != 1 {
		t.Fatalf("%d tries won, %d live passwords; want one of each", won, live)
	}

	// The system actor is never invited.
	if _, _, err := auth.IssueInvite(ctx, q, res.SystemID, "test", time.Now().Add(time.Hour), time.Now()); err == nil {
		t.Fatal("an invitation for the system actor")
	}
}

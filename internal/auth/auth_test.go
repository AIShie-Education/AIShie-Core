package auth_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

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

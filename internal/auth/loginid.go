package auth

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/ids"
)

// A login ID is a person's sign-in name other than an email: the student or
// staff number a school gives them (學號, 工號), which they sign in with
// where they have no mailbox, and by which a school's own sign-on would know
// them. It is 1 to MaxLoginIDLen letters and digits of ASCII, dots, hyphens
// and underscores; it never holds an @, so that a sign-in name with one is
// an email and one without is a login ID. The database holds the same
// (actor_login_id_valid), and keeps it unique in any case
// (actor_login_id_key). Only a person has one (actor_login_id_is_a_persons).

// MaxLoginIDLen is the longest login ID, in characters, each one byte.
const MaxLoginIDLen = 64

// loginIDChar reports whether c may be in a login ID.
func loginIDChar(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '.' || c == '-' || c == '_'
}

// IsLoginID reports whether s, as it is, is shaped like a login ID.
func IsLoginID(s string) bool {
	if len(s) == 0 || len(s) > MaxLoginIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !loginIDChar(s[i]) {
			return false
		}
	}
	return true
}

// IsEmail reports whether a sign-in name is taken for an email rather than
// a login ID: whether it holds an @, which no login ID does.
func IsEmail(name string) bool { return strings.Contains(name, "@") }

// LoginID is a login ID someone gives, trimmed, or invalid_argument when it
// is not one, naming the field it came in.
func LoginID(field, s string) (string, error) {
	s = strings.TrimSpace(s)
	if !IsLoginID(s) {
		return "", apperr.Invalid("%s must be 1 to %d letters, digits, dots, hyphens or underscores, such as a student or "+
			"staff number; it has no @, which is an email's", field, MaxLoginIDLen)
	}
	return s, nil
}

// LoginIDTaken refuses a login ID someone has already. Registering through a
// join link, it tells the person to sign in, and says nothing else about the
// account.
func LoginIDTaken() *apperr.Error {
	return apperr.Conflicts("that login ID is already registered: sign in with it, then open the link again").
		With("reason", "login_id_taken")
}

// A temporary password is one someone else sets for a person
// (member.reset_password), shown once to whoever set it, to be handed to the
// person, read out or written down: TemporaryPasswordGroups groups of four
// characters from an alphabet with nothing that reads as something else (no
// 0 or o, 1, i or l), joined by hyphens. Sixteen characters from 31 are
// about 79 bits, which no guessing within the limits on signing in comes
// near; the person signs in with it once, and must then choose their own.
const (
	temporaryAlphabet       = "abcdefghjkmnpqrstuvwxyz23456789"
	TemporaryPasswordGroups = 4
)

// NewTemporaryPassword makes a temporary password.
func NewTemporaryPassword() (string, error) {
	var b strings.Builder
	n := big.NewInt(int64(len(temporaryAlphabet)))
	for i := range TemporaryPasswordGroups * 4 {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		k, err := rand.Int(rand.Reader, n)
		if err != nil {
			return "", fmt.Errorf("temporary password: %w", err)
		}
		b.WriteByte(temporaryAlphabet[k.Int64()])
	}
	return b.String(), nil
}

// SetTemporaryPassword gives a person a password someone else chose,
// issuedBy, who holds it too: their passwords before it are revoked, and an
// invitation waiting, as setting any password revokes them; it is marked, so
// that their next sign-in with it must set a new one before anything else
// (credential.must_change); and every session they have is ended, so that
// nobody signed in as them carries on. It says how many sessions it ended.
// Who may do this to whom is the tool's to decide (member.reset_password).
func SetTemporaryPassword(ctx context.Context, q *dbq.Queries, actorID, issuedBy uuid.UUID, password, label string, now time.Time) (int64, error) {
	hash, err := HashNewPassword(password)
	if err != nil {
		return 0, err
	}
	if err := q.RevokePasswordCredentials(ctx, dbq.RevokePasswordCredentialsParams{ActorID: actorID, RevokedAt: &now}); err != nil {
		return 0, err
	}
	if err := q.RevokeInvites(ctx, dbq.RevokeInvitesParams{ActorID: actorID, RevokedAt: &now}); err != nil {
		return 0, err
	}
	if err := q.InsertTemporaryPassword(ctx, dbq.InsertTemporaryPasswordParams{
		ID: ids.New(), ActorID: actorID, SecretHash: &hash, Label: &label, CreatedAt: now, IssuedByActorID: &issuedBy,
	}); err != nil {
		return 0, err
	}
	return q.RevokeSessions(ctx, dbq.RevokeSessionsParams{ActorID: actorID, RevokedAt: &now})
}

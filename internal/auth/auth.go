package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/ids"
)

const (
	KindPassword = "password"
	KindSSO      = "sso"
	KindAPIToken = "api_token"
	KindSession  = "session"

	DefaultSessionTTL = 12 * time.Hour
)

// Principal is an authenticated caller: which actor, by which credential.
type Principal struct {
	ActorID      uuid.UUID
	CredentialID uuid.UUID
	Kind         string // api_token or session
	ExpiresAt    *time.Time
}

// errUnauthenticated is deliberately one message for every way a credential
// can be wrong: unknown, revoked, expired or mismatched. The difference is
// for our logs, not for whoever is guessing.
var errUnauthenticated = apperr.New(apperr.Unauthenticated, "the credential is missing or not valid")

type Authenticator struct {
	pool       *pgxpool.Pool
	sessionTTL time.Duration
	now        func() time.Time
}

func NewAuthenticator(pool *pgxpool.Pool, sessionTTL time.Duration) *Authenticator {
	if sessionTTL <= 0 {
		sessionTTL = DefaultSessionTTL
	}
	return &Authenticator{pool: pool, sessionTTL: sessionTTL, now: time.Now}
}

func (a *Authenticator) SetClock(now func() time.Time) { a.now = now }

// Authenticate verifies a presented bearer secret: an API token or a session.
//
// Nothing about the actor's standing is checked here. A suspended actor still
// authenticates and is then denied by step 1 of authorize() on every call,
// which also puts the attempt in the action log. The system actor is the one
// exception: it never authenticates, so that a token issued to it before the
// database refused them lends nobody the sweeps' authority.
func (a *Authenticator) Authenticate(ctx context.Context, presented string) (Principal, error) {
	prefix, ok := parsePrefix(presented)
	if !ok {
		return Principal{}, errUnauthenticated
	}
	q := dbq.New(a.pool)
	cred, err := q.GetCredentialByPrefix(ctx, &prefix)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, errUnauthenticated
	}
	if err != nil {
		return Principal{}, fmt.Errorf("credential lookup: %w", err)
	}
	now := a.now()
	switch {
	case cred.SecretHash == nil || !tokenMatches(presented, *cred.SecretHash):
		return Principal{}, errUnauthenticated
	case cred.RevokedAt != nil:
		return Principal{}, errUnauthenticated
	case cred.ExpiresAt != nil && !cred.ExpiresAt.After(now):
		return Principal{}, errUnauthenticated
	case cred.ActorKind == "system":
		return Principal{}, errUnauthenticated
	}
	// Best effort: a failure to note the time of use is no reason to refuse.
	_ = q.TouchCredential(ctx, dbq.TouchCredentialParams{ID: cred.ID, LastUsedAt: &now})
	return Principal{ActorID: cred.ActorID, CredentialID: cred.ID, Kind: cred.Kind, ExpiresAt: cred.ExpiresAt}, nil
}

// Session is what a successful login hands back.
type Session struct {
	Token     string
	ActorID   uuid.UUID
	ExpiresAt time.Time
}

// errBadLogin does not say which of the two was wrong.
var errBadLogin = apperr.New(apperr.Unauthenticated, "the email or the password is wrong")

// Login checks an email and password and mints a session.
func (a *Authenticator) Login(ctx context.Context, email, password string) (Session, error) {
	q := dbq.New(a.pool)

	var stored string
	// An email the database cannot hold is one no account has. The database
	// is not asked, since it would answer with a fault of ours, and the
	// guess is answered, and costs the hash, like any other wrong one.
	var actor dbq.GetActorByEmailRow
	err := pgx.ErrNoRows
	if utf8.ValidString(email) && !strings.ContainsRune(email, 0) {
		actor, err = q.GetActorByEmail(ctx, email)
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return Session{}, fmt.Errorf("actor lookup: %w", err)
	default:
		cred, err := q.GetPasswordCredential(ctx, actor.ID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Session{}, fmt.Errorf("password lookup: %w", err)
		}
		if err == nil && cred.SecretHash != nil {
			stored = *cred.SecretHash
		}
	}
	// Always do the expensive comparison, so that timing does not reveal
	// whether the email is known or has a password at all.
	known := stored != ""
	if !known {
		stored = decoyHash
	}
	ok, err := VerifyPassword(password, stored)
	if err != nil {
		return Session{}, fmt.Errorf("verify password: %w", err)
	}
	if !ok || !known || actor.Status != domain.ActorActive {
		return Session{}, errBadLogin
	}
	return a.StartSession(ctx, actor.ID, "password login")
}

// StartSession mints a session credential for an actor whose identity has
// already been established, by password here or by an identity provider.
func (a *Authenticator) StartSession(ctx context.Context, actorID uuid.UUID, label string) (Session, error) {
	tok, err := NewToken()
	if err != nil {
		return Session{}, err
	}
	now := a.now()
	expires := now.Add(a.sessionTTL)
	if err := dbq.New(a.pool).InsertCredential(ctx, dbq.InsertCredentialParams{
		ID: ids.New(), ActorID: actorID, Kind: KindSession, SecretHash: &tok.Hash,
		TokenPrefix: &tok.Prefix, Label: &label, ExpiresAt: &expires, CreatedAt: now,
	}); err != nil {
		return Session{}, fmt.Errorf("start session: %w", err)
	}
	return Session{Token: tok.Full, ActorID: actorID, ExpiresAt: expires}, nil
}

// Logout ends the session (or revokes the token) the caller came in with.
func (a *Authenticator) Logout(ctx context.Context, p Principal) error {
	now := a.now()
	return dbq.New(a.pool).RevokeCredentialByID(ctx, dbq.RevokeCredentialByIDParams{ID: p.CredentialID, RevokedAt: &now})
}

// IssueToken creates an API token for an actor. It is used by the tools that
// issue tokens, by bootstrap and by the operator's command line. The system
// actor is never given one: a token of its would act as the sweeps do, and
// could take their idempotency keys before them.
func IssueToken(ctx context.Context, q *dbq.Queries, actorID uuid.UUID, label string, expiresAt *time.Time, now time.Time) (Token, uuid.UUID, error) {
	actor, err := q.GetActor(ctx, actorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, uuid.Nil, apperr.Missing("no such actor")
	}
	if err != nil {
		return Token{}, uuid.Nil, err
	}
	if actor.Kind == "system" {
		return Token{}, uuid.Nil, apperr.Forbid("the system actor is never issued a token")
	}
	tok, err := NewToken()
	if err != nil {
		return Token{}, uuid.Nil, err
	}
	id := ids.New()
	var l *string
	if label != "" {
		l = &label
	}
	if err := q.InsertCredential(ctx, dbq.InsertCredentialParams{
		ID: id, ActorID: actorID, Kind: KindAPIToken, SecretHash: &tok.Hash,
		TokenPrefix: &tok.Prefix, Label: l, ExpiresAt: expiresAt, CreatedAt: now,
	}); err != nil {
		return Token{}, uuid.Nil, err
	}
	return tok, id, nil
}

// SetPassword replaces an actor's password. Older passwords are revoked, not
// deleted: the row says when each stopped working.
func SetPassword(ctx context.Context, q *dbq.Queries, actorID uuid.UUID, password string, now time.Time) error {
	hash, err := HashPassword(password)
	if err != nil {
		if errors.Is(err, ErrWeakPassword) {
			return apperr.Invalid("%v", err)
		}
		return err
	}
	if err := q.RevokePasswordCredentials(ctx, dbq.RevokePasswordCredentialsParams{ActorID: actorID, RevokedAt: &now}); err != nil {
		return err
	}
	return q.InsertCredential(ctx, dbq.InsertCredentialParams{
		ID: ids.New(), ActorID: actorID, Kind: KindPassword, SecretHash: &hash, CreatedAt: now,
	})
}

// BootstrapInput describes the first human of an installation.
type BootstrapInput struct {
	DisplayName string
	Email       string
	Password    string // optional; without it the root signs in with the token
}

type BootstrapResult struct {
	RootID, SystemID uuid.UUID
	Token            Token
}

// ErrAlreadyBootstrapped means a root actor exists; bootstrap runs once.
var ErrAlreadyBootstrapped = errors.New("this installation already has a root actor")

// Bootstrap creates the root actor, the system actor and root's first
// credential, in one transaction.
//
// Root is the head of the delegation chain and the only actor created by
// nobody. This is the one state change in the system with no action row:
// there is no actor yet for it to be an action of.
func Bootstrap(ctx context.Context, pool *pgxpool.Pool, in BootstrapInput) (BootstrapResult, error) {
	var res BootstrapResult
	err := db.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		// Serialise concurrent bootstraps; the count below is then reliable.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('aishiteru.bootstrap'))`); err != nil {
			return err
		}
		n, err := q.CountRootActors(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			return ErrAlreadyBootstrapped
		}
		now := time.Now()
		res.RootID, res.SystemID = ids.New(), ids.New()
		root, system := domain.PlatformRoot, "system"
		var email *string
		if in.Email != "" {
			email = &in.Email
		}
		if err := q.InsertActor(ctx, dbq.InsertActorParams{
			ID: res.RootID, Kind: "human", DisplayName: in.DisplayName, Email: email, PlatformRole: &root, CreatedAt: now,
		}); err != nil {
			return err
		}
		// The system actor runs the background jobs, so that expiries and
		// roster syncs have someone to be attributed to.
		if err := q.InsertActor(ctx, dbq.InsertActorParams{
			ID: res.SystemID, Kind: system, DisplayName: system, CreatedByActorID: &res.RootID, CreatedAt: now,
		}); err != nil {
			return err
		}
		if in.Password != "" {
			if err := SetPassword(ctx, q, res.RootID, in.Password, now); err != nil {
				return err
			}
		}
		res.Token, _, err = IssueToken(ctx, q, res.RootID, "bootstrap", nil, now)
		return err
	})
	return res, err
}

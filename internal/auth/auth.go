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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/authz"
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
	KindInvite   = "invite"

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
	// PasswordChangeRequired says the password signed in with is one someone
	// else set (member.reset_password): the person must set one of their own
	// (credential.set_password) before anything else, and every other call
	// is refused until they do.
	PasswordChangeRequired bool
}

// errBadLogin does not say which of the two was wrong, nor whether what was
// given for an account was an email or a login ID: one message for all.
var errBadLogin = apperr.New(apperr.Unauthenticated, "the login ID or email, or the password, is wrong")

// Login checks a sign-in name and a password and mints a session. The name
// is an email when it holds an @, and a login ID otherwise; either is
// matched in any case, and trimmed. Both are answered alike, wrong or
// right: the same lookup of one indexed row, the same hash, the same
// refusal.
func (a *Authenticator) Login(ctx context.Context, name, password string) (Session, error) {
	q := dbq.New(a.pool)
	name = strings.TrimSpace(name)

	var stored string
	var mustChange bool
	// A name the database cannot hold, or that is no login ID, is one no
	// account has. The database is not asked, since it would answer with a
	// fault of ours, and the guess is answered, and costs the hash, like any
	// other wrong one.
	var actor dbq.GetActorByEmailRow
	err := pgx.ErrNoRows
	switch {
	case !utf8.ValidString(name) || strings.ContainsRune(name, 0):
	case IsEmail(name):
		actor, err = q.GetActorByEmail(ctx, name)
	case IsLoginID(name):
		var row dbq.GetActorByLoginIDRow
		row, err = q.GetActorByLoginID(ctx, name)
		actor = dbq.GetActorByEmailRow(row)
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
			stored, mustChange = *cred.SecretHash, cred.MustChange
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
	label := "password login"
	if mustChange {
		label = "password login, with a temporary password"
	}
	sess, err := a.StartSession(ctx, actor.ID, label)
	sess.PasswordChangeRequired = mustChange
	return sess, err
}

// StartSession mints a session credential for an actor whose identity has
// already been established, by password here or by an identity provider.
func (a *Authenticator) StartSession(ctx context.Context, actorID uuid.UUID, label string) (Session, error) {
	return a.startSession(ctx, dbq.New(a.pool), actorID, label)
}

// StartSessionIn is StartSession through q, a transaction's: the session a
// person who registers is signed in with comes to be with their account, or
// neither does.
func (a *Authenticator) StartSessionIn(ctx context.Context, q *dbq.Queries, actorID uuid.UUID, label string) (Session, error) {
	return a.startSession(ctx, q, actorID, label)
}

// startSession is StartSession through q, which may be a transaction's.
func (a *Authenticator) startSession(ctx context.Context, q *dbq.Queries, actorID uuid.UUID, label string) (Session, error) {
	tok, err := NewToken()
	if err != nil {
		return Session{}, err
	}
	now := a.now()
	expires := now.Add(a.sessionTTL)
	if err := q.InsertCredential(ctx, dbq.InsertCredentialParams{
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
// could take their idempotency keys before them. issuedBy is the actor who
// asked for it, nil when no actor did (bootstrap, the command line).
func IssueToken(ctx context.Context, q *dbq.Queries, actorID uuid.UUID, issuedBy *uuid.UUID, label string, expiresAt *time.Time, now time.Time) (Token, uuid.UUID, error) {
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
		TokenPrefix: &tok.Prefix, Label: l, ExpiresAt: expiresAt, CreatedAt: now, IssuedByActorID: issuedBy,
	}); err != nil {
		return Token{}, uuid.Nil, err
	}
	return tok, id, nil
}

// SetPassword replaces an actor's password. Older passwords are revoked, not
// deleted: the row says when each stopped working. So is an invitation
// waiting: it was for choosing a password, and one has been chosen.
func SetPassword(ctx context.Context, q *dbq.Queries, actorID uuid.UUID, password string, now time.Time) error {
	hash, err := HashNewPassword(password)
	if err != nil {
		return err
	}
	return setPasswordHash(ctx, q, actorID, hash, now)
}

// CheckNewPassword holds a password someone chooses to the rules for one,
// without hashing it: one outside them is their fault (invalid_argument).
func CheckNewPassword(password string) error {
	if len(password) < MinPasswordLen || len(password) > MaxPasswordLen {
		return apperr.Invalid("%v", ErrWeakPassword)
	}
	return nil
}

// HashNewPassword is HashPassword for a password someone chooses: one
// outside the rules for a password is their fault (invalid_argument).
func HashNewPassword(password string) (string, error) {
	hash, err := HashPassword(password)
	if errors.Is(err, ErrWeakPassword) {
		return "", apperr.Invalid("%v", err)
	}
	return hash, err
}

func setPasswordHash(ctx context.Context, q *dbq.Queries, actorID uuid.UUID, hash string, now time.Time) error {
	if err := q.RevokePasswordCredentials(ctx, dbq.RevokePasswordCredentialsParams{ActorID: actorID, RevokedAt: &now}); err != nil {
		return err
	}
	if err := q.RevokeInvites(ctx, dbq.RevokeInvitesParams{ActorID: actorID, RevokedAt: &now}); err != nil {
		return err
	}
	return q.InsertCredential(ctx, dbq.InsertCredentialParams{
		ID: ids.New(), ActorID: actorID, Kind: KindPassword, SecretHash: &hash, CreatedAt: now,
	})
}

// NewPerson is someone who registers themselves. A join link is the one way
// a person does (docs/schema.md §2.2): there is no open sign-up. They give an
// email, a login ID or both, which they will sign in with; one left empty is
// none.
type NewPerson struct {
	DisplayName string
	Email       string
	LoginID     string
	// PasswordHash is HashNewPassword's, made before the transaction that
	// registers them, so that the hash holds no lock.
	PasswordHash string
	// CreatedBy is whose authority lets them in: the link's maker, which
	// keeps the chain of who created whom (actor.created_by_actor_id)
	// without a gap.
	CreatedBy uuid.UUID
}

// EmailTaken refuses to register an email someone has already. It tells the
// person to sign in, and says nothing else about the account.
func EmailTaken() *apperr.Error {
	return apperr.Conflicts("that email is already registered: sign in with it, then open the link again").
		With("reason", "email_taken")
}

// RegisterPerson makes a person, their email and login ID, whichever they
// gave, recorded as vouched for by nobody but them (email_verified,
// login_id_verified false: Core sends no email, and checks no number), and
// sets their password. It runs in the transaction that seats them, so that
// an account that joins nothing is never left behind. An email or a login
// ID registered already is refused (EmailTaken, LoginIDTaken), and nothing
// of the account that has it is read or touched: two at once for one
// address or one number meet at actor_email_key or actor_login_id_key, and
// the second is refused the same way. The transaction is then over; the
// caller starts again, and finds it taken before it gets here.
func RegisterPerson(ctx context.Context, q *dbq.Queries, p NewPerson, now time.Time) (uuid.UUID, error) {
	id := ids.New()
	var email, loginID *string
	if p.Email != "" {
		email = &p.Email
	}
	if p.LoginID != "" {
		loginID = &p.LoginID
	}
	err := q.InsertRegisteredPerson(ctx, dbq.InsertRegisteredPersonParams{
		ID: id, DisplayName: p.DisplayName, Email: email, LoginID: loginID, CreatedByActorID: &p.CreatedBy, CreatedAt: now})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "actor_email_key":
			return uuid.Nil, EmailTaken()
		case "actor_login_id_key":
			return uuid.Nil, LoginIDTaken()
		}
	}
	if err != nil {
		return uuid.Nil, err
	}
	if err := setPasswordHash(ctx, q, id, p.PasswordHash, now); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// IssueInvite makes an invitation for an actor to set their password, and
// revokes the one they had: only the newest works. Who may be invited is the
// tool's to decide (actor.invite, actor.invite_new); the database holds the
// one live invitation, and refuses one for the system actor. Setting a
// password, by the invitation or otherwise, revokes it (SetPassword).
// issuedBy is who made it, and is asked again when it is taken up
// (AcceptInvite).
func IssueInvite(ctx context.Context, q *dbq.Queries, actorID uuid.UUID, issuedBy *uuid.UUID, label string, expiresAt, now time.Time) (Token, uuid.UUID, error) {
	tok, err := NewInvite()
	if err != nil {
		return Token{}, uuid.Nil, err
	}
	if err := q.RevokeInvites(ctx, dbq.RevokeInvitesParams{ActorID: actorID, RevokedAt: &now}); err != nil {
		return Token{}, uuid.Nil, err
	}
	id := ids.New()
	if err := q.InsertCredential(ctx, dbq.InsertCredentialParams{
		ID: id, ActorID: actorID, Kind: KindInvite, SecretHash: &tok.Hash,
		TokenPrefix: &tok.Prefix, Label: &label, ExpiresAt: &expiresAt, CreatedAt: now, IssuedByActorID: issuedBy,
	}); err != nil {
		return Token{}, uuid.Nil, err
	}
	return tok, id, nil
}

// Why a department administrator may not invite a person (again), as
// InviteRefusal says it: the clauses of the rule, in the order they are
// asked.
const (
	InviteNotAPerson      = "not_a_person"
	InviteSignedIn        = "signed_in"
	InvitePlatformRole    = "platform_role"
	InviteAdministers     = "administers"
	InviteOwnsAgents      = "owns_agents"
	InviteSeatedElsewhere = "seated_elsewhere"
)

// InviteRefusal is the rule for an invitation a department administrator
// makes, applied to what InvitableBy found: the first clause the person
// fails, or "" when they pass them all. Whoever holds an invitation can sign
// in as the person it is for, so the person's account must reach nothing
// the issuer does not administer already: they are a person, have never
// been able to sign in, hold no platform role and no appointment, own no
// agent, and hold seats only in courses of the departments the issuer
// administers. A platform administrator is held to actor.invite's own rule
// instead.
func InviteRefusal(r dbq.InvitableByRow) string {
	switch {
	case !r.IsPerson:
		return InviteNotAPerson
	case r.CanSignIn:
		return InviteSignedIn
	case r.HoldsRole:
		return InvitePlatformRole
	case r.Administers:
		return InviteAdministers
	case r.OwnsAgents:
		return InviteOwnsAgents
	case r.SeatsOutside > 0:
		return InviteSeatedElsewhere
	}
	return ""
}

// issuerStands reports whether whoever issued an invitation would still be
// let make it: an active platform administrator, or an active department
// administrator the rule lets invite the person as they are now. The rule
// is asked again because the person may have been seated elsewhere since,
// or the issuer's appointment ended, and the invitation would then open an
// account that reaches beyond its issuer; and someone who administers
// nothing is refused though a person with no seats passes every clause. An
// invitation made before its issuer was recorded is a platform
// administrator's, as every invitation then was.
func issuerStands(ctx context.Context, q *dbq.Queries, issuedBy *uuid.UUID, actorID uuid.UUID) (bool, error) {
	if issuedBy == nil {
		return true, nil
	}
	issuer, err := authz.LoadActor(ctx, q, *issuedBy)
	if err != nil {
		return false, err
	}
	switch {
	case !issuer.Active():
		return false, nil
	case authz.Platform(issuer, domain.PlatformRoot, domain.PlatformAdmin).Level.Allowed():
		return true, nil
	case !issuer.Administers:
		return false, nil
	}
	facts, err := q.InvitableBy(ctx, dbq.InvitableByParams{IssuerID: *issuedBy, ActorID: actorID})
	if err != nil {
		return false, err
	}
	return InviteRefusal(facts) == "", nil
}

// errBadInvite is one message for every way an invitation can be wrong:
// unknown, used, replaced, withdrawn, expired, for an account that is
// suspended, or from an issuer who could no longer make it.
var errBadInvite = apperr.New(apperr.Unauthenticated,
	"the invitation is not valid: it may have expired, been used, or been replaced by a newer one")

// Accepted is what taking up an invitation hands back: the session it signs
// the person in with, and what they will sign in with from now on: their
// email, their login ID, or both, whichever they have.
type Accepted struct {
	Session
	Email   *string
	LoginID *string
}

// AcceptInvite sets the password of the person an invitation was made for,
// uses the invitation up, and signs them in.
//
// The invitation is checked first, and the password hashed only for one
// that is good: a guess costs a lookup, not an argon2 hash. It is checked
// again, locked, in the transaction that sets the password, which revokes
// it, and starts the session: of two tries at once, one sets the password
// and the other finds the invitation used, and nothing is half done. A weak
// password is refused before anything changes, and the invitation still
// works.
func (a *Authenticator) AcceptInvite(ctx context.Context, presented, password string) (Accepted, error) {
	prefix, ok := parseInvitePrefix(presented)
	if !ok {
		return Accepted{}, errBadInvite
	}
	check := func(q *dbq.Queries) (dbq.GetInviteByPrefixRow, error) {
		inv, err := q.GetInviteByPrefix(ctx, &prefix)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return inv, errBadInvite
		case err != nil:
			return inv, fmt.Errorf("invitation lookup: %w", err)
		case inv.SecretHash == nil || !tokenMatches(presented, *inv.SecretHash),
			inv.RevokedAt != nil,
			inv.ExpiresAt == nil || !inv.ExpiresAt.After(a.now()),
			// Only an actor with a sign-in name, an email or a login ID, is
			// invited (actor.invite), and a suspension since then holds. The
			// system actor is refused as Authenticate refuses it, though it
			// holds no credential.
			inv.ActorKind == "system", inv.ActorStatus != domain.ActorActive, inv.ActorEmail == nil && inv.ActorLoginID == nil:
			return inv, errBadInvite
		}
		// Asked again in the transaction that takes it up, under the
		// invitation's lock: an issuer who could not make it now is refused
		// as an expired one is, and the refusal says nothing more.
		switch ok, err := issuerStands(ctx, q, inv.IssuedByActorID, inv.ActorID); {
		case err != nil:
			return inv, fmt.Errorf("invitation issuer: %w", err)
		case !ok:
			return inv, errBadInvite
		}
		return inv, nil
	}
	if _, err := check(dbq.New(a.pool)); err != nil {
		return Accepted{}, err
	}
	hash, err := HashNewPassword(password)
	if err != nil {
		return Accepted{}, err
	}
	var acc Accepted
	err = db.InTx(ctx, a.pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		inv, err := check(q)
		if err != nil {
			return err
		}
		if err := setPasswordHash(ctx, q, inv.ActorID, hash, a.now()); err != nil {
			return err
		}
		acc.Email, acc.LoginID = inv.ActorEmail, inv.ActorLoginID
		acc.Session, err = a.startSession(ctx, q, inv.ActorID, "invitation accepted")
		return err
	})
	if err != nil {
		return Accepted{}, err
	}
	return acc, nil
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
		res.Token, _, err = IssueToken(ctx, q, res.RootID, nil, "bootstrap", nil, now)
		return err
	})
	return res, err
}

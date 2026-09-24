// Package pipeline is the one road every tool call takes, whoever makes it
// and through whichever adapter:
//
//	decode and validate the arguments      (bad input: an error, nothing recorded)
//	a Read:  authorize, scope included → run the query.  No action row.
//	a Write: canonicalize and hash the arguments
//	         same key seen before? → replay it, or refuse if the content differs
//	         authorize: steps 1–3, resolve the target, steps 4–5
//	         write the action row — before anything happens, denials included
//	           denied            → done
//	           confirm_required  → it is a proposal now; nothing else is written
//	           otherwise         → execute inside a savepoint, write the events,
//	                               mark the row executed (pending_review also
//	                               enters the review queue)
//
// All of a Write happens in one transaction. The action row is written before
// the state change in statement order, and both commit together or not at
// all: there is never a half-done action to resume. A tool that fails for a
// reason of the caller's making is rolled back to the savepoint and the
// attempt is recorded as failed. A fault of ours rolls back everything and
// records nothing; the caller retries with the same key and gets a clean run.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/canon"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/signing"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

type Config struct {
	// ProposalTTL is how long a proposal may wait for a decision. Approval
	// checks it inline, so it holds whether or not the sweep has run. Zero
	// means proposals do not expire.
	ProposalTTL time.Duration
	// Secrets seals the secret fields of a call (a password) into its payload
	// hash, so that a key reused with a different secret is caught like any
	// other reuse, while the hash gives nothing away to whoever reads the
	// database. It must be the same on every instance, or a retry landing
	// elsewhere reads as a conflict; nil makes one for this process alone.
	Secrets *signing.Signer
}

// DefaultProposalTTL is two weeks: long enough to survive a holiday, short
// enough that an approval still means what the proposer meant.
const DefaultProposalTTL = 14 * 24 * time.Hour

// MaxIdempotencyKeyLen bounds the key; it is indexed.
const MaxIdempotencyKeyLen = 200

type Pipeline struct {
	pool *pgxpool.Pool
	reg  *tool.Registry
	cfg  Config
	now  func() time.Time
}

func New(pool *pgxpool.Pool, reg *tool.Registry, cfg Config) *Pipeline {
	if cfg.Secrets == nil {
		s, err := signing.New("")
		if err != nil {
			panic("pipeline: " + err.Error())
		}
		cfg.Secrets = s
	}
	return &Pipeline{pool: pool, reg: reg, cfg: cfg, now: time.Now}
}

// secretPurpose keeps the sealed secrets apart from every other digest the
// signer makes.
const secretPurpose = "action.secret"

// payload returns what is stored for a call and what is hashed for it. The
// two differ only where the tool takes a secret: the stored form drops it,
// the hashed form carries a keyed digest of it.
func (p *Pipeline) payload(t tool.Tool, raw []byte) (canonical []byte, hash string, err error) {
	canonical, err = canon.Canonicalize(raw, t.SecretIn...)
	if err != nil {
		return nil, "", err
	}
	hashed := canonical
	if len(t.SecretIn) > 0 {
		hashed, err = canon.Sealed(raw, func(field string, value []byte) string {
			return p.cfg.Secrets.Digest(secretPurpose, append([]byte(field+"\n"), value...))
		}, t.SecretIn...)
		if err != nil {
			return nil, "", err
		}
	}
	return canonical, canon.Hash(t.Name, hashed), nil
}

// SetClock replaces the clock, for tests of expiry.
func (p *Pipeline) SetClock(now func() time.Time) { p.now = now }

func (p *Pipeline) Registry() *tool.Registry { return p.reg }

// Caller is who is making the call, as established by authentication.
type Caller struct {
	ActorID uuid.UUID
}

// Outcome is what became of a call that got as far as being attempted.
// Calls that did not — unknown tool, bad arguments, target not found, a key
// reused for different content — come back as an error instead.
type Outcome struct {
	// Status is executed, proposed, denied or failed; a replay of an old
	// proposal may also report rejected or cancelled.
	Status domain.ActionStatus `json:"status"`
	// ActionID is nil for a Read: reads are not actions.
	ActionID    *uuid.UUID         `json:"action_id,omitempty"`
	ReviewState domain.ReviewState `json:"review_state,omitempty"`
	Result      json.RawMessage    `json:"result,omitempty"`
	Error       *apperr.Error      `json:"error,omitempty"`
	// Replayed is true when this is the stored outcome of an earlier call
	// with the same idempotency key, and nothing was done this time.
	Replayed bool `json:"replayed,omitempty"`
}

// Invoke runs one tool call.
func (p *Pipeline) Invoke(ctx context.Context, caller Caller, name string, rawArgs []byte, idempotencyKey string) (Outcome, error) {
	t, ok := p.reg.Get(name)
	if !ok || t.Internal {
		return Outcome{}, apperr.Missing("there is no tool named %q", name)
	}
	in, err := t.Decode(rawArgs)
	if err != nil {
		return Outcome{}, err
	}
	if t.Kind == tool.Read {
		return p.invokeRead(ctx, caller, t, in)
	}
	return p.invokeWrite(ctx, caller, t, in, rawArgs, idempotencyKey)
}

// isCallerFault reports whether err is the caller's doing, and as what. An
// integrity violation that escapes a tool — two calls racing for the one
// live grade a submission may have — is a conflict, not a server fault: the
// savepoint is rolled back and the transaction carries on.
func isCallerFault(err error) (*apperr.Error, bool) {
	if e, ok := apperr.As(err); ok {
		return e, true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && len(pgErr.Code) == 5 {
		switch pgErr.Code[:2] {
		case "23": // integrity constraint violation
			return apperr.Conflicts("the change conflicts with the current state").
				With("constraint", pgErr.ConstraintName), true
		case "40": // serialization failure, deadlock
			return apperr.Conflicts("the change collided with another; try again"), true
		}
	}
	return nil, false
}

// savepoint runs fn inside a savepoint of tx. On success, deferred
// constraints are checked there and then, so that a violation surfaces as
// this action's failure rather than at COMMIT, where it would take the
// action row down with it.
func savepoint(ctx context.Context, tx pgx.Tx, fn func(sp pgx.Tx) (any, error)) (any, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	out, err := fn(sp)
	if err == nil {
		_, err = sp.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE")
	}
	if err == nil {
		_, err = sp.Exec(ctx, "SET CONSTRAINTS ALL DEFERRED")
	}
	if err != nil {
		if rbErr := sp.Rollback(ctx); rbErr != nil {
			return nil, errors.Join(err, rbErr)
		}
		return nil, err
	}
	if err := sp.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// validate runs a tool's Validate inside a savepoint. Validate may take locks
// — a grade's takes the work's — and so may lose a deadlock; the savepoint
// keeps that from aborting the transaction its failure is then recorded in.
// What it locked it keeps when it succeeds, for Execute after it.
func validate(ctx context.Context, tx pgx.Tx, t tool.Tool, m *domain.Member, in any) error {
	_, err := savepoint(ctx, tx, func(sp pgx.Tx) (any, error) {
		return nil, t.Validate(ctx, dbq.New(sp), m, in)
	})
	return err
}

func errorResult(e *apperr.Error) []byte {
	b, _ := json.Marshal(map[string]any{"error": e})
	return b
}

// storedError reads back what errorResult wrote.
func storedError(result []byte) *apperr.Error {
	var v struct {
		Error *apperr.Error `json:"error"`
	}
	if json.Unmarshal(result, &v) != nil {
		return nil
	}
	return v.Error
}

// stripTopLevel returns raw without the named top-level fields. It is what
// keeps a freshly issued token out of the action log, so when raw is not an
// object it can take fields from, it stores nothing rather than everything.
func stripTopLevel(raw []byte, fields []string) []byte {
	if len(fields) == 0 || len(raw) == 0 {
		return raw
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return []byte("{}")
	}
	for _, f := range fields {
		delete(m, f)
	}
	out, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return out
}

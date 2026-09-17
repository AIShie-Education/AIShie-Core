package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/canon"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/events"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/ids"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

// InvokeSystem runs an Internal tool as the system actor: what a background
// sweep does, it does as an action, so that an expired membership or a
// cancelled proposal has a row in the log saying what did it and when, like
// everything else.
//
// It differs from Invoke in exactly one way: nobody is authorized, because
// nobody is calling. The system actor holds no membership and no platform
// role; the tools it may run are the ones marked Internal, which no adapter
// exposes and Invoke refuses. The row is written as 'autonomous' with no
// member. Everything else is the same road: the action row first, the tool in
// a savepoint, the events last, one transaction.
//
// The idempotency key is chosen by the sweep and is deterministic — it names
// the thing swept — so that two instances sweeping at once, or one sweep run
// twice, act once.
func (p *Pipeline) InvokeSystem(ctx context.Context, systemActor uuid.UUID, name string, args any, key string) (Outcome, error) {
	t, ok := p.reg.Get(name)
	if !ok || !t.Internal {
		return Outcome{}, fmt.Errorf("pipeline: %q is not an internal tool", name)
	}
	if t.Kind != tool.Write || key == "" {
		return Outcome{}, fmt.Errorf("pipeline: %s: a system call is a write with an idempotency key", name)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return Outcome{}, err
	}
	in, err := t.Decode(raw)
	if err != nil {
		return Outcome{}, err
	}
	canonical, err := canon.Canonicalize(raw, t.SecretIn...)
	if err != nil {
		return Outcome{}, err
	}
	hash := canon.Hash(t.Name, canonical)

	var out Outcome
	err = db.InTx(ctx, p.pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		now := p.now()

		existing, err := q.GetActionByKey(ctx, dbq.GetActionByKeyParams{ActorID: systemActor, IdempotencyKey: key})
		if err == nil {
			out, err = replay(existing, hash)
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		target, err := t.Resolve(ctx, q, in)
		if err != nil {
			return err
		}
		if target.Type == "" {
			target.Type = noun(t.Name)
		}
		var courseID *uuid.UUID
		if target.CourseID != uuid.Nil {
			courseID = &target.CourseID
		}
		actionID := ids.New()
		n, err := q.InsertAction(ctx, dbq.InsertActionParams{
			ID: actionID, ActorID: systemActor, CourseID: courseID, ActionType: t.Name,
			TargetType: target.Type, TargetID: target.ID, Payload: canonical, PayloadHash: hash, IdempotencyKey: key,
			AuthzResult: dbq.AutonomyLevel(domain.Autonomous.String()), Status: string(domain.StatusApproved), CreatedAt: now,
		})
		if err != nil {
			return fmt.Errorf("record action: %w", err)
		}
		if n == 0 { // another instance got there first
			existing, err := q.GetActionByKey(ctx, dbq.GetActionByKeyParams{ActorID: systemActor, IdempotencyKey: key})
			if err != nil {
				return err
			}
			out, err = replay(existing, hash)
			return err
		}
		out = Outcome{ActionID: &actionID, ReviewState: domain.ReviewNone}

		buf := &events.Buffer{}
		res, err := savepoint(ctx, tx, func(sp pgx.Tx) (any, error) {
			return t.Execute(ctx, &tool.ExecCtx{
				Tx: sp, Q: dbq.New(sp), Actor: domain.Actor{ID: systemActor, DisplayName: "system", Status: domain.ActorActive},
				ActionID: actionID, Now: now, Emit: stamp(buf, actionID),
			}, in)
		})
		if err != nil {
			e, ok := isCallerFault(err)
			if !ok {
				return fmt.Errorf("%s: %w", t.Name, err)
			}
			out.Status, out.Error = domain.StatusFailed, e
			return q.MarkActionFailed(ctx, dbq.MarkActionFailedParams{ID: actionID, Result: errorResult(e)})
		}
		full, err := json.Marshal(res)
		if err != nil {
			return err
		}
		if err := events.Flush(ctx, q, buf); err != nil {
			return err
		}
		out.Status, out.Result = domain.StatusExecuted, full
		return q.MarkActionExecuted(ctx, dbq.MarkActionExecutedParams{ID: actionID, ExecutedAt: &now,
			ReviewState: string(domain.ReviewNone), Result: stripTopLevel(full, t.SecretOut)})
	})
	if err != nil {
		if e, ok := apperr.As(err); ok {
			return Outcome{}, e
		}
		return Outcome{}, err
	}
	return out, nil
}

// Config returns the pipeline's settings; the sweeps use the same proposal
// TTL that approval checks inline.
func (p *Pipeline) Config() Config { return p.cfg }

// Clock is the pipeline's clock, which a test may have moved. The sweeps tell
// the time by it so that what they sweep and what approval refuses agree.
func (p *Pipeline) Clock() time.Time { return p.now() }

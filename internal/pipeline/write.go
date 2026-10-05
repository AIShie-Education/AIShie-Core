package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/authz"
	"github.com/AIShie-Education/AIShie-Core/internal/canon"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/events"
	"github.com/AIShie-Education/AIShie-Core/internal/ids"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

// checkKey refuses an idempotency key the action log cannot take.
func checkKey(t tool.Tool, key string) error {
	switch {
	case key == "":
		return apperr.Invalid("%s changes state, so it needs an idempotency key", t.Name)
	case !utf8.ValidString(key) || strings.ContainsRune(key, 0):
		// The database cannot hold it, and would say so as a fault of ours.
		return apperr.Invalid("the idempotency key must be UTF-8 text without U+0000")
	case utf8.RuneCountInString(key) > MaxIdempotencyKeyLen:
		return apperr.Invalid("the idempotency key is longer than %d characters", MaxIdempotencyKeyLen)
	}
	return nil
}

func (p *Pipeline) invokeWrite(ctx context.Context, caller Caller, t tool.Tool, in any, rawArgs []byte, key string, revises *uuid.UUID) (Outcome, error) {
	if err := checkKey(t, key); err != nil {
		return Outcome{}, err
	}
	canonical, hash, err := p.payload(t, rawArgs)
	if err != nil {
		return Outcome{}, apperr.Invalid("%v", err)
	}

	var out Outcome
	again := false
	err = p.inTx(ctx, func(tx pgx.Tx, final bool) error {
		var err error
		if again {
			// Made again, it is made from what the caller sent, not from what
			// the first attempt's tool may have made of it.
			if in, err = t.Decode(rawArgs); err != nil {
				return err
			}
		}
		again = true
		out, err = p.write(ctx, tx, caller, t, in, canonical, hash, key, revises, final)
		return err
	})
	if err != nil {
		return Outcome{}, err
	}
	return out, nil
}

// write is the body of one Write, inside its transaction. Returning an error
// rolls everything back and records nothing. Every recorded outcome, denied
// and failed included, is a nil error and a commit. A deadlock lost on an
// attempt that is not the final one is returned as it is, for inTx to make
// the call again; lost on the final one, it is recorded as failed. revises
// is the proposal the call revises, if it names one (InvokeRevising).
func (p *Pipeline) write(ctx context.Context, tx pgx.Tx, caller Caller, t tool.Tool, in any, canonical []byte, hash, key string, revises *uuid.UUID, final bool) (Outcome, error) {
	q := dbq.New(tx)
	now := p.now()

	if err := q.LockIdempotencyKey(ctx, dbq.LockIdempotencyKeyParams{ActorID: caller.ActorID, IdempotencyKey: key}); err != nil {
		return Outcome{}, fmt.Errorf("idempotency key lock: %w", err)
	}
	// Seen before? A call with this key that is still in flight has been
	// waited for just above; the insert below still settles any other race.
	existing, err := q.GetActionByKey(ctx, dbq.GetActionByKeyParams{ActorID: caller.ActorID, IdempotencyKey: key})
	if err == nil {
		return replay(existing, hash, revises)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Outcome{}, fmt.Errorf("idempotency lookup: %w", err)
	}

	actor, err := authz.LoadActor(ctx, q, caller.ActorID)
	if err != nil {
		return Outcome{}, err
	}
	a, err := p.authorize(ctx, q, t, in, actor, caller.CredentialID, nil, now)
	if err != nil {
		return Outcome{}, err
	}
	if revises != nil {
		if err := revisable(ctx, q, actor.ID, a.courseID, *revises); err != nil {
			return Outcome{}, err
		}
	}
	level := a.decision.Level

	// Decide what the row will say before writing it. A call that is allowed
	// but breaks a rule of the domain is recorded as failed, and one that
	// would only queue a proposal nobody could ever approve is failed now
	// rather than after someone has been asked.
	status := initialStatus(level)
	var result []byte
	var failure *apperr.Error
	switch {
	case !level.Allowed():
		failure = a.refusal()
	case t.Validate != nil:
		if err := validate(ctx, tx, t, a.decision.Member, now, in); err != nil {
			if !final && transient(err) {
				return Outcome{}, err
			}
			e, ok := isCallerFault(err)
			if !ok {
				return Outcome{}, err
			}
			status, failure = domain.StatusFailed, e
		}
	}
	if failure != nil {
		result = errorResult(failure)
	}
	// A proposal is carried out later, against a world that may have moved.
	// What it must be carried out against as it stands now is pinned into
	// the stored payload; the hash stays that of the call as it was made,
	// which is what a retry of it presents.
	if status == domain.StatusProposed && t.Pin != nil {
		pinned, err := t.Pin(ctx, q, a.decision.Member, now, in)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			// Pin runs in the transaction itself, not in a savepoint, so an
			// error from the database has aborted it and nothing can be
			// recorded in it. Returned as it is, a lost deadlock is made
			// again, and anything else is the fault of ours it is.
			return Outcome{}, err
		}
		if err != nil {
			e, ok := isCallerFault(err)
			if !ok {
				return Outcome{}, err
			}
			status, failure = domain.StatusFailed, e
			result = errorResult(failure)
		} else if raw, err := json.Marshal(pinned); err != nil {
			return Outcome{}, fmt.Errorf("%s: pinned arguments: %w", t.Name, err)
		} else if canonical, err = canon.Canonicalize(raw, t.SecretIn...); err != nil {
			return Outcome{}, fmt.Errorf("%s: pinned arguments: %w", t.Name, err)
		}
	}

	actionID := ids.New()
	row := dbq.InsertActionParams{
		ID:              actionID,
		ActorID:         actor.ID,
		CourseID:        a.courseID,
		ActionType:      t.Name,
		TargetType:      a.target.Type,
		TargetID:        a.target.ID,
		Payload:         canonical,
		PayloadHash:     hash,
		IdempotencyKey:  key,
		AuthzResult:     dbq.AutonomyLevel(level.String()),
		Status:          string(status),
		Result:          result,
		CreatedAt:       now,
		RevisesActionID: revises,
	}
	if a.decision.Member != nil {
		row.MemberID = &a.decision.Member.ID
	}
	if level.Allowed() {
		// The capacity it was allowed in. A denied call was allowed in none,
		// whatever the caller holds.
		row.Authority, row.AuthorityDeptID = a.authority, a.authorityDept
	}
	n, err := q.InsertAction(ctx, row)
	if err != nil {
		return Outcome{}, fmt.Errorf("record action: %w", err)
	}
	if n == 0 {
		// Another call with this key committed while we were working.
		existing, err := q.GetActionByKey(ctx, dbq.GetActionByKeyParams{ActorID: caller.ActorID, IdempotencyKey: key})
		if err != nil {
			return Outcome{}, fmt.Errorf("idempotency lookup after conflict: %w", err)
		}
		return replay(existing, hash, revises)
	}

	out := Outcome{Status: status, ActionID: &actionID, ReviewState: domain.ReviewNone, Error: failure}
	buf := &events.Buffer{}

	switch status {
	case domain.StatusDenied, domain.StatusFailed:
		return out, nil

	case domain.StatusProposed:
		payload := map[string]any{"action_type": t.Name, "target_type": a.target.Type, "target_id": a.target.ID}
		if revises != nil {
			payload["revises_action_id"] = *revises
		}
		buf.Emit(events.Event{
			Type: events.ActionProposed, CourseID: a.courseID, ActionID: &actionID,
			SubjectType: "action", SubjectID: &actionID, Payload: payload,
		})
		if err := events.Flush(ctx, q, buf); err != nil {
			return Outcome{}, err
		}
		return out, nil
	}

	// Execute.
	res, err := savepoint(ctx, tx, func(sp pgx.Tx) (any, error) {
		return t.Execute(ctx, &tool.ExecCtx{
			Tx: sp, Q: dbq.New(sp), Actor: actor, CredentialID: caller.CredentialID, Member: a.decision.Member, Admin: a.admin,
			Perms: a.perms(t), ActionID: actionID, Now: now, ActionCreatedAt: now, Emit: stamp(buf, actionID),
		}, in)
	})
	if err != nil {
		if !final && transient(err) {
			return Outcome{}, fmt.Errorf("%s: %w", t.Name, err)
		}
		e, ok := isCallerFault(err)
		if !ok {
			return Outcome{}, fmt.Errorf("%s: %w", t.Name, err)
		}
		if err := q.MarkActionFailed(ctx, dbq.MarkActionFailedParams{ID: actionID, Result: errorResult(e)}); err != nil {
			return Outcome{}, fmt.Errorf("record failure: %w", err)
		}
		out.Status, out.Error = domain.StatusFailed, e
		return out, nil
	}

	full, err := json.Marshal(res)
	if err != nil {
		return Outcome{}, fmt.Errorf("%s: result: %w", t.Name, err)
	}
	if err := events.Flush(ctx, q, buf); err != nil {
		return Outcome{}, err
	}
	if level == domain.PendingReview {
		out.ReviewState = domain.ReviewPending
	}
	if err := q.MarkActionExecuted(ctx, dbq.MarkActionExecutedParams{
		ID: actionID, ExecutedAt: &now, ReviewState: string(out.ReviewState),
		Result: stripTopLevel(full, t.SecretOut),
	}); err != nil {
		return Outcome{}, fmt.Errorf("record execution: %w", err)
	}
	out.Status, out.Result = domain.StatusExecuted, full
	return out, nil
}

// revisable says why the actor may not revise proposal id in course, or nil
// when they may: it is their own (the actor's, whichever seat it was made
// from), in the same course, and its decider sent it back for changes. Any
// other action of the course's, or of none, is no proposal of theirs to
// revise; one of theirs still waiting, or decided otherwise, is not one sent
// back. That a proposal was sent back never changes, nor whose it is, so it
// is read without a lock. The database holds the same
// (action_revises_own_changes_requested).
func revisable(ctx context.Context, q dbq.Querier, actor uuid.UUID, course *uuid.UUID, id uuid.UUID) error {
	notYours := apperr.Invalid("revises names no proposal of yours in this course").With("reason", "not_revisable")
	if course == nil {
		return notYours
	}
	prop, err := q.GetActionInCourse(ctx, dbq.GetActionInCourseParams{ID: id, CourseID: course})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && prop.ActorID != actor) {
		return notYours
	}
	if err != nil {
		return err
	}
	if prop.Status != string(domain.StatusChangesRequested) {
		return apperr.Precondition("the proposal it revises is %s: only one sent back for changes is revised", prop.Status).
			With("reason", "not_revisable")
	}
	return nil
}

func sameID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// stamp returns an Emit that files events under actionID unless the event
// already names an action. An approved proposal's events name the proposal.
func stamp(buf *events.Buffer, actionID uuid.UUID) func(events.Event) {
	return func(e events.Event) {
		if e.ActionID == nil {
			id := actionID
			e.ActionID = &id
		}
		buf.Emit(e)
	}
}

// replay answers a call whose idempotency key has been used before. Nothing
// is executed. The same content gets the stored outcome, as it stands now: a
// proposal replayed after it was approved reports executed. Different content
// under the same key is a bug in the caller, and saying "done" to a request
// that was never made would hide it; so is the same call revising another
// proposal, or one where the first revised none, or the other way round.
//
// A call whose action was emptied since, as one about an assignment deleted
// for good (assignment.delete), has no stored outcome to give: whatever is
// sent, it is told its target was deleted, and by which action.
func replay(a dbq.Action, hash string, revises *uuid.UUID) (Outcome, error) {
	if a.RedactedByActionID != nil {
		return Outcome{}, apperr.Missing("what this %s call was about has been deleted for good, and its record emptied", a.ActionType).
			With("reason", CancelTargetDeleted).With("action_id", a.ID).With("by_action_id", *a.RedactedByActionID)
	}
	if a.PayloadHash != hash || !sameID(a.RevisesActionID, revises) {
		return Outcome{}, apperr.New(apperr.IdempotencyConflict,
			"this idempotency key was already used for a different %s call; use a new key for a new request", a.ActionType).
			With("action_id", a.ID)
	}
	id := a.ID
	out := Outcome{
		Status:      domain.ActionStatus(a.Status),
		ActionID:    &id,
		ReviewState: domain.ReviewState(a.ReviewState),
		Replayed:    true,
	}
	// What result holds is fixed by the status, not by its shape: a failed,
	// denied or cancelled row stores {"error": …}; an executed one stores the
	// tool's result, which is free to have an "error" field of its own — a
	// decision whose proposal was cancelled reports exactly that.
	switch out.Status {
	case domain.StatusDenied, domain.StatusFailed, domain.StatusCancelled:
		out.Error = storedError(a.Result)
	default:
		if len(a.Result) > 0 {
			out.Result = a.Result
		}
	}
	return out, nil
}

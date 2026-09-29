package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

// unlisted finds an Unlisted tool by name. Asking for any other kind is a
// fault of the handler asking, not of its caller.
func (p *Pipeline) unlisted(name string) (tool.Tool, error) {
	t, ok := p.reg.Get(name)
	if !ok || !t.Unlisted {
		return tool.Tool{}, fmt.Errorf("pipeline: %q is not an unlisted tool", name)
	}
	return t, nil
}

// InvokeUnlisted runs an Unlisted tool for a caller a handler of this server
// has authenticated: the way a signed-in person joins a course by a link. It
// is Invoke in every other way — gated, recorded, replayed by its key — for a
// tool neither adapter offers.
func (p *Pipeline) InvokeUnlisted(ctx context.Context, caller Caller, name string, rawArgs []byte, idempotencyKey string) (Outcome, error) {
	t, err := p.unlisted(name)
	if err != nil {
		return Outcome{}, err
	}
	in, err := t.Decode(rawArgs)
	if err != nil {
		return Outcome{}, err
	}
	return p.invoke(ctx, caller, t, in, rawArgs, idempotencyKey)
}

// NewActor is the caller of a call made with InvokeAsNew, made in the call's
// own transaction.
type NewActor struct {
	// Make makes the actor, and says who it is. It runs first, and again if
	// the call is made again after losing a deadlock, each time in a fresh
	// transaction.
	Make func(ctx context.Context, q *dbq.Queries, now time.Time) (uuid.UUID, error)
	// Then runs once the call has executed, in the same transaction, before
	// it commits: the session a person who registered is signed in with.
	Then func(ctx context.Context, q *dbq.Queries, actorID uuid.UUID, out Outcome) error
}

// undone carries an outcome that was not carried out out of the transaction
// it undoes.
type undone struct{ err *apperr.Error }

func (u undone) Error() string { return u.err.Error() }

// InvokeAsNew runs an Unlisted Write for an actor that comes to be in the
// same transaction: a person who registers through a join link is made, and
// joins, and is signed in, all at once or not at all. The call is made as
// any is, as the new actor, and recorded as theirs. Only an executed outcome
// commits. Any other — denied, failed, a proposal — undoes everything, the
// actor included, and comes back as the error, with nothing recorded: the
// actor it would be recorded against never came to be.
func (p *Pipeline) InvokeAsNew(ctx context.Context, name string, rawArgs []byte, idempotencyKey string, na NewActor) (Outcome, error) {
	t, err := p.unlisted(name)
	if err != nil {
		return Outcome{}, err
	}
	if t.Kind != tool.Write || na.Make == nil {
		return Outcome{}, fmt.Errorf("pipeline: %s: a new actor's call is a write, by an actor Make makes", name)
	}
	if err := checkKey(t, idempotencyKey); err != nil {
		return Outcome{}, err
	}
	if _, err := t.Decode(rawArgs); err != nil {
		return Outcome{}, err
	}
	canonical, hash, err := p.payload(t, rawArgs)
	if err != nil {
		return Outcome{}, apperr.Invalid("%v", err)
	}

	var out Outcome
	err = p.inTx(ctx, func(tx pgx.Tx, final bool) error {
		in, err := t.Decode(rawArgs)
		if err != nil {
			return err
		}
		q := dbq.New(tx)
		actor, err := na.Make(ctx, q, p.now())
		if err != nil {
			return err
		}
		if out, err = p.write(ctx, tx, Caller{ActorID: actor}, t, in, canonical, hash, idempotencyKey, final); err != nil {
			return err
		}
		if out.Status != domain.StatusExecuted {
			e := out.Error
			if e == nil {
				e = apperr.Precondition("%s was not carried out: it is %s", t.Name, out.Status)
			}
			return undone{e}
		}
		if na.Then != nil {
			return na.Then(ctx, q, actor, out)
		}
		return nil
	})
	var u undone
	if errors.As(err, &u) {
		return Outcome{}, u.err
	}
	if err != nil {
		return Outcome{}, err
	}
	return out, nil
}

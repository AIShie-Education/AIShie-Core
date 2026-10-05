package pipeline

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/authz"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

// invokeEphemeral runs an Ephemeral tool: a change not worth an action, an
// answer's draft as it is written, many times a second, or a claim on what
// waits in a queue, which lapses. It is authorized as a Write is — the
// caller's seat held to the end, an archived course refused, scope
// included — and carried out at once, in a transaction of its own, at
// whatever level above denied the caller holds: there is nothing to
// propose, since nothing of it outlasts what it is about. It records
// nothing: no action row, no idempotency key, no event. A denial is answered
// as a Read's is; a refusal of the tool's, or a fault, undoes all of it and
// comes back as the error, as a Read's does. A call that loses a deadlock is
// made again, once, as a Write is (inTx).
//
// One that can wait (tool.CanWait), asked to, and that finds nothing to do,
// waits as a Read does (waitFor), and is carried out again, authorized again,
// each time it may find something.
func (p *Pipeline) invokeEphemeral(ctx context.Context, caller Caller, t tool.Tool, in any) (Outcome, error) {
	return p.waitFor(ctx, t, in, func(before func(*tool.ReadCtx)) (Outcome, any, error) {
		return p.ephemeral(ctx, caller, t, in, before)
	})
}

// ephemeral is one call of an Ephemeral tool: authorized, and carried out.
// before, when given, is called once the call is authorized and before it is
// carried out. It returns the outcome and the tool's result as it was, for a
// call that waits to compare.
func (p *Pipeline) ephemeral(ctx context.Context, caller Caller, t tool.Tool, in any, before func(*tool.ReadCtx)) (Outcome, any, error) {
	var (
		out Outcome
		res any
	)
	err := p.inTx(ctx, func(tx pgx.Tx, _ bool) error {
		q := dbq.New(tx)
		now := p.now()
		actor, err := authz.LoadActor(ctx, q, caller.ActorID)
		if err != nil {
			return err
		}
		a, err := p.authorize(ctx, q, t, in, actor, caller.CredentialID, nil, now)
		if err != nil {
			return err
		}
		if !a.decision.Level.Allowed() {
			out, res = Outcome{Status: domain.StatusDenied, Error: a.refusal()}, nil
			return nil
		}
		if before != nil {
			before(&tool.ReadCtx{Q: q, Actor: actor, Member: a.decision.Member, Admin: a.admin, Now: now})
		}
		res, err = t.Execute(ctx, &tool.ExecCtx{
			Tx: tx, Q: q, Actor: actor, CredentialID: caller.CredentialID, Member: a.decision.Member, Admin: a.admin,
			Perms: a.perms(t), Now: now, ActionCreatedAt: now,
		}, in)
		if err != nil {
			return err
		}
		body, err := json.Marshal(res)
		if err != nil {
			return fmt.Errorf("%s: result: %w", t.Name, err)
		}
		out = Outcome{Status: domain.StatusExecuted, Result: body}
		return nil
	})
	if err != nil {
		if e, ok := isCallerFault(err); ok {
			return Outcome{}, nil, e
		}
		return Outcome{}, nil, fmt.Errorf("%s: %w", t.Name, err)
	}
	return out, res, nil
}

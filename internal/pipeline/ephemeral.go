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
// answer's draft as it is written, many times a second. It is authorized as
// a Write is — the caller's seat held to the end, an archived course
// refused, scope included — and carried out at once, in a transaction of its
// own, at whatever level above denied the caller holds: there is nothing to
// propose, since nothing of it outlasts what it is a draft of. It records
// nothing: no action row, no idempotency key, no event. A denial is answered
// as a Read's is; a refusal of the tool's, or a fault, undoes all of it and
// comes back as the error, as a Read's does. A call that loses a deadlock is
// made again, once, as a Write is (inTx).
func (p *Pipeline) invokeEphemeral(ctx context.Context, caller Caller, t tool.Tool, in any) (Outcome, error) {
	var out Outcome
	err := p.inTx(ctx, func(tx pgx.Tx, _ bool) error {
		q := dbq.New(tx)
		now := p.now()
		actor, err := authz.LoadActor(ctx, q, caller.ActorID)
		if err != nil {
			return err
		}
		a, err := p.authorize(ctx, q, t, in, actor, nil, now)
		if err != nil {
			return err
		}
		if !a.decision.Level.Allowed() {
			out = Outcome{Status: domain.StatusDenied, Error: a.refusal()}
			return nil
		}
		res, err := t.Execute(ctx, &tool.ExecCtx{
			Tx: tx, Q: q, Actor: actor, CredentialID: caller.CredentialID, Member: a.decision.Member, Admin: a.admin,
			Now: now, ActionCreatedAt: now,
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
			return Outcome{}, e
		}
		return Outcome{}, fmt.Errorf("%s: %w", t.Name, err)
	}
	return out, nil
}

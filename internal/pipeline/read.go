package pipeline

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/authz"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

// invokeRead authorizes a Read exactly as a Write is authorized, scope
// included, and then runs it. It records nothing: only state changes are
// actions. Any level above denied permits a read; "confirm before reading"
// has no meaning, so the rungs in between are not distinguished here.
func (p *Pipeline) invokeRead(ctx context.Context, caller Caller, t tool.Tool, in any) (Outcome, error) {
	q := dbq.New(p.pool)
	now := p.now()

	actor, err := authz.LoadActor(ctx, q, caller.ActorID)
	if err != nil {
		return Outcome{}, err
	}
	a, err := p.authorize(ctx, q, t, in, actor, nil, now)
	if err != nil {
		return Outcome{}, err
	}
	if !a.decision.Level.Allowed() {
		return Outcome{Status: domain.StatusDenied, Error: a.refusal()}, nil
	}

	rc := &tool.ReadCtx{Q: q, Actor: actor, Member: a.decision.Member, Admin: a.admin, Now: now}
	if a.decision.Member != nil {
		rc.Scope = authz.FilterFor(a.decision.Member)
	}
	res, err := t.Query(ctx, rc, in)
	if err != nil {
		if e, ok := isCallerFault(err); ok {
			return Outcome{}, e
		}
		return Outcome{}, fmt.Errorf("%s: %w", t.Name, err)
	}
	body, err := json.Marshal(res)
	if err != nil {
		return Outcome{}, fmt.Errorf("%s: result: %w", t.Name, err)
	}
	return Outcome{Status: domain.StatusExecuted, Result: body}, nil
}

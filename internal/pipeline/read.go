package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/AIShie-Education/AIShie-Core/internal/authz"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
	"github.com/AIShie-Education/AIShie-Core/internal/wake"
)

// invokeRead authorizes a Read exactly as a Write is authorized, scope
// included, and then runs it. It records nothing: only state changes are
// actions. Any level above denied permits a read; "confirm before reading"
// has no meaning, so the rungs in between are not distinguished here.
//
// A Read that can wait (tool.CanWait), asked to (wait_s), and that finds
// nothing new, waits for news of what it reads (package wake), holding no
// transaction and no connection, and then reads again, authorized again as
// the first time: whatever the caller lost meanwhile, it does not read. It
// reads again each time it is woken, and answers once it finds something,
// or at the end of its time with what it then finds. A call whose client has
// gone answers what it last read, and one that cannot wait — the server's
// bounds reached, or no hub — answers what it first read. Either way it is
// one call, as the rate limit counts it.
func (p *Pipeline) invokeRead(ctx context.Context, caller Caller, t tool.Tool, in any) (Outcome, error) {
	var wait time.Duration
	if t.WaitSeconds != nil && p.cfg.Wake != nil {
		wait = time.Duration(t.WaitSeconds(in)) * time.Second
	}
	if wait <= 0 {
		out, _, err := p.read(ctx, caller, t, in, nil)
		return out, err
	}
	until := time.Now().Add(wait)
	var (
		w     *wake.Waiter
		first any
		held  bool
	)
	defer func() {
		if w != nil {
			w.Close()
		}
	}()
	// The first read subscribes once it is authorized and before it
	// queries, so that whatever is committed after its query looked wakes
	// the call.
	subscribe := func(rc *tool.ReadCtx) {
		if first == nil && w == nil {
			w, _ = p.cfg.Wake.Subscribe(rc.Actor.ID, t.WaitFor(rc, in))
		}
	}
	for final := false; ; {
		out, res, err := p.read(ctx, caller, t, in, subscribe)
		if err != nil || out.Status != domain.StatusExecuted || w == nil || final {
			return out, err
		}
		if first == nil {
			first = res
		}
		if !t.WaitNothing(in, first, res) || !time.Now().Before(until) {
			return out, nil
		}
		if !held {
			wake.Hold(ctx, until)
			held = true
		}
		switch w.Wait(ctx, until) {
		case wake.Cancelled:
			return out, nil
		case wake.TimedOut, wake.ShutDown:
			final = true
		}
	}
}

// read is one read: authorized, and run. before, when given, is called once
// the call is authorized and before it queries. It returns the outcome and
// the tool's result as it was, for a call that waits to compare.
func (p *Pipeline) read(ctx context.Context, caller Caller, t tool.Tool, in any, before func(*tool.ReadCtx)) (Outcome, any, error) {
	q := dbq.New(p.pool)
	now := p.now()

	actor, err := authz.LoadActor(ctx, q, caller.ActorID)
	if err != nil {
		return Outcome{}, nil, err
	}
	a, err := p.authorize(ctx, q, t, in, actor, nil, now)
	if err != nil {
		return Outcome{}, nil, err
	}
	if !a.decision.Level.Allowed() {
		return Outcome{Status: domain.StatusDenied, Error: a.refusal()}, nil, nil
	}

	rc := &tool.ReadCtx{Q: q, Actor: actor, Member: a.decision.Member, Admin: a.admin, Now: now}
	if a.decision.Member != nil {
		rc.Scope = authz.FilterFor(a.decision.Member)
	}
	if before != nil {
		before(rc)
	}
	res, err := t.Query(ctx, rc, in)
	if err != nil {
		if e, ok := isCallerFault(err); ok {
			return Outcome{}, nil, e
		}
		return Outcome{}, nil, fmt.Errorf("%s: %w", t.Name, err)
	}
	body, err := json.Marshal(res)
	if err != nil {
		return Outcome{}, nil, fmt.Errorf("%s: result: %w", t.Name, err)
	}
	return Outcome{Status: domain.StatusExecuted, Result: body}, res, nil
}

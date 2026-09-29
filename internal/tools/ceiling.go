package tools

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/authz"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
)

// A seat's ceilings are the most it may hold of each permission at all,
// whatever its row says and whoever grants it (domain.Ceiling, the one rule;
// docs/schema.md §2.2, Ceilings). Seating a member cuts a preset's levels
// down to them and refuses a level named above them (toCeilings); every
// change that widens a seat is held to them (withinCeilings, from grant);
// and the member views show them (ceilingsOf), with the reason each is below
// autonomous, in the codes a refusal gives.

// isAgent reads whether an actor is an agent, for a ceiling: to limit what
// its seat holds, never to grant (domain.Ceiling).
func isAgent(kind string) bool { return kind == "agent" }

// errAboveCeiling refuses a level above what the seat may hold at all,
// saying which permission, how far it may go and why, in the codes the
// member views give (perm_ceiling_reasons).
func errAboveCeiling(p domain.Perm, asked, limit domain.Level, why domain.CeilingReason) *apperr.Error {
	var msg string
	switch why {
	case domain.CeilingAgentNever:
		msg = fmt.Sprintf("a delegate never holds %s: it brings no agents of its own", p)
	case domain.CeilingAgentDecidesByProposal:
		msg = fmt.Sprintf("an agent holds %s at %s at most: it decides and reviews only by proposal, which a person confirms", p, limit)
	case domain.CeilingConversationsAreWithAgents:
		msg = fmt.Sprintf("a person holds %s at %s: conversations are between a person and an agent, and a person answers "+
			"none; people talk to people elsewhere", p, limit)
	case domain.CeilingStudentAgentByProposal:
		msg = fmt.Sprintf("the agent of someone who does not manage the course's members holds %s at %s at most: "+
			"beyond what the delegate preset gives, it acts only by proposal", p, limit)
	default:
		msg = fmt.Sprintf("the delegate's principal holds %s at %s, so the delegate cannot hold it at %s", p, limit, asked)
	}
	return apperr.Forbid("%s", msg).With("permission", string(p)).With("reason", string(why)).With("ceiling", limit.String())
}

// withinCeilings refuses a seat whose levels would go above its ceilings:
// what every change that widens a seat is held to (grant).
func withinCeilings(agent bool, principal *domain.Member, perms permSet) error {
	for _, p := range domain.AllPerms {
		if limit, why := domain.Ceiling(agent, principal, p); perms[p] > limit {
			return errAboveCeiling(p, perms[p], limit, why)
		}
	}
	return nil
}

// toCeilings cuts the levels a seat is being given down to its ceilings, as
// a preset's are cut down to what the owner holds for a delegate; a level
// the call named is refused instead: the caller asked for it by name.
func toCeilings(agent bool, principal *domain.Member, perms permSet, named PermLevels) error {
	for _, p := range domain.AllPerms {
		limit, why := domain.Ceiling(agent, principal, p)
		if perms[p] <= limit {
			continue
		}
		if _, ok := named[string(p)]; ok {
			return errAboveCeiling(p, perms[p], limit, why)
		}
		perms[p] = limit
	}
	return nil
}

// Ceilings is what a view says of a seat's ceilings.
type Ceilings struct {
	PermCeilings       PermLevels        `json:"perm_ceilings" jsonschema:"for each permission, the most this seat may be given, whoever gives it: what member.update_perms, member.update_perms_bulk and member.add_delegate accept at most, before what the one giving it holds themselves"`
	PermCeilingReasons map[string]string `json:"perm_ceiling_reasons,omitempty" jsonschema:"why, for each permission whose ceiling is below autonomous: agent_never (a delegate brings no agents of its own), agent_decides_by_proposal (an agent decides and reviews only by proposal), conversations_are_with_agents (a person answers no conversation: conversation_answer is an agent's), student_agent_by_proposal (the agent of someone who does not manage the members does by proposal what the delegate preset does not give), principal_level (a delegate holds no more than its principal); the same codes a refusal to go above it gives"`
}

// ceilingsOf is a seat's ceilings as the views say them, worked out by
// domain.Ceiling, as enforcement works them out.
func ceilingsOf(agent bool, principal *domain.Member) Ceilings {
	out := Ceilings{PermCeilings: make(PermLevels, len(domain.AllPerms))}
	for _, p := range domain.AllPerms {
		limit, why := domain.Ceiling(agent, principal, p)
		out.PermCeilings[string(p)] = limit.String()
		if why != domain.CeilingNone {
			if out.PermCeilingReasons == nil {
				out.PermCeilingReasons = map[string]string{}
			}
			out.PermCeilingReasons[string(p)] = string(why)
		}
	}
	return out
}

// principalsOf loads the principals of the delegates among seats, as they
// stand, locking nothing: what the views work their ceilings out from. A
// principal that cannot be found leaves its delegates capped at nothing
// (denied principal_level), as authorization would.
func principalsOf(ctx context.Context, q dbq.Querier, principals []uuid.UUID) (map[uuid.UUID]*domain.Member, error) {
	return authz.LoadMembers(ctx, q, principals)
}

// ceilingsFor is a seat's ceilings given what the view knows of it: its
// actor's kind and, for a delegate, its principal's seat among loaded. A
// delegate whose principal was not loaded is given an empty principal,
// which holds nothing.
func ceilingsFor(kind string, principal *uuid.UUID, loaded map[uuid.UUID]*domain.Member) Ceilings {
	if principal == nil {
		return ceilingsOf(isAgent(kind), nil)
	}
	p, ok := loaded[*principal]
	if !ok {
		p = &domain.Member{ID: *principal}
	}
	return ceilingsOf(true, p)
}

// seatCeilings is the ceilings of a seat authorization has loaded, its
// principal with it, given its actor's kind.
func seatCeilings(kind string, m *domain.Member) Ceilings {
	if m.PrincipalID == nil {
		return ceilingsOf(isAgent(kind), nil)
	}
	p := m.Principal
	if p == nil {
		p = &domain.Member{ID: *m.PrincipalID}
	}
	return ceilingsOf(true, p)
}

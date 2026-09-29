package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/authz"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

// authorized is the gate's verdict on one call, with what was learnt on the
// way: the target, and the course it is in.
type authorized struct {
	decision authz.Decision
	target   tool.Target
	courseID *uuid.UUID
	// admin is who makes an Admin-gated call, for the tool to limit itself
	// by (tool.ExecCtx.Admin).
	admin authz.AdminScope
	// authority is the capacity a call outside any course is allowed in, if
	// it is: domain.AuthorityPlatform or AuthorityDepartment, the second with
	// the department of the appointment relied on, when there is one. A call
	// from a seat, or on one's own account, has none.
	authority     *string
	authorityDept *uuid.UUID
	// refused is what a denied call is told, where its gate knows more than
	// the permission that denied it (tool.Gate.Refusal); nil for the plain
	// denial of decision.Reason.
	refused *apperr.Error
}

// refusal is what a denied call is told.
func (a authorized) refusal() *apperr.Error {
	if a.refused != nil {
		return a.refused
	}
	return denial(a.decision.Reason)
}

// explain asks a gate that knows why a seat its permissions deny is refused
// (tool.Gate.Refusal), and records what it says: the refusal, and its reason
// in place of permission_denied. It changes nothing else.
func (a *authorized) explain(ctx context.Context, q dbq.Querier, t tool.Tool, actor domain.Actor) error {
	if t.Gate.Refusal == nil || a.decision.Reason != authz.ReasonPermDenied || a.decision.Member == nil {
		return nil
	}
	e, err := t.Gate.Refusal(ctx, q, actor, a.decision.Member)
	if err != nil || e == nil {
		return err
	}
	why, _ := e.Details["reason"].(string)
	if why == "" {
		return fmt.Errorf("%s: its gate's refusal gives no reason", t.Name)
	}
	a.refused, a.decision.Reason = e, authz.Reason(why)
	return nil
}

// authorize runs a tool's gate for one call.
//
// Steps 1–3 come before the target is resolved, and resolution happens only
// for a caller who passed them. So a non-member probing ids gets the same
// recorded denial whether or not the id exists, and learns nothing from it.
//
// The Admin gate is decided in two parts the same way: whether the caller is
// an administrator of either kind before the target is resolved, and whether
// a department administrator covers what the call is about (reach) after.
//
// asMember, when set, checks that one membership row instead of looking the
// actor's seat up: it is how a proposal is re-authorized on approval.
//
// Before any gate, a person whose password someone else set
// (member.reset_password) is refused every call but setting their own
// (password_change_required), whatever they hold: the sign-in with it must
// set a new password before anything else. It is the caller's call alone
// that is refused, as a suspended actor's is at step 1: approving a proposal
// they made before is someone else's call, re-authorized against their seat.
func (p *Pipeline) authorize(ctx context.Context, q dbq.Querier, t tool.Tool, in any, actor domain.Actor, asMember *uuid.UUID, now time.Time) (authorized, error) {
	// An Ephemeral tool changes state, and is authorized as a Write is.
	write := t.Kind != tool.Read
	a := authorized{target: tool.Target{Type: noun(t.Name)}}
	if asMember == nil && actor.PasswordChangeRequired && !t.SetsOwnPassword {
		a.decision = authz.Decision{Level: domain.Denied, Reason: authz.ReasonPasswordChangeRequired}
		return a, nil
	}

	switch {
	case t.Gate.CourseScoped():
		cid := t.CourseID(in)
		if cid == uuid.Nil {
			return a, apperr.Invalid("course_id is required")
		}
		a.courseID, a.target.CourseID = &cid, cid

		check := func(perms []domain.Perm) (authz.Decision, error) {
			if asMember != nil {
				return authz.ForMember(ctx, q, *asMember, perms, write, now)
			}
			return authz.ForActor(ctx, q, actor.ID, cid, perms, write, now)
		}
		var err error
		if t.Gate.Any {
			// Enough to hold one of them. Which one governs is the target's
			// to say, below.
			for _, perm := range t.Gate.Perms {
				if a.decision, err = check([]domain.Perm{perm}); err != nil || a.decision.Level.Allowed() {
					break
				}
			}
		} else {
			a.decision, err = check(t.Gate.Perms)
		}
		if err != nil {
			return a, err
		}
		// An agent's owner, whose seat counts, may go on to see whether the
		// target concerns their own agents (Gate.OwnAgents), however little
		// the permissions give them; anyone else denied stops here.
		owner := t.Gate.OwnAgents != nil && a.decision.Member != nil && a.decision.Level < domain.Autonomous &&
			(a.decision.Level.Allowed() || a.decision.Reason == authz.ReasonPermDenied)
		if !a.decision.Level.Allowed() && !owner {
			return a, a.explain(ctx, q, t, actor)
		}
		target, err := t.Resolve(ctx, q, in)
		if err == nil && target.CourseID != cid {
			// A tool resolves its target within the course it was given, so
			// this cannot happen; if it does, the target is not in the course.
			err = apperr.Missing("not found in this course")
		}
		if err != nil {
			if _, ok := apperr.As(err); ok && !a.decision.Level.Allowed() {
				// Denied, and let as far as the target only as an owner:
				// whether it exists is none of their business.
				return a, nil
			}
			return a, err
		}
		if target.Type == "" {
			target.Type = a.target.Type
		}
		a.target = target
		if owner {
			level, err := t.Gate.OwnAgents(ctx, q, actor, a.decision.Member, target, now)
			if err != nil {
				return a, err
			}
			if level > a.decision.Level {
				a.decision = authz.Decision{Level: level, Member: a.decision.Member}
			}
			if !a.decision.Level.Allowed() {
				return a, nil
			}
		}
		if t.Gate.Any && len(target.Perms) == 0 {
			return a, fmt.Errorf("%s: its gate is Any, so its Resolve must name the governing permission", t.Name)
		}
		if len(target.Perms) > 0 {
			if a.decision, err = check(target.Perms); err != nil || !a.decision.Level.Allowed() {
				return a, err
			}
		}
		reason, err := authz.CheckScope(ctx, q, a.decision.Member, target.Scope)
		if err != nil {
			return a, err
		}
		if reason != authz.ReasonNone {
			a.decision = authz.Decision{Level: domain.Denied, Reason: reason, Member: a.decision.Member}
		}
		return a, nil

	case len(t.Gate.Platform) > 0:
		a.decision = authz.Platform(actor, t.Gate.Platform...)
		a.authority = capacity(domain.AuthorityPlatform)

	case t.Gate.Admin:
		// A department administrator is held to what the call is about once
		// the target has said, below.
		a.admin, a.decision = authz.Admin(actor, write)
		if a.admin.Platform {
			a.authority = capacity(domain.AuthorityPlatform)
		}

	case t.Gate.Self:
		if actor.Active() {
			a.decision = authz.Decision{Level: domain.Autonomous}
		} else {
			a.decision = authz.Decision{Level: domain.Denied, Reason: authz.ReasonActorNotActive}
		}

	default:
		return a, apperr.Forbid("%s cannot be called from outside", t.Name)
	}

	if !a.decision.Level.Allowed() {
		return a, nil
	}
	target, err := t.Resolve(ctx, q, in)
	if err != nil {
		return a, err
	}
	if target.Type == "" {
		target.Type = a.target.Type
	}
	a.target = target
	if target.CourseID != uuid.Nil {
		cid := target.CourseID
		a.courseID = &cid
	}
	if t.Gate.Admin && !a.admin.Platform {
		if err := a.reach(ctx, q); err != nil || !a.decision.Level.Allowed() {
			return a, err
		}
	}
	// "course.status = 'archived' refuses every write." For a member that is
	// step 1 of authorize(). A platform tool is not gated by membership and
	// never reaches step 1, so the same rule is applied here: an admin is no
	// more able to write to an archived course than its instructor is.
	// Un-archiving is the one exception.
	if a.courseID != nil && write && !t.OnArchived {
		course, err := q.GetCourseForAuthz(ctx, *a.courseID)
		if err != nil {
			return a, fmt.Errorf("load course: %w", err)
		}
		if course.Status == domain.CourseArchived {
			a.decision = authz.Decision{Level: domain.Denied, Reason: authz.ReasonCourseArchived}
		}
	}
	return a, nil
}

// reach is the Admin gate's last step for a department administrator, once
// the target has said what the call is about: a department, which an
// appointment of theirs must cover, and which is then recorded as the one
// relied on; any department of theirs, the tool limiting itself to them; or,
// naming neither, the top of the tree, which is a platform administrator's
// alone.
func (a *authorized) reach(ctx context.Context, q dbq.Querier) error {
	switch {
	case a.target.DeptID != nil:
		auth, ok, err := a.admin.Covers(ctx, q, *a.target.DeptID)
		if err != nil {
			return err
		}
		if !ok {
			a.decision = authz.Decision{Level: domain.Denied, Reason: authz.ReasonDepartmentScope}
			return nil
		}
		a.authority, a.authorityDept = capacity(domain.AuthorityDepartment), &auth.DeptID
	case a.target.AnyDept:
		a.authority = capacity(domain.AuthorityDepartment)
	default:
		a.decision = authz.Decision{Level: domain.Denied, Reason: authz.ReasonPlatformRole}
	}
	return nil
}

func capacity(c string) *string { return &c }

// noun is the part of a tool name before the dot: what kind of thing it acts
// on. It stands in for the target type when a call is denied before its
// target was looked up.
func noun(toolName string) string {
	n, _, _ := strings.Cut(toolName, ".")
	return n
}

func denial(reason authz.Reason) *apperr.Error {
	return apperr.Forbid("not permitted").With("reason", string(reason))
}

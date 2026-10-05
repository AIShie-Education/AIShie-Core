package tools

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/events"
	"github.com/AIShie-Education/AIShie-Core/internal/ids"
	"github.com/AIShie-Education/AIShie-Core/internal/members"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

// Internal tools are what the background sweeps do. They are run by the
// system actor through pipeline.InvokeSystem, are offered by neither adapter,
// and have no gate: nobody calls them, so there is nobody to authorize.
//
// Each re-checks, under a row lock, the very condition the sweep selected it
// for. Between the sweep's SELECT and this transaction a person may have
// decided the proposal or extended the membership, and the sweep must lose
// that race quietly.

func systemTools() []tool.Tool {
	return []tool.Tool{actionExpire(), memberExpire(), memberRemoveOrphan(), submissionMarkMissing()}
}

// Names of the internal tools, for the sweeps.
const (
	ToolActionExpire          = "action.expire"
	ToolMemberExpire          = "member.expire"
	ToolMemberRemoveOrphan    = "member.remove_orphan"
	ToolSubmissionMarkMissing = "submission.mark_missing"
)

type ActionExpireIn struct {
	ActionID uuid.UUID `json:"action_id"`
	// CreatedBefore is the cutoff the sweep used. The tool checks the row
	// against it again rather than trusting that the sweep chose well.
	CreatedBefore time.Time `json:"created_before"`
}

type SweepOut struct {
	Done bool `json:"done" jsonschema:"false when there turned out to be nothing to do"`
}

func actionExpire() tool.Tool {
	return tool.Define(tool.Spec[ActionExpireIn, SweepOut]{
		Name:        ToolActionExpire,
		Description: "Cancel a proposal that has waited longer than the proposal TTL for a decision.",
		Kind:        tool.Write, Internal: true,
		Resolve: func(ctx context.Context, q dbq.Querier, in ActionExpireIn) (tool.Target, error) {
			// The sweep's own row says which course it acted in.
			course, err := q.GetActionCourse(ctx, in.ActionID)
			if errors.Is(err, pgx.ErrNoRows) {
				return tool.Target{}, apperr.Missing("no such action")
			}
			t := tool.Target{Type: "action", ID: &in.ActionID}
			if course != nil {
				t.CourseID = *course
			}
			return t, err
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in ActionExpireIn) (SweepOut, error) {
			prop, err := ec.Q.GetActionForUpdate(ctx, in.ActionID)
			if errors.Is(err, pgx.ErrNoRows) {
				return SweepOut{}, apperr.Missing("no such action")
			}
			if err != nil {
				return SweepOut{}, err
			}
			if prop.Status != string(domain.StatusProposed) || !prop.CreatedAt.Before(in.CreatedBefore) {
				return SweepOut{}, nil // decided in the meantime: nothing to do
			}
			_, stored := pipeline.Cancellation(pipeline.CancelExpired, nil)
			n, err := ec.Q.CancelProposal(ctx, dbq.CancelProposalParams{ID: prop.ID, Result: stored})
			if err != nil || n == 0 {
				return SweepOut{}, err
			}
			// Filed under the proposal, so that its proposer finds it.
			id := prop.ID
			ec.Emit(events.Event{Type: events.ActionCancelled, CourseID: prop.CourseID, ActionID: &id,
				SubjectType: "action", SubjectID: &id,
				Payload: map[string]any{"action_type": prop.ActionType, "reason": pipeline.CancelExpired, "by_action_id": ec.ActionID}})
			return SweepOut{Done: true}, nil
		},
	})
}

type MemberExpireIn struct {
	CourseID uuid.UUID `json:"course_id"`
	MemberID uuid.UUID `json:"member_id"`
}

type MemberExpireOut struct {
	Done               bool `json:"done"`
	CancelledProposals int  `json:"cancelled_proposals"`
}

func memberExpire() tool.Tool {
	return tool.Define(tool.Spec[MemberExpireIn, MemberExpireOut]{
		Name: ToolMemberExpire,
		Description: "Remove a member whose expires_at has passed. The member has been denied on every call since that " +
			"moment already; this makes the removal visible and cancels what they had proposed.",
		Kind: tool.Write, Internal: true,
		Resolve: func(ctx context.Context, q dbq.Querier, in MemberExpireIn) (tool.Target, error) {
			return tool.Target{CourseID: in.CourseID, Type: "course_member", ID: &in.MemberID}, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in MemberExpireIn) (MemberExpireOut, error) {
			if err := holdPrincipalOf(ctx, ec.Q, in.MemberID); err != nil {
				return MemberExpireOut{}, err
			}
			m, err := ec.Q.GetMemberForSweep(ctx, in.MemberID)
			if errors.Is(err, pgx.ErrNoRows) {
				return MemberExpireOut{}, apperr.Missing("no such member")
			}
			if err != nil {
				return MemberExpireOut{}, err
			}
			// Someone may have extended or removed the membership since the
			// sweep looked.
			if m.CourseID != in.CourseID || m.Status == domain.MemberRemoved || m.ExpiresAt == nil || m.ExpiresAt.After(ec.Now) {
				return MemberExpireOut{}, nil
			}
			n, err := members.Remove(ctx, ec.Q, ec.Emit, m.CourseID, m.ID, members.ReasonExpired)
			return MemberExpireOut{Done: err == nil, CancelledProposals: n}, err
		},
	})
}

// ErrNotOrphaned is what member.remove_orphan returns for a seat that turned
// out not to be orphaned, or to be removed already, once it was locked. Like
// ErrSweepMoot it is not an apperr, so nothing is kept of the call: its key
// names the seat, and a no-op stored under it would pass the seat over for
// good.
var ErrNotOrphaned = errors.New("the seat is not an orphan to remove")

type MemberRemoveOrphanIn struct {
	CourseID uuid.UUID `json:"course_id"`
	MemberID uuid.UUID `json:"member_id"`
}

func memberRemoveOrphan() tool.Tool {
	return tool.Define(tool.Spec[MemberRemoveOrphanIn, MemberExpireOut]{
		Name: ToolMemberRemoveOrphan,
		Description: "Remove a seat that counts for nothing for good: a delegate's whose principal is removed or past its " +
			"expiry, or one that no longer matches its actor's owner. authorize() has refused it since; this makes the " +
			"removal visible and cancels what it had proposed.",
		Kind: tool.Write, Internal: true,
		Resolve: func(_ context.Context, _ dbq.Querier, in MemberRemoveOrphanIn) (tool.Target, error) {
			return tool.Target{CourseID: in.CourseID, Type: "course_member", ID: &in.MemberID}, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in MemberRemoveOrphanIn) (MemberExpireOut, error) {
			// The delegate's seat is locked, as the expiry sweep locks the
			// seat it removes, after its principal's KEY SHARE, as whatever
			// locks a delegate's seat takes them (holdPrincipalOf). Nothing
			// the principal can come to undoes an orphan (SeatOrphaned).
			if err := holdPrincipalOf(ctx, ec.Q, in.MemberID); err != nil {
				return MemberExpireOut{}, err
			}
			m, err := ec.Q.GetMemberForSweep(ctx, in.MemberID)
			if errors.Is(err, pgx.ErrNoRows) {
				return MemberExpireOut{}, apperr.Missing("no such member")
			}
			if err != nil {
				return MemberExpireOut{}, err
			}
			if m.CourseID != in.CourseID || m.Status == domain.MemberRemoved {
				return MemberExpireOut{}, ErrNotOrphaned
			}
			orphaned, err := ec.Q.SeatOrphaned(ctx, dbq.SeatOrphanedParams{MemberID: m.ID, Now: ec.Now})
			if err != nil {
				return MemberExpireOut{}, err
			}
			if !orphaned {
				return MemberExpireOut{}, ErrNotOrphaned
			}
			n, err := members.Remove(ctx, ec.Q, ec.Emit, m.CourseID, m.ID, members.ReasonOrphaned)
			return MemberExpireOut{Done: err == nil, CancelledProposals: n}, err
		},
	})
}

// ErrSweepMoot is what submission.mark_missing returns for an assignment that
// is no longer published and due as the sweep listed it: unpublished, or its
// due date moved or cleared, since. It is not an apperr, so the pipeline keeps
// nothing of the call: the sweep's key names the due date, and a no-op stored
// under it would pass the assignment over for good once it is published again,
// or given back the same due date.
var ErrSweepMoot = errors.New("the assignment is no longer published and due as the sweep listed it")

type MarkMissingIn struct {
	CourseID     uuid.UUID `json:"course_id"`
	AssignmentID uuid.UUID `json:"assignment_id"`
	// DueAt is the due date the sweep saw. If it has moved since, there is
	// nothing to do yet.
	DueAt time.Time `json:"due_at"`
	// Groups is whether the sweep saw a group assignment, whose key says
	// so: if it has become the other kind since, there is nothing to do
	// under this key.
	Groups bool `json:"groups,omitempty"`
}

type MarkMissingOut struct {
	Done    bool `json:"done"`
	Missing int  `json:"missing" jsonschema:"students marked as having handed in nothing; on a group assignment, groups"`
}

func submissionMarkMissing() tool.Tool {
	return tool.Define(tool.Spec[MarkMissingIn, MarkMissingOut]{
		Name: ToolSubmissionMarkMissing,
		Description: "When an assignment's due date passes: record that it has, and give every current student who has " +
			"handed in nothing a 'missing' submission, so that the gap is a row a grader can see and grade. Late work " +
			"takes the missing row over, unless a grade has been entered or proposed for it. On a group assignment, every " +
			"group of its set with members and no submission gets one, for its members.",
		Kind: tool.Write, Internal: true,
		Resolve: func(ctx context.Context, q dbq.Querier, in MarkMissingIn) (tool.Target, error) {
			return tool.Target{CourseID: in.CourseID, Type: "assignment", ID: &in.AssignmentID}, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in MarkMissingIn) (MarkMissingOut, error) {
			a, err := ec.Q.GetAssignmentForSubmission(ctx, dbq.GetAssignmentForSubmissionParams{ID: in.AssignmentID, CourseID: in.CourseID})
			if errors.Is(err, pgx.ErrNoRows) {
				return MarkMissingOut{}, apperr.Missing("no such assignment")
			}
			if err != nil {
				return MarkMissingOut{}, err
			}
			if a.PublishedAt == nil || a.DueAt == nil || !a.DueAt.Equal(in.DueAt) || a.DueAt.After(ec.Now) || (a.GroupSetID != nil) != in.Groups {
				return MarkMissingOut{}, ErrSweepMoot
			}
			ec.Emit(events.Event{Type: EventAssignmentDuePassed, CourseID: &in.CourseID, SubjectType: "assignment",
				SubjectID: &a.ID, AssignmentID: &a.ID})
			if a.GroupSetID != nil {
				return markGroupsMissing(ctx, ec, dbq.GetAssignmentInCourseRow(a))
			}

			students, err := ec.Q.ListStudentsWithoutSubmission(ctx, dbq.ListStudentsWithoutSubmissionParams{CourseID: in.CourseID, AssignmentID: a.ID})
			if err != nil {
				return MarkMissingOut{}, err
			}
			out := MarkMissingOut{Done: true}
			for _, student := range students {
				id := ids.New()
				n, err := ec.Q.InsertMissingSubmission(ctx, dbq.InsertMissingSubmissionParams{ID: id, AssignmentID: a.ID,
					CourseID: in.CourseID, StudentMemberID: student, CreatedAt: ec.Now})
				if err != nil {
					return MarkMissingOut{}, err
				}
				if n == 0 {
					continue // the student started a draft this very moment
				}
				out.Missing++
				s := student
				ec.Emit(events.Event{Type: EventSubmissionMissing, CourseID: &in.CourseID, SubjectType: "submission",
					SubjectID: &id, StudentMemberID: &s, AssignmentID: &a.ID})
			}
			return out, nil
		},
	})
}

// markGroupsMissing is the sweep on group assignment a: each group of its
// set, not archived, with no submission row, is given a 'missing' row for
// its members now, less those another group's work for a names; a group
// left with nobody gets none. Students in no group get nothing: the roster
// says they are in none.
func markGroupsMissing(ctx context.Context, ec *tool.ExecCtx, a dbq.GetAssignmentInCourseRow) (MarkMissingOut, error) {
	groups, err := ec.Q.ListGroupsWithoutSubmission(ctx, dbq.ListGroupsWithoutSubmissionParams{SetID: *a.GroupSetID, AssignmentID: a.ID})
	if err != nil {
		return MarkMissingOut{}, err
	}
	out := MarkMissingOut{Done: true}
	for _, g := range groups {
		id, err := writeGroupMissing(ctx, ec, a, g)
		if err != nil {
			return MarkMissingOut{}, err
		}
		if id != nil {
			out.Missing++
		}
	}
	return out, nil
}

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/authz"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

func gradeReadTools() []tool.Tool { return []tool.Tool{gradeList(), gradeGet()} }

var readGrades = tool.Gate{Perms: []domain.Perm{domain.PermGradeRead}}

type GradeView struct {
	ID                uuid.UUID       `json:"id"`
	StudentMemberID   uuid.UUID       `json:"student_member_id"`
	SubmissionID      *uuid.UUID      `json:"submission_id,omitempty"`
	ComponentID       *uuid.UUID      `json:"component_id,omitempty"`
	AssignmentID      *uuid.UUID      `json:"assignment_id,omitempty"`
	Origin            string          `json:"origin" jsonschema:"entered by a grader, or computed: a rolled-up total written down when grades were posted"`
	Score             decimal.Decimal `json:"score"`
	Feedback          *string         `json:"feedback,omitempty"`
	Breakdown         json.RawMessage `json:"breakdown,omitempty"`
	RubricVersionID   *uuid.UUID      `json:"rubric_version_id,omitempty"`
	GraderMemberID    uuid.UUID       `json:"grader_member_id"`
	CreatedByActionID uuid.UUID       `json:"created_by_action_id" jsonschema:"the action that made this grade: who, under which membership, approved by whom"`
	State             string          `json:"state" jsonschema:"draft, posted or superseded"`
	PostedAt          *time.Time      `json:"posted_at,omitempty"`
	SupersededBy      *uuid.UUID      `json:"superseded_by,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
}

func viewGrade(g dbq.GetGradeFullRow) GradeView {
	// There is no status column to keep in sync: the state is read off the
	// two facts that define it.
	state := "draft"
	switch {
	case g.SupersededBy != nil:
		state = "superseded"
	case g.PostedAt != nil:
		state = "posted"
	}
	return GradeView{ID: g.ID, StudentMemberID: g.StudentMemberID, SubmissionID: g.SubmissionID, ComponentID: g.ComponentID,
		AssignmentID: g.AssignmentID, Origin: g.Origin, Score: g.Score, Feedback: g.Feedback, Breakdown: g.Breakdown,
		RubricVersionID: g.RubricVersionID, GraderMemberID: g.GraderMemberID, CreatedByActionID: g.CreatedByActionID,
		State: state, PostedAt: g.PostedAt, SupersededBy: g.SupersededBy, CreatedAt: g.CreatedAt}
}

// seesDrafts: drafts and history are for those who grade. Everyone else sees
// live posted grades and nothing else — which is the rule for students,
// stated without asking whether anyone is a student.
func seesDrafts(m *domain.Member) bool {
	return m.Perm(domain.PermGradeSubmit).Allowed() || m.Perm(domain.PermGradePost).Allowed()
}

type GradeListIn struct {
	tool.InCourse
	StudentMemberID *uuid.UUID `json:"student_member_id,omitempty"`
	AssignmentID    *uuid.UUID `json:"assignment_id,omitempty"`
	Page
}

type GradeListOut struct {
	Grades []GradeView `json:"grades"`
	Next   *uuid.UUID  `json:"next,omitempty"`
}

func gradeList() tool.Tool {
	return tool.Define(tool.Spec[GradeListIn, GradeListOut]{
		Name: "grade.list",
		Description: "Grades within the caller's scope. Members who grade also see drafts and superseded grades; everyone " +
			"else sees posted grades only. A member limited to listed assignments does not see grades on components.",
		Kind: tool.Read, Gate: readGrades,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/grades"},
		Resolve: func(_ context.Context, _ dbq.Querier, in GradeListIn) (tool.Target, error) {
			return tool.Target{CourseID: in.CourseID, Type: "grade"}, nil
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in GradeListIn) (GradeListOut, error) {
			rows, err := rc.Q.ListGrades(ctx, dbq.ListGradesParams{
				CourseID: in.CourseID, After: in.after(), MaxRows: in.limit(),
				StudentMemberID: in.StudentMemberID, AssignmentID: in.AssignmentID, IncludeDrafts: seesDrafts(rc.Member),
				StudentAll: rc.Scope.StudentAll, AssignmentAll: rc.Scope.AssignmentAll, MemberID: rc.Scope.MemberID,
			})
			out := GradeListOut{Grades: make([]GradeView, 0, len(rows))}
			for _, r := range rows {
				out.Grades = append(out.Grades, viewGrade(dbq.GetGradeFullRow(r)))
			}
			if len(rows) > 0 && len(rows) == int(in.limit()) {
				out.Next = &rows[len(rows)-1].ID
			}
			return out, err
		},
	})
}

type GradeIDIn struct {
	tool.InCourse
	GradeID uuid.UUID `json:"grade_id"`
}

func gradeGet() tool.Tool {
	return tool.Define(tool.Spec[GradeIDIn, GradeView]{
		Name:        "grade.get",
		Description: "One grade in full: score, feedback, per-criterion breakdown, the rubric version it was given against, and the action that made it.",
		Kind:        tool.Read, Gate: readGrades,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/grades/{grade_id}"},
		Resolve: func(ctx context.Context, q dbq.Querier, in GradeIDIn) (tool.Target, error) {
			g, err := q.GetGradeFull(ctx, dbq.GetGradeFullParams{ID: in.GradeID, CourseID: in.CourseID})
			if errors.Is(err, pgx.ErrNoRows) {
				return tool.Target{}, apperr.Missing("no such grade in this course")
			}
			if err != nil {
				return tool.Target{}, err
			}
			t := tool.Target{CourseID: in.CourseID, Type: "grade", ID: &in.GradeID, Scope: authz.Target{StudentMemberIDs: []uuid.UUID{g.StudentMemberID}}}
			if g.AssignmentID != nil {
				t.Scope.AssignmentIDs = []uuid.UUID{*g.AssignmentID}
			} else {
				t.Scope.SpansAssignments = true
			}
			return t, nil
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in GradeIDIn) (GradeView, error) {
			g, err := rc.Q.GetGradeFull(ctx, dbq.GetGradeFullParams{ID: in.GradeID, CourseID: in.CourseID})
			if err != nil {
				return GradeView{}, err
			}
			if (g.PostedAt == nil || g.SupersededBy != nil) && !seesDrafts(rc.Member) {
				// To a student an unposted grade does not exist yet.
				return GradeView{}, apperr.Missing("no such grade in this course")
			}
			return viewGrade(g), nil
		},
	})
}

package tools

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/events"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/ids"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/members"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

func courseTools() []tool.Tool {
	return []tool.Tool{courseCreate(), courseUpdate(), courseActivate(), courseArchive(), courseSeatInstructor(), courseList(), courseGet()}
}

const (
	EventCourseCreated   = "course.created"
	EventCourseUpdated   = "course.updated"
	EventCourseActivated = "course.activated"
	EventCourseArchived  = "course.archived"
)

type CourseView struct {
	ID          uuid.UUID `json:"id"`
	DeptID      uuid.UUID `json:"dept_id"`
	TermID      uuid.UUID `json:"term_id"`
	Code        string    `json:"code"`
	Section     string    `json:"section"`
	Title       string    `json:"title"`
	Description *string   `json:"description,omitempty"`
	Status      string    `json:"status" jsonschema:"draft, active or archived; an archived course refuses every write"`
	CreatedAt   time.Time `json:"created_at"`
}

// platformCourse resolves a course for a platform tool. The input carries a
// plain course_id rather than embedding tool.InCourse, because these tools
// are gated by platform role, not by a seat in the course.
func platformCourse(ctx context.Context, q dbq.Querier, id uuid.UUID) (tool.Target, error) {
	if _, err := q.GetCourse(ctx, id); errors.Is(err, pgx.ErrNoRows) {
		return tool.Target{}, apperr.Missing("no such course")
	} else if err != nil {
		return tool.Target{}, err
	}
	return tool.Target{CourseID: id, Type: "course", ID: &id}, nil
}

type CourseCreateIn struct {
	DeptID      uuid.UUID `json:"dept_id"`
	TermID      uuid.UUID `json:"term_id"`
	Code        string    `json:"code" jsonschema:"CS101"`
	Section     string    `json:"section,omitempty" jsonschema:"A; one row is one offering, so another section is another course"`
	Title       string    `json:"title"`
	Description *string   `json:"description,omitempty"`
}

type CourseCreateOut struct {
	CourseID        uuid.UUID `json:"course_id"`
	RootComponentID uuid.UUID `json:"root_component_id" jsonschema:"the course total; the grading scheme hangs beneath it"`
}

func courseCreate() tool.Tool {
	return tool.Define(tool.Spec[CourseCreateIn, CourseCreateOut]{
		Name: "course.create",
		Description: "Create one course offering — CS101 section A, this term — as a draft, with the root of its grading " +
			"scheme. It has no members yet: seat its first instructor with course.seat_instructor, and they add the rest.",
		Kind: tool.Write, Gate: admins,
		HTTP:    tool.Route{Method: "POST", Pattern: "/v1/courses"},
		Resolve: noTarget[CourseCreateIn]("course"),
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in CourseCreateIn) (CourseCreateOut, error) {
			if strings.TrimSpace(in.Code) == "" || strings.TrimSpace(in.Title) == "" {
				return CourseCreateOut{}, apperr.Invalid("code and title are required")
			}
			if ok, err := ec.Q.DepartmentExists(ctx, in.DeptID); err != nil {
				return CourseCreateOut{}, err
			} else if !ok {
				return CourseCreateOut{}, apperr.Missing("no such department")
			}
			if ok, err := ec.Q.TermExists(ctx, in.TermID); err != nil {
				return CourseCreateOut{}, err
			} else if !ok {
				return CourseCreateOut{}, apperr.Missing("no such term")
			}
			if taken, err := ec.Q.CourseCodeTaken(ctx, dbq.CourseCodeTakenParams{TermID: in.TermID, Code: in.Code, Section: in.Section}); err != nil {
				return CourseCreateOut{}, err
			} else if taken {
				return CourseCreateOut{}, apperr.Conflicts("%s section %q already exists in that term", in.Code, in.Section)
			}
			out := CourseCreateOut{CourseID: ids.New(), RootComponentID: ids.New()}
			if err := ec.Q.InsertCourse(ctx, dbq.InsertCourseParams{
				ID: out.CourseID, DeptID: in.DeptID, TermID: in.TermID, Code: in.Code, Section: in.Section,
				Title: in.Title, Description: in.Description, CreatedByActorID: ec.Actor.ID, CreatedAt: ec.Now,
			}); err != nil {
				return CourseCreateOut{}, err
			}
			// The root is the course total. It is made with the course so
			// that there is never a course without one.
			if err := ec.Q.InsertComponent(ctx, dbq.InsertComponentParams{
				ID: out.RootComponentID, CourseID: out.CourseID, Name: "Total", Weight: one, CreatedAt: ec.Now,
			}); err != nil {
				return CourseCreateOut{}, err
			}
			ec.Emit(events.Event{Type: EventCourseCreated, CourseID: &out.CourseID, SubjectType: "course", SubjectID: &out.CourseID})
			return out, nil
		},
	})
}

type CourseUpdateIn struct {
	CourseID    uuid.UUID `json:"course_id"`
	Title       *string   `json:"title,omitempty"`
	Description *string   `json:"description,omitempty"`
}

func courseUpdate() tool.Tool {
	return tool.Define(tool.Spec[CourseUpdateIn, OK]{
		Name:        "course.update",
		Description: "Change a course's title or description. Its code, section and term are what it is, and do not change.",
		Kind:        tool.Write, Gate: admins,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}"},
		Resolve: func(ctx context.Context, q dbq.Querier, in CourseUpdateIn) (tool.Target, error) {
			return platformCourse(ctx, q, in.CourseID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in CourseUpdateIn) (OK, error) {
			c, err := ec.Q.GetCourse(ctx, in.CourseID)
			if err != nil {
				return OK{}, err
			}
			if in.Title != nil {
				if strings.TrimSpace(*in.Title) == "" {
					return OK{}, apperr.Invalid("title cannot be empty")
				}
				c.Title = *in.Title
			}
			if in.Description != nil {
				c.Description = in.Description
			}
			if err := ec.Q.UpdateCourse(ctx, dbq.UpdateCourseParams{ID: c.ID, Title: c.Title, Description: c.Description}); err != nil {
				return OK{}, err
			}
			ec.Emit(events.Event{Type: EventCourseUpdated, CourseID: &c.ID, SubjectType: "course", SubjectID: &c.ID})
			return OK{OK: true}, nil
		},
	})
}

type CourseIDIn struct {
	CourseID uuid.UUID `json:"course_id"`
}

func courseSetStatus(name, desc, path, status, event string) tool.Tool {
	return tool.Define(tool.Spec[CourseIDIn, OK]{
		Name: name, Description: desc, Kind: tool.Write, Gate: admins,
		// Changing whether a course is archived is the one write an archived
		// course accepts.
		OnArchived: true,
		HTTP:       tool.Route{Method: "POST", Pattern: path},
		Resolve: func(ctx context.Context, q dbq.Querier, in CourseIDIn) (tool.Target, error) {
			return platformCourse(ctx, q, in.CourseID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in CourseIDIn) (OK, error) {
			n, err := ec.Q.SetCourseStatus(ctx, dbq.SetCourseStatusParams{ID: in.CourseID, Status: status})
			if err != nil {
				return OK{}, err
			}
			if n == 0 {
				return OK{}, apperr.Conflicts("the course is already %s", status)
			}
			ec.Emit(events.Event{Type: event, CourseID: &in.CourseID, SubjectType: "course", SubjectID: &in.CourseID})
			return OK{OK: true}, nil
		},
	})
}

func courseActivate() tool.Tool {
	return courseSetStatus("course.activate",
		"Open a course: move it from draft (or back from archived) to active.",
		"/v1/courses/{course_id}/activate", domain.CourseActive, EventCourseActivated)
}

func courseArchive() tool.Tool {
	return courseSetStatus("course.archive",
		"Archive a course. From then on it refuses every write, from anyone, agents included; everything in it stays readable.",
		"/v1/courses/{course_id}/archive", domain.CourseArchived, EventCourseArchived)
}

type SeatInstructorIn struct {
	CourseID uuid.UUID `json:"course_id"`
	ActorID  uuid.UUID `json:"actor_id"`
}

type MemberIDOut struct {
	MemberID uuid.UUID `json:"member_id"`
}

func courseSeatInstructor() tool.Tool {
	return tool.Define(tool.Spec[SeatInstructorIn, MemberIDOut]{
		Name: "course.seat_instructor",
		Description: "Seat an actor in a course with the built-in instructor preset. This is how a course gets its first " +
			"member; from there the instructor adds everyone else with member.add.",
		Kind: tool.Write, Gate: admins,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/instructors"},
		Resolve: func(ctx context.Context, q dbq.Querier, in SeatInstructorIn) (tool.Target, error) {
			return platformCourse(ctx, q, in.CourseID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SeatInstructorIn) (MemberIDOut, error) {
			preset, err := ec.Q.GetBuiltinPresetByName(ctx, "instructor")
			if err != nil {
				return MemberIDOut{}, apperr.Precondition("the built-in instructor preset is missing; run `aishiterud seed`")
			}
			id, err := seat(ctx, ec, seating{
				courseID: in.CourseID, actorID: in.ActorID, preset: &preset, perms: presetPerms(preset),
				role: preset.Role, studentScope: preset.StudentScope, assignmentScope: preset.AssignmentScope,
			})
			return MemberIDOut{MemberID: id}, err
		},
	})
}

type CourseListIn struct {
	TermID *uuid.UUID `json:"term_id,omitempty"`
	DeptID *uuid.UUID `json:"dept_id,omitempty"`
	Page
}

type CourseListOut struct {
	Courses []CourseView `json:"courses"`
	Next    *uuid.UUID   `json:"next,omitempty"`
}

func viewCourse(c dbq.GetCourseRow) CourseView {
	return CourseView{ID: c.ID, DeptID: c.DeptID, TermID: c.TermID, Code: c.Code, Section: c.Section,
		Title: c.Title, Description: c.Description, Status: c.Status, CreatedAt: c.CreatedAt}
}

func courseList() tool.Tool {
	return tool.Define(tool.Spec[CourseListIn, CourseListOut]{
		Name:        "course.list",
		Description: "Every course on the platform, optionally by term or department. For the courses you are seated in, use me.memberships.",
		Kind:        tool.Read, Gate: admins,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/courses"},
		Resolve: noTarget[CourseListIn]("course"),
		Query: func(ctx context.Context, rc *tool.ReadCtx, in CourseListIn) (CourseListOut, error) {
			rows, err := rc.Q.ListCourses(ctx, dbq.ListCoursesParams{After: in.after(), TermID: in.TermID, DeptID: in.DeptID, MaxRows: in.limit()})
			out := CourseListOut{Courses: make([]CourseView, 0, len(rows))}
			for _, r := range rows {
				out.Courses = append(out.Courses, viewCourse(dbq.GetCourseRow(r)))
			}
			if len(rows) > 0 && len(rows) == int(in.limit()) {
				out.Next = &rows[len(rows)-1].ID
			}
			return out, err
		},
	})
}

func courseGet() tool.Tool {
	return tool.Define(tool.Spec[tool.InCourse, CourseView]{
		Name:        "course.get",
		Description: "The course itself: code, section, title, description, status. Any member who can read its material may read this.",
		Kind:        tool.Read, Gate: tool.Gate{Perms: []domain.Perm{domain.PermDocumentRead}},
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}"},
		Resolve: func(_ context.Context, _ dbq.Querier, in tool.InCourse) (tool.Target, error) {
			return tool.Target{CourseID: in.CourseID, Type: "course", ID: &in.CourseID}, nil
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in tool.InCourse) (CourseView, error) {
			c, err := rc.Q.GetCourse(ctx, in.CourseID)
			return viewCourse(c), err
		},
	})
}

// ---------------------------------------------------------------------------
// Seating a member, shared by course.seat_instructor and member.add
// ---------------------------------------------------------------------------

type seating struct {
	courseID, actorID             uuid.UUID
	preset                        *dbq.PermissionPreset
	perms                         permSet
	role                          string
	studentScope, assignmentScope string
	listedStudents                []uuid.UUID
	listedAssignments             []uuid.UUID
	expiresAt                     *time.Time
}

// listsItself reports whether the seat's student list will be the seat
// itself, as a student's is unless someone else is named.
func (s seating) listsItself() bool {
	return s.role == "student" && s.studentScope == domain.ScopeListed && len(s.listedStudents) == 0
}

// errSeated refuses a second live seat, whether the first was live all along
// or was given longer while seat() looked at it.
var errSeated = apperr.Conflicts("the actor already has a seat in this course; change it, or remove it and add again for a fresh start")

// seat adds a member: a new course_member row with the preset copied onto it.
// preset_id is kept as provenance only — nothing reads it afterwards, so
// editing the preset later changes nobody already seated.
func seat(ctx context.Context, ec *tool.ExecCtx, s seating) (uuid.UUID, error) {
	a, err := ec.Q.GetActor(ctx, s.actorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, apperr.Missing("no such actor")
	}
	if err != nil {
		return uuid.Nil, err
	}
	if a.Status != domain.ActorActive {
		return uuid.Nil, apperr.Precondition("the actor is suspended")
	}
	if a.Kind == "system" {
		return uuid.Nil, apperr.Precondition("the system actor is not seated in courses")
	}
	// A seat whose expiry has passed is removed now rather than by the next
	// sweep: it is in the way of the fresh one, and it was over anyway.
	switch live, err := ec.Q.GetLiveMembership(ctx, dbq.GetLiveMembershipParams{CourseID: s.courseID, ActorID: s.actorID}); {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return uuid.Nil, err
	case live.ExpiresAt != nil && !live.ExpiresAt.After(ec.Now):
		// Only now is the seat locked, as every removal locks the seat it
		// removes: the removal then waits for its member's calls in flight,
		// and cancels what they proposed. Then it is looked at again: the
		// sweep may have removed it meanwhile, and nothing is in the way, or
		// it may have been given longer, and it is as live as any other.
		locked, err := ec.Q.GetMemberForSweep(ctx, live.ID)
		if err != nil {
			return uuid.Nil, err
		}
		switch {
		case locked.Status == domain.MemberRemoved:
		case locked.ExpiresAt == nil || locked.ExpiresAt.After(ec.Now):
			return uuid.Nil, errSeated
		default:
			if _, err := members.Remove(ctx, ec.Q, ec.Emit, s.courseID, live.ID, members.ReasonExpired); err != nil {
				return uuid.Nil, err
			}
		}
	default:
		return uuid.Nil, errSeated
	}
	if s.expiresAt != nil && !s.expiresAt.After(ec.Now) {
		return uuid.Nil, apperr.Invalid("expires_at is in the past")
	}

	id := ids.New()
	row := dbq.InsertMemberParams{
		ID: id, CourseID: s.courseID, ActorID: s.actorID, Role: s.role, AddedByActorID: ec.Actor.ID,
		ExpiresAt: s.expiresAt, StudentScope: s.studentScope, AssignmentScope: s.assignmentScope,
		PermDocumentRead: s.perms.col(domain.PermDocumentRead), PermDocumentReadDraft: s.perms.col(domain.PermDocumentReadDraft),
		PermDocumentWrite: s.perms.col(domain.PermDocumentWrite), PermRubricRead: s.perms.col(domain.PermRubricRead),
		PermAssignmentWrite: s.perms.col(domain.PermAssignmentWrite), PermSubmissionRead: s.perms.col(domain.PermSubmissionRead),
		PermSubmissionWrite: s.perms.col(domain.PermSubmissionWrite), PermGradeRead: s.perms.col(domain.PermGradeRead),
		PermGradeSubmit: s.perms.col(domain.PermGradeSubmit), PermGradePost: s.perms.col(domain.PermGradePost),
		PermMemberRead: s.perms.col(domain.PermMemberRead), PermMemberManage: s.perms.col(domain.PermMemberManage),
		PermActionDecide: s.perms.col(domain.PermActionDecide), CreatedAt: ec.Now,
	}
	if s.preset != nil {
		row.PresetID = &s.preset.ID
	}
	if err := ec.Q.InsertMember(ctx, row); err != nil {
		return uuid.Nil, err
	}

	// A student is 'listed' with one row pointing at itself. There is no
	// self-access special case anywhere: a student sees their own work for
	// the same reason a tutor listed for them does.
	students := s.listedStudents
	if s.listsItself() {
		students = []uuid.UUID{id}
	}
	if err := writeScope(ctx, ec.Q, s.courseID, id, s.studentScope, students, s.assignmentScope, s.listedAssignments); err != nil {
		return uuid.Nil, err
	}
	ec.Emit(events.Event{
		Type: members.EventAdded, CourseID: &s.courseID, SubjectType: "course_member", SubjectID: &id,
		Payload: map[string]any{"actor_id": s.actorID, "role": s.role},
	})
	return id, nil
}

// writeScope replaces a member's listed students and assignments. Every id
// must belong to this course: a scope row naming another course's student
// would be a quiet hole in the wall between courses.
func writeScope(ctx context.Context, q *dbq.Queries, courseID, memberID uuid.UUID, studentScope string, students []uuid.UUID, assignmentScope string, assignments []uuid.UUID) error {
	if err := writeStudentScope(ctx, q, courseID, memberID, studentScope, students); err != nil {
		return err
	}
	return writeAssignmentScope(ctx, q, courseID, memberID, assignmentScope, assignments)
}

func writeStudentScope(ctx context.Context, q *dbq.Queries, courseID, memberID uuid.UUID, studentScope string, students []uuid.UUID) error {
	students = dedupe(students)
	if studentScope != domain.ScopeListed && len(students) > 0 {
		return apperr.Invalid("listed_students only makes sense with student_scope = listed")
	}
	if len(students) > 0 {
		// The member's own id is counted like any other: a new seat's row is
		// already visible in this transaction, so a student may list itself,
		// and a seat that is not a student's cannot — it would reach nothing,
		// yet be measured as reaching someone when it is next granted to.
		n, err := q.CountStudentsOfCourse(ctx, dbq.CountStudentsOfCourseParams{CourseID: courseID, MemberIds: students})
		if err != nil {
			return err
		}
		if int(n) != len(students) {
			return apperr.Precondition("listed_students must all be current students of this course")
		}
	}
	if err := q.ClearStudentScope(ctx, memberID); err != nil {
		return err
	}
	for _, s := range students {
		if err := q.AddStudentScope(ctx, dbq.AddStudentScopeParams{MemberID: memberID, StudentMemberID: s}); err != nil {
			return err
		}
	}
	return nil
}

func writeAssignmentScope(ctx context.Context, q *dbq.Queries, courseID, memberID uuid.UUID, assignmentScope string, assignments []uuid.UUID) error {
	assignments = dedupe(assignments)
	if assignmentScope != domain.ScopeListed && len(assignments) > 0 {
		return apperr.Invalid("listed_assignments only makes sense with assignment_scope = listed")
	}
	if len(assignments) > 0 {
		n, err := q.CountAssignmentsOfCourse(ctx, dbq.CountAssignmentsOfCourseParams{CourseID: courseID, AssignmentIds: assignments})
		if err != nil {
			return err
		}
		if int(n) != len(assignments) {
			return apperr.Precondition("listed_assignments must all be assignments of this course")
		}
	}
	if err := q.ClearAssignmentScope(ctx, memberID); err != nil {
		return err
	}
	for _, a := range assignments {
		if err := q.AddAssignmentScope(ctx, dbq.AddAssignmentScopeParams{MemberID: memberID, AssignmentID: a}); err != nil {
			return err
		}
	}
	return nil
}

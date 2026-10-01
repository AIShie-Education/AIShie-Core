package tools

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/authz"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/events"
	"github.com/AIShie-Education/AIShie-Core/internal/ids"
	"github.com/AIShie-Education/AIShie-Core/internal/members"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

func courseTools() []tool.Tool {
	return []tool.Tool{courseCreate(), courseUpdate(), courseActivate(), courseArchive(), courseSeatInstructor(), courseList(),
		courseMove(), courseGet(), courseUpdateDetails()}
}

// Courses are managed from outside them, as a whole, by the administrators
// gate: a platform administrator manages every course, and a department
// administrator the courses of the departments an appointment of theirs
// covers (docs/schema.md §2.10). Neither is anybody inside a course by it:
// what is done in one is done from a seat there, which an administrator who
// wants one is given as anyone is, course.seat_instructor included.

const (
	EventCourseCreated   = "course.created"
	EventCourseUpdated   = "course.updated"
	EventCourseActivated = "course.activated"
	EventCourseArchived  = "course.archived"
	EventCourseMoved     = "course.moved"
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

// platformCourse resolves a course for a tool that manages it from outside.
// The input carries a plain course_id rather than embedding tool.InCourse,
// because these tools are gated by the administrators gate, not by a seat in
// the course. The course's department is what a department administrator
// must cover.
func platformCourse(ctx context.Context, q dbq.Querier, id uuid.UUID) (tool.Target, error) {
	c, err := q.GetCourse(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return tool.Target{}, apperr.Missing("no such course")
	} else if err != nil {
		return tool.Target{}, err
	}
	return tool.Target{CourseID: id, Type: "course", ID: &id, DeptID: &c.DeptID}, nil
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
			"scheme. It has no members yet: seat its first instructor with course.seat_instructor, and they add the rest. " +
			"A department administrator creates courses in the departments they administer and beneath them.",
		Kind: tool.Write, Gate: administrators,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses"},
		Check: func(in CourseCreateIn) error {
			if strings.TrimSpace(in.Code) == "" || strings.TrimSpace(in.Title) == "" {
				return apperr.Invalid("code and title are required")
			}
			return nil
		},
		Resolve: func(ctx context.Context, q dbq.Querier, in CourseCreateIn) (tool.Target, error) {
			if _, err := findDepartment(ctx, q, in.DeptID, "no such department"); err != nil {
				return tool.Target{}, err
			}
			return tool.Target{Type: "course", DeptID: &in.DeptID}, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in CourseCreateIn) (CourseCreateOut, error) {
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
		Name: "course.update",
		Description: "Change a course's title or description. Its code, section and term are what it is, and do not change. " +
			"A department administrator does this for the courses of the departments they administer; the course's " +
			"instructors do it from their seat with course.update_details.",
		Kind: tool.Write, Gate: administrators,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}"},
		Check: func(in CourseUpdateIn) error {
			if in.Title != nil && strings.TrimSpace(*in.Title) == "" {
				return apperr.Invalid("title cannot be empty")
			}
			return nil
		},
		Resolve: func(ctx context.Context, q dbq.Querier, in CourseUpdateIn) (tool.Target, error) {
			return platformCourse(ctx, q, in.CourseID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in CourseUpdateIn) (OK, error) {
			c, err := ec.Q.GetCourse(ctx, in.CourseID)
			if err != nil {
				return OK{}, err
			}
			if in.Title != nil {
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

type CourseUpdateDetailsIn struct {
	tool.InCourse
	Title       *string `json:"title,omitempty"`
	Description *string `json:"description,omitempty"`
}

type CourseUpdateDetailsOut struct {
	Changed bool `json:"changed" jsonschema:"false when the course already said what was given: nothing was done"`
}

// courseUpdateDetails is course.update from a seat: whoever manages a course's
// members — its instructors — names and describes it. What makes the course
// the offering it is (code, section, term), where it sits (its department)
// and whether it is open stay with its administrators, from outside.
func courseUpdateDetails() tool.Tool {
	return tool.Define(tool.Spec[CourseUpdateDetailsIn, CourseUpdateDetailsOut]{
		Name: "course.update_details",
		Description: "Change the course's title or description from a seat in it, for whoever manages its members, as its " +
			"instructors do. Its code, section, term, department and status stay with its administrators (course.update, " +
			"course.move, course.activate, course.archive). Giving what the course already says changes nothing and says " +
			"so (changed: false).",
		Kind: tool.Write, Gate: tool.Gate{Perms: []domain.Perm{domain.PermMemberManage}},
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/details"},
		Check: func(in CourseUpdateDetailsIn) error {
			if in.Title == nil && in.Description == nil {
				return apperr.Invalid("give title, description or both")
			}
			if in.Title != nil && strings.TrimSpace(*in.Title) == "" {
				return apperr.Invalid("title cannot be empty")
			}
			return nil
		},
		Resolve: func(_ context.Context, _ dbq.Querier, in CourseUpdateDetailsIn) (tool.Target, error) {
			return tool.Target{CourseID: in.CourseID, Type: "course", ID: &in.CourseID}, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in CourseUpdateDetailsIn) (CourseUpdateDetailsOut, error) {
			c, err := ec.Q.LockCourseDetails(ctx, in.CourseID)
			if err != nil {
				return CourseUpdateDetailsOut{}, err
			}
			var fields []string
			if in.Title != nil && *in.Title != c.Title {
				c.Title, fields = *in.Title, append(fields, "title")
			}
			if in.Description != nil && (c.Description == nil || *in.Description != *c.Description) {
				c.Description, fields = in.Description, append(fields, "description")
			}
			if len(fields) == 0 {
				return CourseUpdateDetailsOut{}, nil
			}
			if err := ec.Q.UpdateCourse(ctx, dbq.UpdateCourseParams{ID: in.CourseID, Title: c.Title, Description: c.Description}); err != nil {
				return CourseUpdateDetailsOut{}, err
			}
			ec.Emit(events.Event{Type: EventCourseUpdated, CourseID: &in.CourseID, SubjectType: "course", SubjectID: &in.CourseID,
				Payload: map[string]any{"fields": fields}})
			return CourseUpdateDetailsOut{Changed: true}, nil
		},
	})
}

type CourseIDIn struct {
	CourseID uuid.UUID `json:"course_id"`
}

func courseSetStatus(name, desc, path, status, event string) tool.Tool {
	return tool.Define(tool.Spec[CourseIDIn, OK]{
		Name: name, Description: desc + " A department administrator does this for the courses of the departments they administer.",
		Kind: tool.Write, Gate: administrators,
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
			"member; from there the instructor adds everyone else with member.add. A department administrator does this for " +
			"the courses of the departments they administer. They find the person with actor.lookup_by_email, or register and " +
			"invite them with actor.invite_new. Administering a course gives nothing inside it: an administrator who wants " +
			"to work in one seats themselves here, and that is recorded like any seating.",
		Kind: tool.Write, Gate: administrators,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/instructors"},
		Resolve: func(ctx context.Context, q dbq.Querier, in SeatInstructorIn) (tool.Target, error) {
			return platformCourse(ctx, q, in.CourseID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SeatInstructorIn) (MemberIDOut, error) {
			preset, err := ec.Q.GetBuiltinPresetByName(ctx, "instructor")
			if err != nil {
				return MemberIDOut{}, apperr.Precondition("the built-in instructor preset is missing; run `aishie-core seed`")
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
	TermID       *uuid.UUID `json:"term_id,omitempty"`
	DeptID       *uuid.UUID `json:"dept_id,omitempty" jsonschema:"only courses directly in this department"`
	WithinDeptID *uuid.UUID `json:"within_dept_id,omitempty" jsonschema:"only courses in this department or beneath it"`
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
		Name: "course.list",
		Description: "Courses as their administrators see them, without a seat in them: every course on the platform for a " +
			"platform administrator; for a department administrator, those in the departments they administer and beneath " +
			"them. Optionally by term, by one department (dept_id), or by a department and everything beneath it " +
			"(within_dept_id). For the courses you are seated in, use me.memberships.",
		Kind: tool.Read, Gate: administrators,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses"},
		Resolve: func(context.Context, dbq.Querier, CourseListIn) (tool.Target, error) {
			return tool.Target{Type: "course", AnyDept: true}, nil
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in CourseListIn) (CourseListOut, error) {
			// A platform administrator sees every course; anyone else, the
			// courses beneath their appointments as they stand now, and a
			// course outside them is simply not there.
			rows, err := rc.Q.ListCourses(ctx, dbq.ListCoursesParams{After: in.after(), ActorID: rc.Actor.ID, Platform: rc.Admin.Platform,
				TermID: in.TermID, DeptID: in.DeptID, WithinDeptID: in.WithinDeptID, MaxRows: in.limit()})
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

type CourseMoveIn struct {
	CourseID uuid.UUID `json:"course_id"`
	DeptID   uuid.UUID `json:"dept_id" jsonschema:"the department it moves to"`
}

func courseMove() tool.Tool {
	return tool.Define(tool.Spec[CourseMoveIn, OK]{
		Name: "course.move",
		Description: "Move a course to another department. A department administrator moves courses between departments " +
			"they administer: never to one they do not, nor one that has been moved out of their reach. Its members, their " +
			"seats and everything in the course are unchanged. Who administers it changes with its department: the " +
			"administrators of the department it leaves who do not administer the one it joins stop at once, and those of " +
			"the one it joins start.",
		Kind: tool.Write, Gate: administrators,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/move"},
		Resolve: func(ctx context.Context, q dbq.Querier, in CourseMoveIn) (tool.Target, error) {
			target, err := platformCourse(ctx, q, in.CourseID)
			if err != nil {
				return target, err
			}
			if _, err := findDepartment(ctx, q, in.DeptID, "no such department to move it to"); err != nil {
				return tool.Target{}, err
			}
			return target, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in CourseMoveIn) (OK, error) {
			// The course is held first, and where it is read under that: the
			// gate looked where it was, and a move made since may have taken
			// it out of the caller's reach, when moving it on would take back
			// what is no longer theirs.
			from, err := ec.Q.LockCourseDept(ctx, in.CourseID)
			if err != nil {
				return OK{}, err
			}
			if ok, err := reaches(ctx, ec, &from); err != nil {
				return OK{}, err
			} else if !ok {
				return OK{}, movedAway()
			}
			if from == in.DeptID {
				return OK{}, apperr.Conflicts("the course is in that department already").With("reason", "same_department")
			}
			// Where it goes must be the caller's too, so that nobody moves
			// what they administer out of their own reach, or anyone else's
			// into it.
			if ok, err := reaches(ctx, ec, &in.DeptID); err != nil {
				return OK{}, err
			} else if !ok {
				return OK{}, apperr.Forbid("a course goes only to a department you administer").
					With("reason", "destination_out_of_scope")
			}
			if err := ec.Q.SetCourseDept(ctx, dbq.SetCourseDeptParams{ID: in.CourseID, DeptID: in.DeptID}); err != nil {
				return OK{}, err
			}
			ec.Emit(events.Event{Type: EventCourseMoved, CourseID: &in.CourseID, SubjectType: "course", SubjectID: &in.CourseID,
				Payload: map[string]any{"from_dept_id": from, "to_dept_id": in.DeptID}})
			return OK{OK: true}, nil
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
	// principal is set for a delegate's seat: the seat, in this course, of
	// the person who owns the actor (member.add_delegate); answersCourse
	// says it answers the course, not its principal alone.
	principal     *uuid.UUID
	answersCourse bool
	// joinLinkID is set for a seat a person takes through a join link
	// (course.join), and addedBy then is whoever made the link, whose
	// authority it is; otherwise the seat is added by whoever makes the call.
	joinLinkID *uuid.UUID
	addedBy    *uuid.UUID
	// named are the levels the call named, which seat() refuses above the
	// seat's ceilings; the rest, a preset's, it cuts down to them.
	named PermLevels
}

// listsItself reports whether the seat's student list will be the seat
// itself, as a student's is unless someone else is named.
func (s seating) listsItself() bool {
	return s.role == "student" && s.studentScope == domain.ScopeListed && len(s.listedStudents) == 0
}

// errSeated refuses a second live seat, whether the first was live all along
// or was given longer while seat() looked at it.
var errSeated = apperr.Conflicts("the actor already has a seat in this course; change it, or remove it and add again for a fresh start")

// seatedNow refuses a seat for actor while it holds a live one in the
// course that is neither past its expiry at now nor orphaned, which seat()
// would remove first: member.add asks it before a seat is proposed or given
// (Validate), and seat() as it seats the actor, where it may take the seat
// that is in the way.
func seatedNow(ctx context.Context, q dbq.Querier, courseID, actor uuid.UUID, now time.Time) error {
	live, err := q.GetLiveMembership(ctx, dbq.GetLiveMembershipParams{CourseID: courseID, ActorID: actor})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if live.ExpiresAt != nil && !live.ExpiresAt.After(now) {
		return nil
	}
	orphaned, err := q.SeatOrphaned(ctx, dbq.SeatOrphanedParams{MemberID: live.ID, Now: now})
	if err != nil || orphaned {
		return err
	}
	return errSeated
}

// seatable holds the actor a seat is for, and what the seat would hold, to
// what anyone may be seated with: an active actor that is seated at all, an
// owned agent only as its owner's delegate, and no level above what the seat
// may hold. seat() asks it of the actor it has taken; member.add's Validate
// of the actor as it stands.
func seatable(ctx context.Context, q dbq.Querier, a dbq.Actor, s seating) error {
	if a.Status != domain.ActorActive {
		return apperr.Precondition("the actor is suspended")
	}
	switch a.Kind {
	case "system":
		return apperr.Precondition("the system actor is not seated in courses")
	case "service":
		// The database refuses it too (course_member_not_a_service).
		return apperr.Precondition("a site service is not seated in courses")
	}
	// An agent someone owns acts only as its owner's delegate, and only its
	// owner seats it so. The database refuses it too.
	if s.principal == nil && a.OwnerActorID != nil {
		return apperr.Precondition("the agent belongs to someone: its owner brings it in, with member.add_delegate")
	}
	// No more than the seat may hold at all, whoever seats it: an agent
	// decides only by proposal, and a delegate holds no more than its
	// principal (domain.Ceiling). A preset's level is cut down; a level the
	// call named is refused.
	var principal *domain.Member
	if s.principal != nil {
		var err error
		if principal, err = authz.LoadMember(ctx, q, *s.principal); err != nil {
			return err
		}
	}
	return toCeilings(isAgent(a.Kind), principal, s.perms, s.named)
}

// seat adds a member: a new course_member row with the preset copied onto it.
// preset_id is kept as provenance only — nothing reads it afterwards, so
// editing the preset later changes nobody already seated.
func seat(ctx context.Context, ec *tool.ExecCtx, s seating) (uuid.UUID, error) {
	// FOR SHARE: a suspension waits for the seat to be made, or the seat
	// sees it. Whose the actor is, if anyone's, never changes.
	a, err := ec.Q.GetActorForShare(ctx, s.actorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, apperr.Missing("no such actor")
	}
	if err != nil {
		return uuid.Nil, err
	}
	if err := seatable(ctx, ec.Q, a, s); err != nil {
		return uuid.Nil, err
	}
	// A seat whose expiry has passed is removed now rather than by the next
	// sweep: it is in the way of the fresh one, and it was over anyway. So is
	// an orphaned delegate's, whose principal has gone, and a seat that no
	// longer matches its actor's owner: neither counts for anything again.
	switch live, err := ec.Q.GetLiveMembership(ctx, dbq.GetLiveMembershipParams{CourseID: s.courseID, ActorID: s.actorID}); {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return uuid.Nil, err
	default:
		expired := live.ExpiresAt != nil && !live.ExpiresAt.After(ec.Now)
		orphaned, err := ec.Q.SeatOrphaned(ctx, dbq.SeatOrphanedParams{MemberID: live.ID, Now: ec.Now})
		if err != nil {
			return uuid.Nil, err
		}
		if !expired && !orphaned {
			return uuid.Nil, errSeated
		}
		// Only now is the seat locked, as every removal locks the seat it
		// removes: the removal then waits for its member's calls in flight,
		// and cancels what they proposed. Then it is looked at again: the
		// sweep may have removed it meanwhile, and nothing is in the way, or
		// it may have been given longer, and it is as live as any other. A
		// delegate's principal is held first, as whatever locks a
		// delegate's seat takes them (holdPrincipalOf).
		if err := holdPrincipalOf(ctx, ec.Q, live.ID); err != nil {
			return uuid.Nil, err
		}
		locked, err := ec.Q.GetMemberForSweep(ctx, live.ID)
		if err != nil {
			return uuid.Nil, err
		}
		if locked.Status != domain.MemberRemoved {
			reason := members.ReasonExpired
			if locked.ExpiresAt == nil || locked.ExpiresAt.After(ec.Now) {
				// Given longer meanwhile: in the way, unless it is orphaned,
				// which nothing undoes (SeatOrphaned).
				if !orphaned {
					return uuid.Nil, errSeated
				}
				reason = members.ReasonOrphaned
			}
			if _, err := members.Remove(ctx, ec.Q, ec.Emit, s.courseID, live.ID, reason); err != nil {
				return uuid.Nil, err
			}
		}
	}
	if s.expiresAt != nil && !s.expiresAt.After(ec.Now) {
		return uuid.Nil, errExpiresInPast
	}

	id := ids.New()
	addedBy := ec.Actor.ID
	if s.addedBy != nil {
		addedBy = *s.addedBy
	}
	row := dbq.InsertMemberParams{
		ID: id, CourseID: s.courseID, ActorID: s.actorID, Role: s.role, AddedByActorID: addedBy,
		ExpiresAt: s.expiresAt, StudentScope: s.studentScope, AssignmentScope: s.assignmentScope,
		PermDocumentRead: s.perms.col(domain.PermDocumentRead), PermDocumentReadDraft: s.perms.col(domain.PermDocumentReadDraft),
		PermDocumentWrite: s.perms.col(domain.PermDocumentWrite), PermRubricRead: s.perms.col(domain.PermRubricRead),
		PermAssignmentWrite: s.perms.col(domain.PermAssignmentWrite), PermSubmissionRead: s.perms.col(domain.PermSubmissionRead),
		PermSubmissionWrite: s.perms.col(domain.PermSubmissionWrite), PermGradeRead: s.perms.col(domain.PermGradeRead),
		PermGradeSubmit: s.perms.col(domain.PermGradeSubmit), PermGradePost: s.perms.col(domain.PermGradePost),
		PermMemberRead: s.perms.col(domain.PermMemberRead), PermMemberManage: s.perms.col(domain.PermMemberManage),
		PermActionDecide: s.perms.col(domain.PermActionDecide), PermAgentDelegate: s.perms.col(domain.PermAgentDelegate),
		PermConversationAsk: s.perms.col(domain.PermConversationAsk), PermConversationAnswer: s.perms.col(domain.PermConversationAnswer),
		PermMemberInvite: s.perms.col(domain.PermMemberInvite), CreatedAt: ec.Now, PrincipalMemberID: s.principal,
		AnswersCourse: s.answersCourse, JoinLinkID: s.joinLinkID,
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
	payload := map[string]any{"actor_id": s.actorID, "role": s.role}
	if s.principal != nil {
		payload["delegate"], payload["principal_member_id"], payload["answers_course"] = true, *s.principal, s.answersCourse
	}
	if s.joinLinkID != nil {
		payload["via"], payload["join_link_id"] = "join_link", *s.joinLinkID
	}
	ec.Emit(events.Event{
		Type: members.EventAdded, CourseID: &s.courseID, SubjectType: "course_member", SubjectID: &id, Payload: payload,
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
	if err := checkStudentList(ctx, q, courseID, studentScope, students); err != nil {
		return err
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
	if err := checkAssignmentList(ctx, q, courseID, assignmentScope, assignments); err != nil {
		return err
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

// checkStudentList holds a seat's student list to its scope and its course:
// a list only for a scope that is one, of the course's current students.
// writeStudentScope asks it as it writes the list, and a Validate before
// the change is made or proposed.
func checkStudentList(ctx context.Context, q dbq.Querier, courseID uuid.UUID, studentScope string, students []uuid.UUID) error {
	students = dedupe(students)
	if studentScope != domain.ScopeListed && len(students) > 0 {
		return errStudentList
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
	return nil
}

// checkAssignmentList is checkStudentList for a seat's assignment list.
func checkAssignmentList(ctx context.Context, q dbq.Querier, courseID uuid.UUID, assignmentScope string, assignments []uuid.UUID) error {
	assignments = dedupe(assignments)
	if assignmentScope != domain.ScopeListed && len(assignments) > 0 {
		return errAssignmentList
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
	return nil
}

// A list given with a scope that is not one, refused whether the call names
// the scope (Check) or the seat or its preset says it (Validate, Execute).
var (
	errStudentList    = apperr.Invalid("listed_students only makes sense with student_scope = listed")
	errAssignmentList = apperr.Invalid("listed_assignments only makes sense with assignment_scope = listed")
)

// checkListsGiven refuses a list given with a scope, also given, that is not
// a list: what the arguments say alone, for a Check.
func checkListsGiven(studentScope *string, students []uuid.UUID, assignmentScope *string, assignments []uuid.UUID) error {
	if studentScope != nil && *studentScope != domain.ScopeListed && len(students) > 0 {
		return errStudentList
	}
	if assignmentScope != nil && *assignmentScope != domain.ScopeListed && len(assignments) > 0 {
		return errAssignmentList
	}
	return nil
}

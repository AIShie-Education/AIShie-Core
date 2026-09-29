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
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

// Departments form a tree, and each may have administrators (docs/schema.md
// §2.10). An appointment reaches the department and everything beneath it.
// What is beneath an appointment is its holder's to shape and staff: they
// rename and move every department strictly beneath it, and appoint and
// remove its administrators, never their own department or one above, so
// nobody widens their own reach, removes a fellow administrator, or removes
// whoever is above them. A department at the top of the tree is a platform
// administrator's alone. Nothing here reaches inside a course.

func departmentTools() []tool.Tool {
	return []tool.Tool{
		departmentCreate(), departmentUpdate(), departmentMove(), departmentList(), departmentListTree(),
		departmentAddAdmin(), departmentRemoveAdmin(), departmentListAdmins(),
	}
}

// Every department event is a platform event: in no course's feed.
const (
	EventDepartmentCreated      = "department.created"
	EventDepartmentUpdated      = "department.updated"
	EventDepartmentMoved        = "department.moved"
	EventDepartmentAdminAdded   = "department.admin_added"
	EventDepartmentAdminRemoved = "department.admin_removed"
)

// findDepartment is a department that must exist, or not_found saying so as
// missing does.
func findDepartment(ctx context.Context, q dbq.Querier, id uuid.UUID, missing string) (dbq.GetDepartmentRow, error) {
	d, err := q.GetDepartment(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, apperr.Missing("%s", missing)
	}
	return d, err
}

// departmentName is a department's name as it is kept: trimmed, and not
// empty.
func departmentName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", apperr.Invalid("name is required")
	}
	return name, nil
}

// nameFree refuses a name another department under the same parent (at the
// top, for nil) already has, in any case. id is the department being named;
// uuid.Nil for a new one.
func nameFree(ctx context.Context, q dbq.Querier, parent *uuid.UUID, name string, id uuid.UUID) error {
	taken, err := q.SiblingNameTaken(ctx, dbq.SiblingNameTakenParams{ParentID: parent, Name: name, ID: id})
	if err != nil {
		return err
	}
	if taken {
		return apperr.Conflicts("another department there is already called %q", name).With("reason", "name_taken")
	}
	return nil
}

// reshaping is the target of a change to a department itself, its name or
// its place in the tree: as with its administrators (staffing), whoever
// administers the department above it may make it, so nobody reshapes their
// own appointment's department or one above it.
func reshaping(ctx context.Context, q dbq.Querier, dept uuid.UUID) (tool.Target, error) {
	d, err := findDepartment(ctx, q, dept, "no such department")
	if err != nil {
		return tool.Target{}, err
	}
	return tool.Target{Type: "department", ID: &d.ID, DeptID: d.ParentID}, nil
}

// reaches reports whether the caller of an Admin-gated write may act beneath
// parent: a department, or the top of the tree for nil. A platform
// administrator may anywhere; a department administrator where an
// appointment of theirs is at parent or above it, which is then held FOR
// SHARE to the end of the call (authz.AdminScope.Covers).
func reaches(ctx context.Context, ec *tool.ExecCtx, parent *uuid.UUID) (bool, error) {
	if parent == nil {
		return ec.Admin.Platform, nil
	}
	_, ok, err := ec.Admin.Covers(ctx, ec.Q, *parent)
	return ok, err
}

// movedAway refuses a change to what the gate let the caller act on, when
// it has been moved out of their reach since: the gate looked at where it
// was, and the change is to where it is.
func movedAway() error {
	return apperr.Forbid("it has been moved meanwhile, out of the departments you administer").
		With("reason", "department_out_of_scope")
}

// ---------------------------------------------------------------------------
// The tree
// ---------------------------------------------------------------------------

type DepartmentCreateIn struct {
	Name     string     `json:"name"`
	ParentID *uuid.UUID `json:"parent_id,omitempty" jsonschema:"the department it goes under; absent for one at the top of the tree, which only a platform administrator makes"`
}

func departmentCreate() tool.Tool {
	return tool.Define(tool.Spec[DepartmentCreateIn, IDOut]{
		Name: "department.create",
		Description: "Create a department, at the top of the tree or under another. A department administrator creates " +
			"departments under any department they administer, at any depth; one at the top is a platform administrator's. " +
			"The tree is at most 8 levels deep, and sibling names are unique in any case. The administrators of a department " +
			"administer every department beneath it too, and the courses in all of them. Departments group courses and may " +
			"define their own permission presets.",
		Kind: tool.Write, Gate: administrators,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/departments"},
		Resolve: func(ctx context.Context, q dbq.Querier, in DepartmentCreateIn) (tool.Target, error) {
			if in.ParentID != nil {
				if _, err := findDepartment(ctx, q, *in.ParentID, "no such department to put it under"); err != nil {
					return tool.Target{}, err
				}
			}
			return tool.Target{Type: "department", DeptID: in.ParentID}, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in DepartmentCreateIn) (IDOut, error) {
			name, err := departmentName(in.Name)
			if err != nil {
				return IDOut{}, err
			}
			// The tree lock first: nothing changes the tree's shape between
			// measuring it and adding to it. The trigger measures again.
			if err := ec.Q.LockDepartmentTree(ctx); err != nil {
				return IDOut{}, err
			}
			if in.ParentID != nil {
				depth, err := ec.Q.DepartmentDepth(ctx, *in.ParentID)
				if err != nil {
					return IDOut{}, err
				}
				if int(depth)+1 > domain.MaxDepartmentDepth {
					return IDOut{}, apperr.Precondition("departments nest at most %d levels deep", domain.MaxDepartmentDepth).
						With("reason", "too_deep").With("max_depth", domain.MaxDepartmentDepth)
				}
			}
			if err := nameFree(ctx, ec.Q, in.ParentID, name, uuid.Nil); err != nil {
				return IDOut{}, err
			}
			id := ids.New()
			if err := ec.Q.InsertDepartment(ctx, dbq.InsertDepartmentParams{ID: id, Name: name, ParentID: in.ParentID, CreatedAt: ec.Now}); err != nil {
				return IDOut{}, err
			}
			ec.Emit(events.Event{Type: EventDepartmentCreated, SubjectType: "department", SubjectID: &id,
				Payload: map[string]any{"parent_id": in.ParentID}})
			return IDOut{ID: id}, nil
		},
	})
}

type DepartmentUpdateIn struct {
	DeptID uuid.UUID `json:"dept_id"`
	Name   string    `json:"name"`
}

func departmentUpdate() tool.Tool {
	return tool.Define(tool.Spec[DepartmentUpdateIn, DepartmentView]{
		Name: "department.update",
		Description: "Rename a department. Whoever administers the department above it does this, or a platform " +
			"administrator; a department's own administrators do not rename it, and one at the top of the tree is a " +
			"platform administrator's. Sibling names are unique in any case. Nothing else about it changes.",
		Kind: tool.Write, Gate: administrators,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/departments/{dept_id}"},
		Resolve: func(ctx context.Context, q dbq.Querier, in DepartmentUpdateIn) (tool.Target, error) {
			return reshaping(ctx, q, in.DeptID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in DepartmentUpdateIn) (DepartmentView, error) {
			name, err := departmentName(in.Name)
			if err != nil {
				return DepartmentView{}, err
			}
			// The tree lock, as create and move take it: a name is measured
			// against its siblings' where the department is now, and two
			// names given at once are measured against each other.
			if err := ec.Q.LockDepartmentTree(ctx); err != nil {
				return DepartmentView{}, err
			}
			d, err := findDepartment(ctx, ec.Q, in.DeptID, "no such department")
			if err != nil {
				return DepartmentView{}, err
			}
			if ok, err := reaches(ctx, ec, d.ParentID); err != nil {
				return DepartmentView{}, err
			} else if !ok {
				return DepartmentView{}, movedAway()
			}
			if d.Name == name {
				return DepartmentView{}, apperr.Conflicts("it already has that name").With("reason", "same_name")
			}
			if err := nameFree(ctx, ec.Q, d.ParentID, name, d.ID); err != nil {
				return DepartmentView{}, err
			}
			if err := ec.Q.RenameDepartment(ctx, dbq.RenameDepartmentParams{ID: d.ID, Name: name}); err != nil {
				return DepartmentView{}, err
			}
			ec.Emit(events.Event{Type: EventDepartmentUpdated, SubjectType: "department", SubjectID: &d.ID})
			return DepartmentView{ID: d.ID, Name: name, ParentID: d.ParentID}, nil
		},
	})
}

type DepartmentMoveIn struct {
	DeptID   uuid.UUID  `json:"dept_id"`
	ParentID *uuid.UUID `json:"parent_id" jsonschema:"where it goes: a department, or null for the top of the tree"`
}

func departmentMove() tool.Tool {
	return tool.Define(tool.Spec[DepartmentMoveIn, OK]{
		Name: "department.move",
		Description: "Move a department, with everything beneath it, under another department or to the top of the tree. " +
			"A department administrator moves the departments beneath the ones they administer, only to a department they " +
			"administer as well, and never to the top, which is a platform administrator's. The departments and courses " +
			"beneath it go with it, and so does who administers them: whoever administered them only through a department " +
			"that is no longer above them stops at once, and whoever administers the department it joins starts. Nothing " +
			"inside its courses changes. A department never goes under itself or one beneath it, the tree is at most 8 " +
			"levels deep, and sibling names are unique in any case.",
		Kind: tool.Write, Gate: administrators,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/departments/{dept_id}/move"},
		Resolve: func(ctx context.Context, q dbq.Querier, in DepartmentMoveIn) (tool.Target, error) {
			target, err := reshaping(ctx, q, in.DeptID)
			if err != nil {
				return target, err
			}
			if in.ParentID != nil {
				if _, err := findDepartment(ctx, q, *in.ParentID, "no such department to move it under"); err != nil {
					return tool.Target{}, err
				}
			}
			return target, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in DepartmentMoveIn) (OK, error) {
			// The tree lock first: the tree measured below is the tree this
			// move changes, whoever else is moving a department. The trigger
			// measures again. Where the department is now is read under it,
			// and must still be the caller's: moved since the gate looked,
			// taking it from there would move what is no longer theirs.
			if err := ec.Q.LockDepartmentTree(ctx); err != nil {
				return OK{}, err
			}
			d, err := findDepartment(ctx, ec.Q, in.DeptID, "no such department")
			if err != nil {
				return OK{}, err
			}
			if ok, err := reaches(ctx, ec, d.ParentID); err != nil {
				return OK{}, err
			} else if !ok {
				return OK{}, movedAway()
			}
			if (d.ParentID == nil && in.ParentID == nil) || (d.ParentID != nil && in.ParentID != nil && *d.ParentID == *in.ParentID) {
				return OK{}, apperr.Conflicts("it is there already").With("reason", "same_parent")
			}
			// Where it goes must be the caller's too, so that nobody moves
			// what they administer out of their own reach, or anyone else's
			// into it.
			if ok, err := reaches(ctx, ec, in.ParentID); err != nil {
				return OK{}, err
			} else if !ok {
				return OK{}, apperr.Forbid("a department goes only under one you administer; the top of the tree is a platform administrator's").
					With("reason", "destination_out_of_scope")
			}
			if in.ParentID != nil {
				inside, err := ec.Q.InSubtree(ctx, dbq.InSubtreeParams{DeptID: d.ID, OtherID: *in.ParentID})
				if err != nil {
					return OK{}, err
				}
				if inside {
					return OK{}, apperr.Precondition("a department cannot go under itself or under a department beneath it").
						With("reason", "cycle")
				}
				depth, err := ec.Q.DepartmentDepth(ctx, *in.ParentID)
				if err != nil {
					return OK{}, err
				}
				height, err := ec.Q.SubtreeHeight(ctx, d.ID)
				if err != nil {
					return OK{}, err
				}
				if int(depth)+int(height) > domain.MaxDepartmentDepth {
					return OK{}, apperr.Precondition("departments nest at most %d levels deep", domain.MaxDepartmentDepth).
						With("reason", "too_deep").With("max_depth", domain.MaxDepartmentDepth)
				}
			}
			if err := nameFree(ctx, ec.Q, in.ParentID, d.Name, d.ID); err != nil {
				return OK{}, err
			}
			// department_tree_valid refuses a cycle or a tree too deep here,
			// should anything have changed the tree without the lock.
			if err := ec.Q.SetDepartmentParent(ctx, dbq.SetDepartmentParentParams{ID: d.ID, ParentID: in.ParentID}); err != nil {
				return OK{}, err
			}
			ec.Emit(events.Event{Type: EventDepartmentMoved, SubjectType: "department", SubjectID: &d.ID,
				Payload: map[string]any{"from_parent_id": d.ParentID, "to_parent_id": in.ParentID}})
			return OK{OK: true}, nil
		},
	})
}

type DepartmentView struct {
	ID       uuid.UUID  `json:"id"`
	Name     string     `json:"name"`
	ParentID *uuid.UUID `json:"parent_id,omitempty" jsonschema:"the department it is under; absent for one at the top of the tree"`
}

type DepartmentListOut struct {
	Departments []DepartmentView `json:"departments"`
}

func departmentList() tool.Tool {
	return tool.Define(tool.Spec[Empty, DepartmentListOut]{
		Name: "department.list",
		Description: "Every department, by name, with the department each is under; department.list_tree gives them as a tree. " +
			"Any signed-in actor may read this.",
		Kind: tool.Read, Gate: self,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/departments"},
		Resolve: noTarget[Empty]("department"),
		Query: func(ctx context.Context, rc *tool.ReadCtx, _ Empty) (DepartmentListOut, error) {
			rows, err := rc.Q.ListDepartments(ctx)
			out := DepartmentListOut{Departments: make([]DepartmentView, 0, len(rows))}
			for _, r := range rows {
				out.Departments = append(out.Departments, DepartmentView{ID: r.ID, Name: r.Name, ParentID: r.ParentID})
			}
			return out, err
		},
	})
}

type DepartmentTreeIn struct {
	RootID *uuid.UUID `json:"root_id,omitempty" jsonschema:"only this department and what is beneath it"`
}

type DepartmentNode struct {
	ID          uuid.UUID  `json:"id"`
	Name        string     `json:"name"`
	ParentID    *uuid.UUID `json:"parent_id,omitempty"`
	Depth       int        `json:"depth" jsonschema:"1 at the top of the tree"`
	Administers bool       `json:"administers" jsonschema:"you administer it, and its courses are yours to manage: an appointment of yours is at it or above it, or you are a platform administrator"`
	Manages     bool       `json:"manages" jsonschema:"you may rename it, move it and appoint or remove its administrators: an appointment of yours is above it, or you are a platform administrator"`
	Appointed   bool       `json:"appointed" jsonschema:"you are appointed at this department itself"`
	CourseCount *int       `json:"course_count,omitempty" jsonschema:"courses directly in it, archived ones included; only where you administer it"`
	AdminCount  *int       `json:"admin_count,omitempty" jsonschema:"its own administrators; only where you administer it"`
}

type DepartmentTreeOut struct {
	Departments []DepartmentNode `json:"departments"`
	MaxDepth    int              `json:"max_depth" jsonschema:"how deep the tree may be"`
}

func departmentListTree() tool.Tool {
	return tool.Define(tool.Spec[DepartmentTreeIn, DepartmentTreeOut]{
		Name: "department.list_tree",
		Description: "The departments as a tree, each before those beneath it, siblings by name, with what you may do with " +
			"each: whether you administer it (its courses are yours to manage), and whether you may rename it, move it and " +
			"appoint and remove its administrators (an appointment of yours is above it). Course and administrator counts " +
			"are given where you administer. Any signed-in actor may read this; to someone who administers nothing, every " +
			"flag is false.",
		Kind: tool.Read, Gate: self,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/departments/tree"},
		Resolve: func(ctx context.Context, q dbq.Querier, in DepartmentTreeIn) (tool.Target, error) {
			if in.RootID != nil {
				if _, err := findDepartment(ctx, q, *in.RootID, "no such department"); err != nil {
					return tool.Target{}, err
				}
			}
			return tool.Target{Type: "department", ID: in.RootID}, nil
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in DepartmentTreeIn) (DepartmentTreeOut, error) {
			rows, err := rc.Q.DepartmentTree(ctx, in.RootID)
			if err != nil {
				return DepartmentTreeOut{}, err
			}
			// The names are everyone's, as department.list gives them; what
			// the caller may do with each is theirs alone.
			platform := authz.Platform(rc.Actor, domain.PlatformRoot, domain.PlatformAdmin).Level.Allowed()
			covered := map[uuid.UUID]dbq.AdministeredDepartmentsRow{}
			if rc.Actor.Administers {
				rs, err := rc.Q.AdministeredDepartments(ctx, rc.Actor.ID)
				if err != nil {
					return DepartmentTreeOut{}, err
				}
				for _, r := range rs {
					covered[r.ID] = r
				}
			}
			courses, admins := map[uuid.UUID]int{}, map[uuid.UUID]int{}
			if platform || len(covered) > 0 {
				cs, err := rc.Q.CourseCountsByDept(ctx)
				if err != nil {
					return DepartmentTreeOut{}, err
				}
				for _, c := range cs {
					courses[c.DeptID] = int(c.Courses)
				}
				as, err := rc.Q.AdminCountsByDept(ctx)
				if err != nil {
					return DepartmentTreeOut{}, err
				}
				for _, a := range as {
					admins[a.DeptID] = int(a.Admins)
				}
			}
			out := DepartmentTreeOut{Departments: make([]DepartmentNode, 0, len(rows)), MaxDepth: domain.MaxDepartmentDepth}
			for _, r := range rows {
				c, administers := covered[r.ID]
				n := DepartmentNode{ID: r.ID, Name: r.Name, ParentID: r.ParentID, Depth: int(r.Depth),
					Administers: platform || administers, Manages: platform || c.Strictly, Appointed: c.Appointed}
				if n.Administers {
					nc, na := courses[r.ID], admins[r.ID]
					n.CourseCount, n.AdminCount = &nc, &na
				}
				out.Departments = append(out.Departments, n)
			}
			return out, nil
		},
	})
}

// ---------------------------------------------------------------------------
// Administrators
// ---------------------------------------------------------------------------

type DepartmentAdminIn struct {
	DeptID  uuid.UUID `json:"dept_id"`
	ActorID uuid.UUID `json:"actor_id" jsonschema:"the person: actor.lookup_by_email finds them by their whole email"`
}

// staffing is the target of a change to a department's administrators: whoever
// administers the department above it may make it, so the appointment
// relied on is one above the department, never at it (docs/schema.md §2.10).
func staffing(ctx context.Context, q dbq.Querier, dept uuid.UUID) (tool.Target, error) {
	d, err := findDepartment(ctx, q, dept, "no such department")
	if err != nil {
		return tool.Target{}, err
	}
	return tool.Target{Type: "department_admin", DeptID: d.ParentID}, nil
}

type DepartmentAddAdminOut struct {
	AppointmentID uuid.UUID `json:"appointment_id"`
	CoveredAbove  bool      `json:"covered_above" jsonschema:"they already administer it through a department above; this appointment keeps it theirs if that one ends"`
}

func departmentAddAdmin() tool.Tool {
	return tool.Define(tool.Spec[DepartmentAdminIn, DepartmentAddAdminOut]{
		Name: "department.add_admin",
		Description: "Make a person an administrator of a department. They then administer it and every department beneath " +
			"it: they create, change, open, archive, list and move the courses there and seat their instructors, as a " +
			"platform administrator does; they find people by their whole email and invite new ones; and they create " +
			"departments there, and rename and move those beneath it and appoint their administrators. An administrator of " +
			"a department appoints the administrators of the departments beneath it, never of their own or of one above; a " +
			"platform administrator appoints anywhere, and alone at the top of the tree. Only a person can be one, never an " +
			"agent, and nobody appoints themselves. An appointment gives no seat in any course, and nothing inside one: an " +
			"administrator who wants to work in a course is seated there like anyone else.",
		Kind: tool.Write, Gate: administrators,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/departments/{dept_id}/admins"},
		Resolve: func(ctx context.Context, q dbq.Querier, in DepartmentAdminIn) (tool.Target, error) {
			target, err := staffing(ctx, q, in.DeptID)
			if err != nil {
				return target, err
			}
			if _, err := q.GetActor(ctx, in.ActorID); errors.Is(err, pgx.ErrNoRows) {
				return tool.Target{}, apperr.Missing("no such actor")
			} else if err != nil {
				return tool.Target{}, err
			}
			return target, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in DepartmentAdminIn) (DepartmentAddAdminOut, error) {
			if in.ActorID == ec.Actor.ID {
				return DepartmentAddAdminOut{}, apperr.Forbid("nobody appoints themselves").With("reason", "self_appointment")
			}
			// Held so that a suspension waits for the appointment, or the
			// appointment sees it. The database refuses anyone but a person
			// whatever happens here; this says so in words.
			a, err := ec.Q.GetActorForShare(ctx, in.ActorID)
			if err != nil {
				return DepartmentAddAdminOut{}, err
			}
			switch {
			case a.Kind != "human":
				return DepartmentAddAdminOut{}, apperr.Precondition("only a person administers a department, never an agent").
					With("reason", "not_a_person")
			case a.Status != domain.ActorActive:
				return DepartmentAddAdminOut{}, apperr.Precondition("they are suspended: reactivate them first").
					With("reason", "actor_suspended")
			}
			switch _, err := ec.Q.LiveAppointment(ctx, dbq.LiveAppointmentParams{DeptID: in.DeptID, ActorID: in.ActorID}); {
			case err == nil:
				return DepartmentAddAdminOut{}, apperr.Conflicts("they administer this department already").With("reason", "already_admin")
			case !errors.Is(err, pgx.ErrNoRows):
				return DepartmentAddAdminOut{}, err
			}
			dept, err := ec.Q.GetDepartment(ctx, in.DeptID)
			if err != nil {
				return DepartmentAddAdminOut{}, err
			}
			out := DepartmentAddAdminOut{AppointmentID: ids.New()}
			if dept.ParentID != nil {
				switch _, err := ec.Q.DepartmentAuthority(ctx, dbq.DepartmentAuthorityParams{ActorID: in.ActorID, DeptID: *dept.ParentID}); {
				case err == nil:
					out.CoveredAbove = true
				case !errors.Is(err, pgx.ErrNoRows):
					return DepartmentAddAdminOut{}, err
				}
			}
			// Two appointments of one person at one department made at once
			// meet at department_admin_one_live, and the second is a conflict.
			if err := ec.Q.InsertAppointment(ctx, dbq.InsertAppointmentParams{
				ID: out.AppointmentID, DeptID: in.DeptID, ActorID: in.ActorID, AppointedByActorID: ec.Actor.ID, AppointedAt: ec.Now,
			}); err != nil {
				return DepartmentAddAdminOut{}, err
			}
			ec.Emit(events.Event{Type: EventDepartmentAdminAdded, SubjectType: "department", SubjectID: &in.DeptID,
				Payload: map[string]any{"actor_id": in.ActorID, "appointment_id": out.AppointmentID}})
			return out, nil
		},
	})
}

func departmentRemoveAdmin() tool.Tool {
	return tool.Define(tool.Spec[DepartmentAdminIn, OK]{
		Name: "department.remove_admin",
		Description: "End a person's appointment as administrator of a department. It takes effect at once: a call of " +
			"theirs that relies on it and is in flight finishes first, and their next is decided without it. The appointment " +
			"stays on record, with who ended it and when. If they administer the department through another appointment, " +
			"above it, they keep that. Whoever may appoint a department's administrators ends their appointments: a " +
			"department's administrators never end one another's, nor that of anyone above them.",
		Kind: tool.Write, Gate: administrators,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/departments/{dept_id}/admins/{actor_id}/remove"},
		Resolve: func(ctx context.Context, q dbq.Querier, in DepartmentAdminIn) (tool.Target, error) {
			return staffing(ctx, q, in.DeptID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in DepartmentAdminIn) (OK, error) {
			a, err := ec.Q.LiveAppointment(ctx, dbq.LiveAppointmentParams{DeptID: in.DeptID, ActorID: in.ActorID})
			if errors.Is(err, pgx.ErrNoRows) {
				return OK{}, apperr.Missing("they do not administer this department").With("reason", "not_admin")
			} else if err != nil {
				return OK{}, err
			}
			// An UPDATE: it waits for every write of theirs that holds the
			// appointment (LockDepartmentAuthority), and every call after it
			// finds none.
			n, err := ec.Q.EndAppointment(ctx, dbq.EndAppointmentParams{ID: a.ID, RemovedAt: ec.Now, RemovedByActorID: ec.Actor.ID})
			if err != nil {
				return OK{}, err
			}
			if n == 0 {
				return OK{}, apperr.Conflicts("the appointment was ended by someone else just now")
			}
			ec.Emit(events.Event{Type: EventDepartmentAdminRemoved, SubjectType: "department", SubjectID: &in.DeptID,
				Payload: map[string]any{"actor_id": in.ActorID, "appointment_id": a.ID}})
			return OK{OK: true}, nil
		},
	})
}

type DepartmentAdminsIn struct {
	DeptID         uuid.UUID `json:"dept_id"`
	Inherited      bool      `json:"inherited,omitempty" jsonschema:"also the administrators of every department above it, who administer it too"`
	IncludeRemoved bool      `json:"include_removed,omitempty" jsonschema:"also the appointments that have ended"`
}

type AppointmentView struct {
	ID                 uuid.UUID  `json:"id"`
	DeptID             uuid.UUID  `json:"dept_id"`
	DeptName           string     `json:"dept_name"`
	ActorID            uuid.UUID  `json:"actor_id"`
	DisplayName        string     `json:"display_name"`
	AppointedByActorID uuid.UUID  `json:"appointed_by_actor_id"`
	AppointedByName    string     `json:"appointed_by_name"`
	AppointedAt        time.Time  `json:"appointed_at"`
	RemovedAt          *time.Time `json:"removed_at,omitempty"`
	RemovedByActorID   *uuid.UUID `json:"removed_by_actor_id,omitempty"`
	RemovedByName      *string    `json:"removed_by_name,omitempty"`
}

type DepartmentAdminsOut struct {
	Admins []AppointmentView `json:"admins" jsonschema:"nearest department first, then by name"`
}

func departmentListAdmins() tool.Tool {
	return tool.Define(tool.Spec[DepartmentAdminsIn, DepartmentAdminsOut]{
		Name: "department.list_admins",
		Description: "Who administers a department: its own administrators, and with inherited, those of every department " +
			"above it, who administer it too. With include_removed, past appointments as well. Each says who appointed " +
			"them and when, and for a past one, who ended it and when. For a department's administrators, those above " +
			"them, and platform administrators.",
		Kind: tool.Read, Gate: administrators,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/departments/{dept_id}/admins"},
		Resolve: func(ctx context.Context, q dbq.Querier, in DepartmentAdminsIn) (tool.Target, error) {
			if _, err := findDepartment(ctx, q, in.DeptID, "no such department"); err != nil {
				return tool.Target{}, err
			}
			return tool.Target{Type: "department", ID: &in.DeptID, DeptID: &in.DeptID}, nil
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in DepartmentAdminsIn) (DepartmentAdminsOut, error) {
			rows, err := rc.Q.ListAppointments(ctx, dbq.ListAppointmentsParams{
				DeptID: in.DeptID, Inherited: in.Inherited, IncludeRemoved: in.IncludeRemoved})
			out := DepartmentAdminsOut{Admins: make([]AppointmentView, 0, len(rows))}
			for _, r := range rows {
				out.Admins = append(out.Admins, AppointmentView{
					ID: r.ID, DeptID: r.DeptID, DeptName: r.DeptName, ActorID: r.ActorID, DisplayName: r.DisplayName,
					AppointedByActorID: r.AppointedByActorID, AppointedByName: r.AppointedByName, AppointedAt: r.AppointedAt,
					RemovedAt: r.RemovedAt, RemovedByActorID: r.RemovedByActorID, RemovedByName: r.RemovedByName,
				})
			}
			return out, err
		},
	})
}

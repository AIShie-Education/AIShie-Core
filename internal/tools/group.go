package tools

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/authz"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/events"
	"github.com/AIShie-Education/AIShie-Core/internal/groupsplit"
	"github.com/AIShie-Education/AIShie-Core/internal/ids"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

// Group sets, groups, and forming them (docs/schema.md §2.5a, Groups). A
// course keeps reusable sets of groups; a group assignment names one. Groups
// are formed three ways: by hand (group.set_members), by a random split and
// then by hand (group.split), and by students signing themselves up to the
// groups of a set the teacher opened (group.sign_up). Memberships are
// history: a stay is ended, never deleted, saying by whom and how.

func groupTools() []tool.Tool {
	return []tool.Tool{groupSetList(), groupSetGet(), groupSetCreate(), groupSetUpdate(), groupCreate(), groupUpdate(),
		groupSetMembers(), groupSplit(), groupSignUp()}
}

// Forming groups is gated by perm_assignment_write: groups exist for group
// work and are set up by whoever sets it, and a group grants nothing.
var formGroups = tool.Gate{Perms: []domain.Perm{domain.PermAssignmentWrite}}

const (
	EventGroupSetCreated    = "group_set.created"
	EventGroupSetUpdated    = "group_set.updated"
	EventGroupCreated       = "group.created"
	EventGroupUpdated       = "group.updated"
	EventGroupMemberAdded   = "group.member_added"
	EventGroupMemberRemoved = "group.member_removed"
)

// Why a call about groups, or group work, is refused, in error.details.reason.
const (
	ReasonNoGroup                = "no_group"
	ReasonNotAGroupAssignment    = "not_a_group_assignment"
	ReasonGroupAssignment        = "group_assignment"
	ReasonAssignmentHasWork      = "assignment_has_work"
	ReasonSetArchived            = "set_archived"
	ReasonGroupArchived          = "group_archived"
	ReasonSignupClosed           = "signup_closed"
	ReasonGroupFull              = "group_full"
	ReasonGroupHasWork           = "group_has_work"
	ReasonYourGroupHasWork       = "your_group_has_work"
	ReasonNotAStudent            = "not_a_student"
	ReasonGroupNotEmpty          = "group_not_empty"
	ReasonNoRoom                 = "no_room"
	ReasonGroupEmpty             = "group_empty"
	ReasonPartOfOtherWork        = "part_of_other_work"
	ReasonMemberGraded           = "member_graded"
	ReasonNotFromAGroupGrade     = "not_from_a_group_grade"
	ReasonNotAMemberOfWork       = "not_a_member_of_work"
	ReasonAdjustedBelowZero      = "adjusted_below_zero"
	ReasonAdjustedAbovePoints    = "adjusted_above_points"
	ReasonGroupGradePartlyPosted = "group_grade_partly_posted"
	ReasonDraftChanged           = "draft_changed"
	ReasonMembersChanged         = "members_changed"
	ReasonBaseRevisionRequired   = "base_revision_required"
	ReasonBadAdjustment          = "bad_adjustment"
	ReasonBadSplit               = "bad_split"
	ReasonNameTaken              = "name_taken"
)

// How a stay in a group began, and how it ended.
const (
	joinedAssigned, joinedSplit, joinedSignup                    = "assigned", "split", "signup"
	leftMoved, leftUnassigned, leftSplit, leftLeft, leftSwitched = "moved", "unassigned", "split", "left", "switched"
)

const (
	maxGroupsPerCall     = 100
	maxPlacementsPerCall = 500
	maxGroupSize         = 500
	maxGroupName         = 100
	maxSetDescription    = 2000
	defaultSplitPrefix   = "Group "
	// subjectGroup is the subject_type of a group's events and the
	// target_type of its actions.
	subjectGroup = "group"
)

// selfOf is whom a seat acts as for its own work: its principal, for a
// delegate — a student's own agent acts for the student — and itself
// otherwise.
func selfOf(m *domain.Member) uuid.UUID {
	if m.PrincipalID != nil {
		return *m.PrincipalID
	}
	return m.ID
}

// reached says which of ids a reader's student scope reaches, a delegate's
// principal's as well, as authorize() step 4 would for each.
func reached(ctx context.Context, q dbq.Querier, f authz.ScopeFilter, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	listed := func(member uuid.UUID) (map[uuid.UUID]bool, error) {
		rows, err := q.StudentsInListedScope(ctx, dbq.StudentsInListedScopeParams{MemberID: member, Ids: ids})
		out := make(map[uuid.UUID]bool, len(rows))
		for _, id := range rows {
			out[id] = true
		}
		return out, err
	}
	var own, principal map[uuid.UUID]bool
	var err error
	if !f.StudentAll {
		if own, err = listed(f.MemberID); err != nil {
			return nil, err
		}
	}
	if !f.PrincipalStudentAll {
		if principal, err = listed(f.PrincipalID); err != nil {
			return nil, err
		}
	}
	out := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		out[id] = (f.StudentAll || own[id]) && (f.PrincipalStudentAll || principal[id])
	}
	return out, nil
}

// cleanName is a set's or a group's name as it is kept: trimmed. checkName
// refuses one that says nothing, is longer than maxGroupName characters, or
// holds a control character, a line break among them.
func cleanName(name string) string { return strings.TrimSpace(name) }

func checkName(what, name string) error {
	name = cleanName(name)
	if name == "" || utf8.RuneCountInString(name) > maxGroupName {
		return apperr.Invalid("a %s's name is 1 to %d characters", what, maxGroupName)
	}
	if strings.ContainsFunc(name, unicode.IsControl) {
		return apperr.Invalid("a %s's name is one line, with no control characters", what)
	}
	return nil
}

func errNameTaken(what, name string) error {
	return apperr.Conflicts("another %s here is called %q; names are unique, whatever their case, among those not archived", what, name).
		With("reason", ReasonNameTaken)
}

func errSetArchived() error {
	return apperr.Precondition("the group set is archived; bring it back first").With("reason", ReasonSetArchived)
}

func errGroupArchived(group uuid.UUID) error {
	return apperr.Precondition("the group is archived").With("reason", ReasonGroupArchived).With("group_id", group)
}

func errNotAStudent(member uuid.UUID) error {
	return apperr.Precondition("only a current student of the course is placed in a group or signs up to one").
		With("reason", ReasonNotAStudent).With("member_id", member)
}

// loadSet finds a set of the course.
func loadSet(ctx context.Context, q dbq.Querier, courseID, id uuid.UUID) (dbq.GroupSet, error) {
	s, err := q.GetGroupSet(ctx, dbq.GetGroupSetParams{ID: id, CourseID: courseID})
	if errors.Is(err, pgx.ErrNoRows) {
		return s, apperr.Missing("no such group set in this course")
	}
	return s, err
}

// loadGroup finds a group of the course.
func loadGroup(ctx context.Context, q dbq.Querier, courseID, id uuid.UUID) (dbq.GetGroupRow, error) {
	g, err := q.GetGroup(ctx, dbq.GetGroupParams{ID: id, CourseID: courseID})
	if errors.Is(err, pgx.ErrNoRows) {
		return g, apperr.Missing("no such group in this course")
	}
	return g, err
}

// liveStudents refuses any of ids that is not a seat of the course whose
// roster role is student, not removed and not past its expiry: only such a
// seat is placed in a group or signs up to one, as only such a seat hands
// work in.
func liveStudents(ctx context.Context, q dbq.Querier, courseID uuid.UUID, ids []uuid.UUID) error {
	rows, err := q.ListStudentSeats(ctx, dbq.ListStudentSeatsParams{CourseID: courseID, Ids: ids})
	if err != nil {
		return err
	}
	ok := make(map[uuid.UUID]bool, len(rows))
	for _, r := range rows {
		ok[r.ID] = r.Role == "student" && r.Status != domain.MemberRemoved && r.Unexpired
	}
	for _, id := range ids {
		if _, seated := ok[id]; !seated {
			return apperr.Missing("no such member in this course").With("member_id", id)
		}
		if !ok[id] {
			return errNotAStudent(id)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

type SignupView struct {
	Open     bool       `json:"open" jsonschema:"whether the teacher has opened the set's groups for students to sign up"`
	ClosesAt *time.Time `json:"closes_at,omitempty" jsonschema:"when sign-up closes, if it has a deadline"`
	Joinable bool       `json:"joinable" jsonschema:"whether a student may sign up now"`
	Reason   *string    `json:"reason,omitempty" jsonschema:"why not: signup_closed, set_archived or course_archived"`
}

type GroupMemberView struct {
	MemberID    uuid.UUID  `json:"member_id"`
	DisplayName *string    `json:"display_name,omitempty"`
	JoinedAt    *time.Time `json:"joined_at,omitempty"`
	JoinedHow   *string    `json:"joined_how,omitempty" jsonschema:"assigned, split or signup"`
}

type GroupWorkView struct {
	AssignmentID uuid.UUID `json:"assignment_id"`
	Title        string    `json:"title"`
	SubmissionID uuid.UUID `json:"submission_id" jsonschema:"its latest attempt"`
	Attempt      int32     `json:"attempt"`
	State        string    `json:"state" jsonschema:"draft, submitted, late or missing"`
}

type GroupView struct {
	ID         uuid.UUID         `json:"id"`
	Name       string            `json:"name"`
	Capacity   *int32            `json:"capacity,omitempty" jsonschema:"the most members sign-up takes it to; absent for no limit"`
	Size       int32             `json:"size" jsonschema:"how many students count as its members now"`
	Full       bool              `json:"full" jsonschema:"at its capacity: sign-up takes nobody more"`
	ArchivedAt *time.Time        `json:"archived_at,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	Members    []GroupMemberView `json:"members,omitempty" jsonschema:"its members now, those the caller's student scope reaches, to those who may read the member list; to a student, their own group's"`
	Work       []GroupWorkView   `json:"work,omitempty" jsonschema:"group_set.get only: its latest work for each assignment of the set, to those who read submissions or the member list, for a group whose members their student scope reaches"`
}

type GroupSetAssignment struct {
	AssignmentID uuid.UUID `json:"assignment_id"`
	Title        string    `json:"title"`
	Published    bool      `json:"published"`
}

type MembershipView struct {
	ID               uuid.UUID  `json:"id"`
	GroupID          uuid.UUID  `json:"group_id"`
	MemberID         uuid.UUID  `json:"member_id"`
	DisplayName      string     `json:"display_name"`
	JoinedAt         time.Time  `json:"joined_at"`
	JoinedByMemberID uuid.UUID  `json:"joined_by_member_id"`
	JoinedHow        string     `json:"joined_how" jsonschema:"assigned, split or signup"`
	LeftAt           *time.Time `json:"left_at,omitempty"`
	LeftByMemberID   *uuid.UUID `json:"left_by_member_id,omitempty"`
	LeftHow          *string    `json:"left_how,omitempty" jsonschema:"moved, unassigned, split, left or switched"`
}

type GroupSetView struct {
	ID          uuid.UUID            `json:"id"`
	Name        string               `json:"name"`
	Description *string              `json:"description,omitempty"`
	Signup      SignupView           `json:"signup"`
	ArchivedAt  *time.Time           `json:"archived_at,omitempty"`
	CreatedAt   time.Time            `json:"created_at"`
	UpdatedAt   time.Time            `json:"updated_at"`
	Groups      []GroupView          `json:"groups"`
	Assignments []GroupSetAssignment `json:"assignments" jsonschema:"the assignments using this set, within the caller's assignment scope; one not published only to those who write assignments"`
	MyGroupID   *uuid.UUID           `json:"my_group_id,omitempty" jsonschema:"the group of this set the caller is in now, or, for a student's own agent, its student"`
	// UnassignedCount and the rest are for those who may read the member
	// list, within their student scope.
	UnassignedCount *int              `json:"unassigned_count,omitempty" jsonschema:"how many of the course's students the caller's scope reaches are in no group of this set; to those who may read the member list"`
	Unassigned      []GroupMemberView `json:"unassigned,omitempty" jsonschema:"group_set.get only: those students, by name"`
	History         []MembershipView  `json:"history,omitempty" jsonschema:"group_set.get with include_history only: every stay in a group of the set, ended or not, the newest first, of students the caller's scope reaches; to those who may read the member list"`
}

// signupState says whether a student may sign up to the set now, and why
// not.
func signupState(s dbq.GroupSet, courseStatus string, now time.Time) SignupView {
	v := SignupView{Open: s.SignupOpen, ClosesAt: s.SignupClosesAt}
	reason := ""
	switch {
	case s.ArchivedAt != nil:
		reason = ReasonSetArchived
	case courseStatus == domain.CourseArchived:
		reason = string(authz.ReasonCourseArchived)
	case !s.SignupOpen || (s.SignupClosesAt != nil && !now.Before(*s.SignupClosesAt)):
		reason = ReasonSignupClosed
	}
	if reason == "" {
		v.Joinable = true
	} else {
		v.Reason = &reason
	}
	return v
}

// setViewer is what a reader of sets is shown, and loads it.
type setViewer struct {
	rc      *tool.ReadCtx
	members bool // may read the member list
	work    bool // may read submissions or the member list
	self    uuid.UUID
	course  string
}

func newSetViewer(ctx context.Context, rc *tool.ReadCtx, courseID uuid.UUID) (setViewer, error) {
	c, err := rc.Q.GetCourseForAuthz(ctx, courseID)
	if err != nil {
		return setViewer{}, err
	}
	members := rc.Member.Perm(domain.PermMemberRead).Allowed()
	return setViewer{rc: rc, members: members, work: members || rc.Member.Perm(domain.PermSubmissionRead).Allowed(),
		self: selfOf(rc.Member), course: c.Status}, nil
}

// views shows the sets as the reader may see them; detail adds group_set.get's
// unassigned students, each group's work and, asked for, the history.
func (v setViewer) views(ctx context.Context, sets []dbq.GroupSet, detail, history bool) ([]GroupSetView, error) {
	q, rc := v.rc.Q, v.rc
	out := make([]GroupSetView, 0, len(sets))
	if len(sets) == 0 {
		return out, nil
	}
	setIDs := make([]uuid.UUID, len(sets))
	for i, s := range sets {
		setIDs[i] = s.ID
	}
	groups, err := q.ListGroupsOfSets(ctx, setIDs)
	if err != nil {
		return nil, err
	}
	groupIDs := make([]uuid.UUID, len(groups))
	for i, g := range groups {
		groupIDs[i] = g.ID
	}
	// Members: those the reader's scope reaches, to a reader of the member
	// list; everyone else is shown their own group's, below.
	scoped := map[uuid.UUID][]GroupMemberView{}
	if v.members {
		rows, err := q.ListLiveMembersOfGroups(ctx, dbq.ListLiveMembersOfGroupsParams{GroupIds: groupIDs,
			StudentAll: rc.Scope.StudentAll, MemberID: rc.Scope.MemberID,
			PrincipalStudentAll: rc.Scope.PrincipalStudentAll, PrincipalID: rc.Scope.PrincipalID})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			name, at, how := r.DisplayName, r.JoinedAt, r.JoinedHow
			scoped[r.GroupID] = append(scoped[r.GroupID], GroupMemberView{MemberID: r.MemberID, DisplayName: &name, JoinedAt: &at, JoinedHow: &how})
		}
	}
	assignments, err := q.ListAssignmentsOfSets(ctx, dbq.ListAssignmentsOfSetsParams{SetIds: setIDs,
		IncludeUnpublished: canSeeUnpublished(rc.Member), AssignmentAll: rc.Scope.AssignmentAll, MemberID: rc.Scope.MemberID,
		PrincipalAssignmentAll: rc.Scope.PrincipalAssignmentAll, PrincipalID: rc.Scope.PrincipalID})
	if err != nil {
		return nil, err
	}
	visible := map[uuid.UUID]bool{}
	bySet := map[uuid.UUID][]GroupSetAssignment{}
	for _, a := range assignments {
		visible[a.ID] = true
		bySet[a.GroupSetID] = append(bySet[a.GroupSetID], GroupSetAssignment{AssignmentID: a.ID, Title: a.Title, Published: a.PublishedAt != nil})
	}
	work := map[uuid.UUID][]GroupWorkView{}
	if detail && v.work && len(groupIDs) > 0 {
		rows, err := q.ListWorkOfGroups(ctx, groupIDs)
		if err != nil {
			return nil, err
		}
		seen := map[[2]uuid.UUID]bool{}
		for _, r := range rows {
			key := [2]uuid.UUID{r.GroupID, r.AssignmentID}
			if seen[key] || !visible[r.AssignmentID] {
				continue
			}
			seen[key] = true
			work[r.GroupID] = append(work[r.GroupID], GroupWorkView{AssignmentID: r.AssignmentID, Title: r.Title,
				SubmissionID: r.SubmissionID, Attempt: r.Attempt, State: r.State})
		}
	}
	now := rc.Now
	for _, s := range sets {
		sv := GroupSetView{ID: s.ID, Name: s.Name, Description: s.Description, Signup: signupState(s, v.course, now),
			ArchivedAt: s.ArchivedAt, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt, Groups: []GroupView{},
			Assignments: bySet[s.ID]}
		if sv.Assignments == nil {
			sv.Assignments = []GroupSetAssignment{}
		}
		mine, err := q.CurrentGroupOf(ctx, dbq.CurrentGroupOfParams{SetID: s.ID, MemberID: v.self})
		switch {
		case err == nil:
			sv.MyGroupID = &mine.ID
		case !errors.Is(err, pgx.ErrNoRows):
			return nil, err
		}
		var own []GroupMemberView
		if sv.MyGroupID != nil && !v.members {
			// A student is shown their own group's members, by name: people
			// who hand work in together know each other's names.
			rows, err := q.ListLiveMembersOfGroups(ctx, dbq.ListLiveMembersOfGroupsParams{GroupIds: []uuid.UUID{*sv.MyGroupID},
				StudentAll: true, PrincipalStudentAll: true})
			if err != nil {
				return nil, err
			}
			for _, r := range rows {
				name := r.DisplayName
				own = append(own, GroupMemberView{MemberID: r.MemberID, DisplayName: &name})
			}
		}
		reaches := map[uuid.UUID]bool{} // groups with a member the reader's scope reaches
		for _, g := range groups {
			if g.SetID != s.ID {
				continue
			}
			gv := GroupView{ID: g.ID, Name: g.Name, Capacity: g.Capacity, Size: g.Size, ArchivedAt: g.ArchivedAt, CreatedAt: g.CreatedAt,
				Full: g.Capacity != nil && g.Size >= *g.Capacity}
			switch {
			case v.members:
				gv.Members = scoped[g.ID]
				reaches[g.ID] = len(gv.Members) > 0
			case sv.MyGroupID != nil && *sv.MyGroupID == g.ID:
				gv.Members = own
				reaches[g.ID] = true
			}
			if detail && v.work && (rc.Scope.StudentAll && rc.Scope.PrincipalStudentAll || reaches[g.ID]) {
				gv.Work = work[g.ID]
			}
			sv.Groups = append(sv.Groups, gv)
		}
		if v.members {
			un, err := q.ListUnassignedStudents(ctx, dbq.ListUnassignedStudentsParams{CourseID: s.CourseID, SetID: s.ID,
				StudentAll: rc.Scope.StudentAll, MemberID: rc.Scope.MemberID,
				PrincipalStudentAll: rc.Scope.PrincipalStudentAll, PrincipalID: rc.Scope.PrincipalID})
			if err != nil {
				return nil, err
			}
			n := len(un)
			sv.UnassignedCount = &n
			if detail {
				sv.Unassigned = make([]GroupMemberView, len(un))
				for i, u := range un {
					name := u.DisplayName
					sv.Unassigned[i] = GroupMemberView{MemberID: u.ID, DisplayName: &name}
				}
			}
			if history {
				rows, err := q.ListMembershipHistory(ctx, s.ID)
				if err != nil {
					return nil, err
				}
				students := make([]uuid.UUID, len(rows))
				for i, r := range rows {
					students[i] = r.MemberID
				}
				in, err := reached(ctx, q, rc.Scope, dedupe(students))
				if err != nil {
					return nil, err
				}
				sv.History = []MembershipView{}
				for _, r := range rows {
					if !in[r.MemberID] {
						continue
					}
					sv.History = append(sv.History, MembershipView{ID: r.ID, GroupID: r.GroupID, MemberID: r.MemberID,
						DisplayName: r.DisplayName, JoinedAt: r.JoinedAt, JoinedByMemberID: r.JoinedByMemberID, JoinedHow: r.JoinedHow,
						LeftAt: r.LeftAt, LeftByMemberID: r.LeftByMemberID, LeftHow: r.LeftHow})
				}
			}
		}
		out = append(out, sv)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// group_set.list, group_set.get
// ---------------------------------------------------------------------------

type GroupSetListIn struct {
	tool.InCourse
	IncludeArchived bool `json:"include_archived,omitempty" jsonschema:"list archived sets too"`
}

type GroupSetListOut struct {
	Sets []GroupSetView `json:"sets"`
}

func groupSetList() tool.Tool {
	return tool.Define(tool.Spec[GroupSetListIn, GroupSetListOut]{
		Name: "group_set.list",
		Description: "The course's group sets (分組, such as project groups or lab groups), the oldest first, each with its " +
			"groups (name, capacity, size now), whether students may sign up now and why not, the assignments using it, " +
			"and the caller's own group in it (my_group_id). Members' names come to those who may read the member list, " +
			"for the students their scope reaches, with how many are in no group; a student is shown their own group's " +
			"members.",
		Kind: tool.Read, Gate: tool.Gate{Perms: []domain.Perm{domain.PermDocumentRead}},
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/group-sets"},
		Resolve: func(_ context.Context, _ dbq.Querier, in GroupSetListIn) (tool.Target, error) {
			return tool.Target{CourseID: in.CourseID, Type: "group_set"}, nil
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in GroupSetListIn) (GroupSetListOut, error) {
			sets, err := rc.Q.ListGroupSets(ctx, dbq.ListGroupSetsParams{CourseID: in.CourseID, IncludeArchived: in.IncludeArchived})
			if err != nil {
				return GroupSetListOut{}, err
			}
			v, err := newSetViewer(ctx, rc, in.CourseID)
			if err != nil {
				return GroupSetListOut{}, err
			}
			views, err := v.views(ctx, sets, false, false)
			return GroupSetListOut{Sets: views}, err
		},
	})
}

type GroupSetIDIn struct {
	tool.InCourse
	SetID uuid.UUID `json:"set_id"`
}

type GroupSetGetIn struct {
	GroupSetIDIn
	IncludeHistory bool `json:"include_history,omitempty" jsonschema:"every stay in a group of the set, ended or not, to those who may read the member list"`
}

func setTarget(ctx context.Context, q dbq.Querier, courseID, id uuid.UUID) (tool.Target, error) {
	if _, err := loadSet(ctx, q, courseID, id); err != nil {
		return tool.Target{}, err
	}
	return tool.Target{CourseID: courseID, Type: "group_set", ID: &id}, nil
}

func groupSetGet() tool.Tool {
	return tool.Define(tool.Spec[GroupSetGetIn, GroupSetView]{
		Name: "group_set.get",
		Description: "One group set as group_set.list shows it, and, to those who may read the member list, the students " +
			"in no group of it by name; to those who read submissions or the member list, each group's latest work for " +
			"each assignment of the set, for the groups their student scope reaches; and with include_history, every " +
			"stay in its groups, joined and left, by whom and how.",
		Kind: tool.Read, Gate: tool.Gate{Perms: []domain.Perm{domain.PermDocumentRead}},
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/group-sets/{set_id}"},
		Resolve: func(ctx context.Context, q dbq.Querier, in GroupSetGetIn) (tool.Target, error) {
			return setTarget(ctx, q, in.CourseID, in.SetID)
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in GroupSetGetIn) (GroupSetView, error) {
			s, err := loadSet(ctx, rc.Q, in.CourseID, in.SetID)
			if err != nil {
				return GroupSetView{}, err
			}
			v, err := newSetViewer(ctx, rc, in.CourseID)
			if err != nil {
				return GroupSetView{}, err
			}
			views, err := v.views(ctx, []dbq.GroupSet{s}, true, in.IncludeHistory)
			if err != nil {
				return GroupSetView{}, err
			}
			return views[0], nil
		},
	})
}

// ---------------------------------------------------------------------------
// group_set.create, group_set.update
// ---------------------------------------------------------------------------

type GroupSetCreateIn struct {
	tool.InCourse
	Name           string     `json:"name" jsonschema:"1 to 100 characters on one line, unique among the course's sets not archived"`
	Description    *string    `json:"description,omitempty" jsonschema:"at most 2000 characters"`
	SignupOpen     bool       `json:"signup_open,omitempty" jsonschema:"let students sign themselves up to its groups"`
	SignupClosesAt *time.Time `json:"signup_closes_at,omitempty" jsonschema:"when sign-up closes; none for no deadline"`
}

func checkDescription(d *string) error {
	if d != nil && utf8.RuneCountInString(*d) > maxSetDescription {
		return apperr.Invalid("a set's description is at most %d characters", maxSetDescription)
	}
	return nil
}

// keptDescription is a description as it is kept: none for one that says
// nothing.
func keptDescription(d *string) *string {
	if d == nil || strings.TrimSpace(*d) == "" {
		return nil
	}
	return d
}

func setNameFree(ctx context.Context, q dbq.Querier, courseID, except uuid.UUID, name string) error {
	taken, err := q.GroupSetNameTaken(ctx, dbq.GroupSetNameTakenParams{CourseID: courseID, Name: name, ExceptID: except})
	if err != nil {
		return err
	}
	if taken {
		return errNameTaken("group set", name)
	}
	return nil
}

func groupSetCreate() tool.Tool {
	return tool.Define(tool.Spec[GroupSetCreateIn, IDOut]{
		Name: "group_set.create",
		Description: "Make a group set (分組) in the course, to hold groups that group assignments use: one set serves " +
			"any number of assignments. Groups are added with group.create or group.split; students are placed with " +
			"group.set_members, or sign themselves up while signup_open, until signup_closes_at.",
		Kind: tool.Write, Gate: formGroups,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/group-sets"},
		Check: func(in GroupSetCreateIn) error {
			if err := checkName("group set", in.Name); err != nil {
				return err
			}
			return checkDescription(in.Description)
		},
		Resolve: func(_ context.Context, _ dbq.Querier, in GroupSetCreateIn) (tool.Target, error) {
			return tool.Target{CourseID: in.CourseID, Type: "group_set"}, nil
		},
		Validate: func(ctx context.Context, q dbq.Querier, _ *domain.Member, _ time.Time, in GroupSetCreateIn) error {
			return setNameFree(ctx, q, in.CourseID, uuid.Nil, cleanName(in.Name))
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in GroupSetCreateIn) (IDOut, error) {
			name := cleanName(in.Name)
			if err := setNameFree(ctx, ec.Q, in.CourseID, uuid.Nil, name); err != nil {
				return IDOut{}, err
			}
			id := ids.New()
			if err := ec.Q.InsertGroupSet(ctx, dbq.InsertGroupSetParams{ID: id, CourseID: in.CourseID, Name: name,
				Description: keptDescription(in.Description), SignupOpen: in.SignupOpen, SignupClosesAt: in.SignupClosesAt,
				CreatedByMemberID: ec.Member.ID, CreatedAt: ec.Now}); err != nil {
				return IDOut{}, err
			}
			ec.Emit(events.Event{Type: EventGroupSetCreated, CourseID: &in.CourseID, SubjectType: "group_set", SubjectID: &id,
				Payload: map[string]any{"signup_open": in.SignupOpen, "signup_closes_at": in.SignupClosesAt}})
			return IDOut{ID: id}, nil
		},
	})
}

type GroupSetUpdateIn struct {
	GroupSetIDIn
	Name                *string    `json:"name,omitempty"`
	Description         *string    `json:"description,omitempty" jsonschema:"empty takes it away"`
	SignupOpen          *bool      `json:"signup_open,omitempty"`
	SignupClosesAt      *time.Time `json:"signup_closes_at,omitempty"`
	ClearSignupClosesAt bool       `json:"clear_signup_closes_at,omitempty" jsonschema:"sign-up has no deadline"`
	Archived            *bool      `json:"archived,omitempty" jsonschema:"true archives it: hidden from new assignments, its sign-up closed, assignments using it working as before; false brings it back"`
}

type ChangedOut struct {
	Changed bool `json:"changed" jsonschema:"false when it already said this: nothing was done"`
}

func groupSetUpdate() tool.Tool {
	return tool.Define(tool.Spec[GroupSetUpdateIn, ChangedOut]{
		Name: "group_set.update",
		Description: "Rename a group set, describe it, open or close sign-up to its groups and set its deadline, or archive " +
			"it, which closes its sign-up and hides it from new assignments while those using it go on working, or bring " +
			"it back. An archived set changes in nothing else until it is brought back.",
		Kind: tool.Write, Gate: formGroups,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/group-sets/{set_id}"},
		Check: func(in GroupSetUpdateIn) error {
			if in.Name != nil {
				if err := checkName("group set", *in.Name); err != nil {
					return err
				}
			}
			if in.SignupClosesAt != nil && in.ClearSignupClosesAt {
				return apperr.Invalid("give signup_closes_at or clear_signup_closes_at, not both")
			}
			return checkDescription(in.Description)
		},
		Resolve: func(ctx context.Context, q dbq.Querier, in GroupSetUpdateIn) (tool.Target, error) {
			return setTarget(ctx, q, in.CourseID, in.SetID)
		},
		Validate: func(ctx context.Context, q dbq.Querier, _ *domain.Member, now time.Time, in GroupSetUpdateIn) error {
			s, err := loadSet(ctx, q, in.CourseID, in.SetID)
			if err != nil {
				return err
			}
			_, _, err = in.apply(ctx, q, s, now)
			return err
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in GroupSetUpdateIn) (ChangedOut, error) {
			s, err := ec.Q.LockGroupSet(ctx, dbq.LockGroupSetParams{ID: in.SetID, CourseID: in.CourseID})
			if err != nil {
				return ChangedOut{}, err
			}
			next, changed, err := in.apply(ctx, ec.Q, s, ec.Now)
			if err != nil || len(changed) == 0 {
				return ChangedOut{}, err
			}
			if err := ec.Q.UpdateGroupSet(ctx, dbq.UpdateGroupSetParams{ID: s.ID, Name: next.Name, Description: next.Description,
				SignupOpen: next.SignupOpen, SignupClosesAt: next.SignupClosesAt, ArchivedAt: next.ArchivedAt, UpdatedAt: ec.Now}); err != nil {
				return ChangedOut{}, err
			}
			ec.Emit(events.Event{Type: EventGroupSetUpdated, CourseID: &in.CourseID, SubjectType: "group_set", SubjectID: &s.ID,
				Payload: map[string]any{"changed": changed, "signup_open": next.SignupOpen, "signup_closes_at": next.SignupClosesAt,
					"archived": next.ArchivedAt != nil}})
			return ChangedOut{Changed: true}, nil
		},
	})
}

// apply is the set once in is applied to s, and which of its fields that
// changes; refused for an archived set, but to bring it back, and for a name
// another set has.
func (in GroupSetUpdateIn) apply(ctx context.Context, q dbq.Querier, s dbq.GroupSet, now time.Time) (dbq.GroupSet, []string, error) {
	next, changed := s, []string{}
	if s.ArchivedAt != nil && (in.Archived == nil || *in.Archived) &&
		(in.Name != nil || in.Description != nil || in.SignupOpen != nil || in.SignupClosesAt != nil || in.ClearSignupClosesAt) {
		return s, nil, errSetArchived()
	}
	if in.Name != nil && cleanName(*in.Name) != s.Name {
		next.Name = cleanName(*in.Name)
		changed = append(changed, "name")
	}
	if in.Description != nil && !sameText(keptDescription(in.Description), s.Description) {
		next.Description = keptDescription(in.Description)
		changed = append(changed, "description")
	}
	if in.SignupOpen != nil && *in.SignupOpen != s.SignupOpen {
		next.SignupOpen = *in.SignupOpen
		changed = append(changed, "signup_open")
	}
	switch {
	case in.ClearSignupClosesAt && s.SignupClosesAt != nil:
		next.SignupClosesAt = nil
		changed = append(changed, "signup_closes_at")
	case in.SignupClosesAt != nil && !sameTime(in.SignupClosesAt, s.SignupClosesAt):
		next.SignupClosesAt = in.SignupClosesAt
		changed = append(changed, "signup_closes_at")
	}
	if in.Archived != nil && *in.Archived != (s.ArchivedAt != nil) {
		if *in.Archived {
			at := now
			next.ArchivedAt, next.SignupOpen = &at, false
		} else {
			next.ArchivedAt = nil
		}
		changed = append(changed, "archived")
	}
	if next.ArchivedAt == nil && (s.ArchivedAt != nil || next.Name != s.Name) {
		if err := setNameFree(ctx, q, s.CourseID, s.ID, next.Name); err != nil {
			return s, nil, err
		}
	}
	return next, changed, nil
}

// ---------------------------------------------------------------------------
// group.create, group.update
// ---------------------------------------------------------------------------

type GroupIn struct {
	Name     string `json:"name" jsonschema:"1 to 100 characters on one line, unique among the set's groups not archived"`
	Capacity *int32 `json:"capacity,omitempty" jsonschema:"1 to 500: the most members sign-up takes it to; none for no limit. It does not bind the teacher"`
}

type GroupCreateIn struct {
	GroupSetIDIn
	Groups []GroupIn `json:"groups" jsonschema:"1 to 100 groups to add"`
}

type GroupCreateOut struct {
	GroupIDs []uuid.UUID `json:"group_ids" jsonschema:"in the order given"`
}

func checkCapacity(c *int32) error {
	if c != nil && (*c < 1 || *c > maxGroupSize) {
		return apperr.Invalid("a group's capacity is 1 to %d", maxGroupSize)
	}
	return nil
}

func groupCreate() tool.Tool {
	return tool.Define(tool.Spec[GroupCreateIn, GroupCreateOut]{
		Name: "group.create",
		Description: "Add groups to a group set, each with a name and, if sign-up is to stop at a size, a capacity. A set " +
			"that is archived takes none.",
		Kind: tool.Write, Gate: formGroups,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/group-sets/{set_id}/groups"},
		Check: func(in GroupCreateIn) error {
			if len(in.Groups) == 0 || len(in.Groups) > maxGroupsPerCall {
				return apperr.Invalid("give 1 to %d groups", maxGroupsPerCall)
			}
			seen := map[string]bool{}
			for _, g := range in.Groups {
				if err := checkName("group", g.Name); err != nil {
					return err
				}
				if err := checkCapacity(g.Capacity); err != nil {
					return err
				}
				key := strings.ToLower(cleanName(g.Name))
				if seen[key] {
					return apperr.Invalid("two of the groups are called %q", cleanName(g.Name))
				}
				seen[key] = true
			}
			return nil
		},
		Resolve: func(ctx context.Context, q dbq.Querier, in GroupCreateIn) (tool.Target, error) {
			return setTarget(ctx, q, in.CourseID, in.SetID)
		},
		Validate: func(ctx context.Context, q dbq.Querier, _ *domain.Member, _ time.Time, in GroupCreateIn) error {
			s, err := loadSet(ctx, q, in.CourseID, in.SetID)
			if err != nil {
				return err
			}
			return in.creatable(ctx, q, s)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in GroupCreateIn) (GroupCreateOut, error) {
			s, err := ec.Q.ShareGroupSet(ctx, dbq.ShareGroupSetParams{ID: in.SetID, CourseID: in.CourseID})
			if err != nil {
				return GroupCreateOut{}, err
			}
			if err := in.creatable(ctx, ec.Q, s); err != nil {
				return GroupCreateOut{}, err
			}
			out := GroupCreateOut{GroupIDs: make([]uuid.UUID, 0, len(in.Groups))}
			for _, g := range in.Groups {
				id := ids.New()
				if err := ec.Q.InsertGroup(ctx, dbq.InsertGroupParams{ID: id, CourseID: in.CourseID, SetID: s.ID, Name: cleanName(g.Name),
					Capacity: g.Capacity, CreatedByMemberID: ec.Member.ID, CreatedAt: ec.Now}); err != nil {
					return GroupCreateOut{}, err
				}
				out.GroupIDs = append(out.GroupIDs, id)
				ec.Emit(events.Event{Type: EventGroupCreated, CourseID: &in.CourseID, SubjectType: subjectGroup, SubjectID: &id,
					Payload: map[string]any{"set_id": s.ID}})
			}
			return out, nil
		},
	})
}

// creatable refuses adding in's groups to s: s archived, or a name one of
// its groups has.
func (in GroupCreateIn) creatable(ctx context.Context, q dbq.Querier, s dbq.GroupSet) error {
	if s.ArchivedAt != nil {
		return errSetArchived()
	}
	for _, g := range in.Groups {
		if err := groupNameFree(ctx, q, s.ID, uuid.Nil, cleanName(g.Name)); err != nil {
			return err
		}
	}
	return nil
}

func groupNameFree(ctx context.Context, q dbq.Querier, set, except uuid.UUID, name string) error {
	taken, err := q.GroupNameTaken(ctx, dbq.GroupNameTakenParams{SetID: set, Name: name, ExceptID: except})
	if err != nil {
		return err
	}
	if taken {
		return errNameTaken("group", name)
	}
	return nil
}

type GroupIDIn struct {
	tool.InCourse
	GroupID uuid.UUID `json:"group_id"`
}

type GroupUpdateIn struct {
	GroupIDIn
	Name          *string `json:"name,omitempty"`
	Capacity      *int32  `json:"capacity,omitempty" jsonschema:"1 to 500"`
	ClearCapacity bool    `json:"clear_capacity,omitempty" jsonschema:"no limit"`
	Archived      *bool   `json:"archived,omitempty" jsonschema:"true archives it, only while nobody is in it and it has no work; false brings it back"`
}

func groupUpdate() tool.Tool {
	return tool.Define(tool.Spec[GroupUpdateIn, ChangedOut]{
		Name: "group.update",
		Description: "Rename a group, set or take away its capacity — setting it to the group's size closes it to sign-up — " +
			"or archive it, only while nobody is in it (group_not_empty) and it has no work for any assignment " +
			"(group_has_work), or bring it back.",
		Kind: tool.Write, Gate: formGroups,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/groups/{group_id}"},
		Check: func(in GroupUpdateIn) error {
			if in.Name != nil {
				if err := checkName("group", *in.Name); err != nil {
					return err
				}
			}
			if in.Capacity != nil && in.ClearCapacity {
				return apperr.Invalid("give capacity or clear_capacity, not both")
			}
			return checkCapacity(in.Capacity)
		},
		Resolve: func(ctx context.Context, q dbq.Querier, in GroupUpdateIn) (tool.Target, error) {
			if _, err := loadGroup(ctx, q, in.CourseID, in.GroupID); err != nil {
				return tool.Target{}, err
			}
			return tool.Target{CourseID: in.CourseID, Type: subjectGroup, ID: &in.GroupID}, nil
		},
		Validate: func(ctx context.Context, q dbq.Querier, _ *domain.Member, now time.Time, in GroupUpdateIn) error {
			g, err := loadGroup(ctx, q, in.CourseID, in.GroupID)
			if err != nil {
				return err
			}
			s, err := loadSet(ctx, q, in.CourseID, g.SetID)
			if err != nil {
				return err
			}
			_, _, err = in.apply(ctx, q, s, g, now)
			return err
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in GroupUpdateIn) (ChangedOut, error) {
			g, err := loadGroup(ctx, ec.Q, in.CourseID, in.GroupID)
			if err != nil {
				return ChangedOut{}, err
			}
			s, err := ec.Q.ShareGroupSet(ctx, dbq.ShareGroupSetParams{ID: g.SetID, CourseID: in.CourseID})
			if err != nil {
				return ChangedOut{}, err
			}
			if _, err := ec.Q.LockGroups(ctx, dbq.LockGroupsParams{SetID: s.ID, Ids: []uuid.UUID{g.ID}}); err != nil {
				return ChangedOut{}, err
			}
			if g, err = loadGroup(ctx, ec.Q, in.CourseID, in.GroupID); err != nil {
				return ChangedOut{}, err
			}
			next, changed, err := in.apply(ctx, ec.Q, s, g, ec.Now)
			if err != nil || len(changed) == 0 {
				return ChangedOut{}, err
			}
			if err := ec.Q.UpdateGroup(ctx, dbq.UpdateGroupParams{ID: g.ID, Name: next.Name, Capacity: next.Capacity,
				ArchivedAt: next.ArchivedAt}); err != nil {
				return ChangedOut{}, err
			}
			ec.Emit(events.Event{Type: EventGroupUpdated, CourseID: &in.CourseID, SubjectType: subjectGroup, SubjectID: &g.ID,
				Payload: map[string]any{"set_id": s.ID, "changed": changed, "archived": next.ArchivedAt != nil}})
			return ChangedOut{Changed: true}, nil
		},
	})
}

// apply is the group once in is applied to g, of set s, and which of its
// fields that changes.
func (in GroupUpdateIn) apply(ctx context.Context, q dbq.Querier, s dbq.GroupSet, g dbq.GetGroupRow, now time.Time) (dbq.GetGroupRow, []string, error) {
	if s.ArchivedAt != nil {
		return g, nil, errSetArchived()
	}
	next, changed := g, []string{}
	if g.ArchivedAt != nil && (in.Archived == nil || *in.Archived) && (in.Name != nil || in.Capacity != nil || in.ClearCapacity) {
		return g, nil, errGroupArchived(g.ID)
	}
	if in.Name != nil && cleanName(*in.Name) != g.Name {
		next.Name = cleanName(*in.Name)
		changed = append(changed, "name")
	}
	switch {
	case in.ClearCapacity && g.Capacity != nil:
		next.Capacity = nil
		changed = append(changed, "capacity")
	case in.Capacity != nil && (g.Capacity == nil || *g.Capacity != *in.Capacity):
		next.Capacity = in.Capacity
		changed = append(changed, "capacity")
	}
	if in.Archived != nil && *in.Archived != (g.ArchivedAt != nil) {
		if *in.Archived {
			if g.Size > 0 {
				return g, nil, apperr.Precondition("the group has members; place them elsewhere first").With("reason", ReasonGroupNotEmpty)
			}
			work, err := groupWork(ctx, q, []uuid.UUID{g.ID}, false)
			if err != nil {
				return g, nil, err
			}
			if len(work) > 0 {
				return g, nil, errGroupHasWork(ReasonGroupHasWork, msgArchiveHasWork, work)
			}
			at := now
			next.ArchivedAt = &at
		} else {
			next.ArchivedAt = nil
		}
		changed = append(changed, "archived")
	}
	if next.ArchivedAt == nil && (g.ArchivedAt != nil || next.Name != g.Name) {
		if err := groupNameFree(ctx, q, s.ID, g.ID, next.Name); err != nil {
			return g, nil, err
		}
	}
	return next, changed, nil
}

// WorkDetail is a group's work, as a refusal names it: the group, the
// assignment, and its latest attempt's state.
type WorkDetail struct {
	GroupID      uuid.UUID `json:"group_id"`
	AssignmentID uuid.UUID `json:"assignment_id"`
	SubmissionID uuid.UUID `json:"submission_id"`
	State        string    `json:"state"`
}

// groupWork is the latest work of each of the groups for each assignment of
// their set: any submission row, a draft included, or with handedIn only
// work that is not a draft.
func groupWork(ctx context.Context, q dbq.Querier, groups []uuid.UUID, handedIn bool) ([]WorkDetail, error) {
	if len(groups) == 0 {
		return nil, nil
	}
	if handedIn {
		rows, err := q.GroupsWithHandedInWork(ctx, groups)
		out := make([]WorkDetail, len(rows))
		for i, r := range rows {
			out[i] = WorkDetail{GroupID: r.GroupID, AssignmentID: r.AssignmentID, SubmissionID: r.SubmissionID, State: r.State}
		}
		return out, err
	}
	rows, err := q.ListWorkOfGroups(ctx, groups)
	if err != nil {
		return nil, err
	}
	var out []WorkDetail
	seen := map[[2]uuid.UUID]bool{}
	for _, r := range rows {
		key := [2]uuid.UUID{r.GroupID, r.AssignmentID}
		if !seen[key] {
			seen[key] = true
			out = append(out, WorkDetail{GroupID: r.GroupID, AssignmentID: r.AssignmentID, SubmissionID: r.SubmissionID, State: r.State})
		}
	}
	return out, nil
}

// errGroupHasWork refuses, saying why and naming each work.
func errGroupHasWork(reason, msg string, work []WorkDetail) error {
	return apperr.Precondition("%s", msg).With("reason", reason).With("work", work)
}

const (
	msgPlacementHasWork = "a group this places students out of or into has work for an assignment of its set: a draft " +
		"follows the group, and work handed in keeps who it was handed in for; say affects_work to place them anyway"
	msgArchiveHasWork  = "the group has work for an assignment of its set, and is kept as it is"
	msgYourGroupWork   = "your group has handed work in for an assignment of its set: who did it is the teacher's to change now"
	msgTargetGroupWork = "that group has handed work in for an assignment of its set: who did it is the teacher's to change now"
)

// ---------------------------------------------------------------------------
// Placing students: group.set_members
// ---------------------------------------------------------------------------

type PlacementIn struct {
	StudentMemberID uuid.UUID  `json:"student_member_id"`
	GroupID         *uuid.UUID `json:"group_id,omitempty" jsonschema:"the group to place them in; absent takes them out of every group of the set"`
}

type GroupSetMembersIn struct {
	GroupSetIDIn
	Placements  []PlacementIn `json:"placements" jsonschema:"1 to 500 students, each placed in a group of the set or taken out of its groups; all or none"`
	AffectsWork bool          `json:"affects_work,omitempty" jsonschema:"place them although a group they leave or join has work for an assignment of the set: a draft follows the group, and work handed in keeps who it was handed in for. Without it, such a placement is refused (group_has_work), naming each work. A proposal records it as given"`
}

type PlacementOut struct {
	StudentMemberID uuid.UUID  `json:"student_member_id"`
	FromGroupID     *uuid.UUID `json:"from_group_id,omitempty"`
	GroupID         *uuid.UUID `json:"group_id,omitempty"`
}

type GroupSetMembersOut struct {
	Moved        []PlacementOut `json:"moved" jsonschema:"the students whose group changed; one already where they were placed is left as they were"`
	OverCapacity []uuid.UUID    `json:"over_capacity" jsonschema:"groups now above their capacity: capacity binds sign-up, not the teacher"`
}

func (in GroupSetMembersIn) students() []uuid.UUID {
	out := make([]uuid.UUID, len(in.Placements))
	for i, p := range in.Placements {
		out[i] = p.StudentMemberID
	}
	return out
}

func groupSetMembers() tool.Tool {
	return tool.Define(tool.Spec[GroupSetMembersIn, GroupSetMembersOut]{
		Name: "group.set_members",
		Description: "Place students in groups of a set by hand, or take them out of its groups: each student named goes " +
			"to the group given, or out of every group of the set when none is; all or none. Capacity does not bind the " +
			"teacher: the result names any group now over it. A placement that moves a student out of, or into, a group " +
			"with work for an assignment of the set — a draft included — is refused (group_has_work, naming each work) " +
			"unless the call says affects_work: a draft follows the group, and work handed in keeps who it was handed in " +
			"for. It reaches every student named.",
		Kind: tool.Write, Gate: formGroups,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/group-sets/{set_id}/members"},
		Check: func(in GroupSetMembersIn) error {
			if len(in.Placements) == 0 || len(in.Placements) > maxPlacementsPerCall {
				return apperr.Invalid("give 1 to %d placements", maxPlacementsPerCall)
			}
			seen := map[uuid.UUID]bool{}
			for _, p := range in.Placements {
				if seen[p.StudentMemberID] {
					return apperr.Invalid("student %s is placed twice", p.StudentMemberID)
				}
				seen[p.StudentMemberID] = true
			}
			return nil
		},
		Resolve: func(ctx context.Context, q dbq.Querier, in GroupSetMembersIn) (tool.Target, error) {
			t, err := setTarget(ctx, q, in.CourseID, in.SetID)
			t.Scope.StudentMemberIDs = in.students()
			return t, err
		},
		Validate: func(ctx context.Context, q dbq.Querier, _ *domain.Member, _ time.Time, in GroupSetMembersIn) error {
			s, err := loadSet(ctx, q, in.CourseID, in.SetID)
			if err != nil {
				return err
			}
			_, err = in.plan(ctx, q, s, false)
			return err
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in GroupSetMembersIn) (GroupSetMembersOut, error) {
			s, err := ec.Q.ShareGroupSet(ctx, dbq.ShareGroupSetParams{ID: in.SetID, CourseID: in.CourseID})
			if err != nil {
				return GroupSetMembersOut{}, err
			}
			p, err := in.plan(ctx, ec.Q, s, true)
			if err != nil {
				return GroupSetMembersOut{}, err
			}
			out := GroupSetMembersOut{Moved: []PlacementOut{}, OverCapacity: []uuid.UUID{}}
			for _, mv := range p.moves {
				if err := moveStudent(ctx, ec, s, mv, joinedAssigned); err != nil {
					return GroupSetMembersOut{}, err
				}
				out.Moved = append(out.Moved, PlacementOut{StudentMemberID: mv.student, FromGroupID: mv.from, GroupID: mv.to})
			}
			for _, g := range p.locked {
				if g.Capacity == nil {
					continue
				}
				n, err := ec.Q.LiveMembersOf(ctx, g.ID)
				if err != nil {
					return GroupSetMembersOut{}, err
				}
				if int32(len(n)) > *g.Capacity { //nolint:gosec // at most a course's students
					out.OverCapacity = append(out.OverCapacity, g.ID)
				}
			}
			return out, nil
		},
	})
}

// move is one student's change of group: out of from (nil: none), into to
// (nil: none).
type move struct {
	student  uuid.UUID
	stay     *uuid.UUID // the stay that ends, if any
	from, to *uuid.UUID
}

type placementPlan struct {
	moves  []move
	locked []dbq.LockGroupsRow
}

// plan works out the moves in asks of set s, refusing what may not be done:
// an archived set or group, a group of another set, a seat that is not a
// student's, and, without affects_work, a move out of or into a group with
// work. lock takes the groups touched FOR UPDATE, in id order, and reads
// them again under it.
func (in GroupSetMembersIn) plan(ctx context.Context, q dbq.Querier, s dbq.GroupSet, lock bool) (placementPlan, error) {
	var p placementPlan
	if s.ArchivedAt != nil {
		return p, errSetArchived()
	}
	if err := liveStudents(ctx, q, s.CourseID, in.students()); err != nil {
		return p, err
	}
	stays, err := q.ListCurrentMemberships(ctx, dbq.ListCurrentMembershipsParams{SetID: s.ID, MemberIds: in.students()})
	if err != nil {
		return p, err
	}
	current := map[uuid.UUID]dbq.ListCurrentMembershipsRow{}
	for _, st := range stays {
		current[st.MemberID] = st
	}
	touched := map[uuid.UUID]bool{}
	for _, pl := range in.Placements {
		mv := move{student: pl.StudentMemberID, to: pl.GroupID}
		if st, ok := current[pl.StudentMemberID]; ok {
			id, from := st.ID, st.GroupID
			mv.stay, mv.from = &id, &from
		}
		if sameID(mv.from, mv.to) {
			continue
		}
		for _, g := range []*uuid.UUID{mv.from, mv.to} {
			if g != nil {
				touched[*g] = true
			}
		}
		p.moves = append(p.moves, mv)
	}
	groups := make([]uuid.UUID, 0, len(touched))
	for g := range touched {
		groups = append(groups, g)
	}
	slices.SortFunc(groups, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	if lock {
		if p.locked, err = q.LockGroups(ctx, dbq.LockGroupsParams{SetID: s.ID, Ids: groups}); err != nil {
			return p, err
		}
	} else {
		rows, err := q.ListGroupsOfSets(ctx, []uuid.UUID{s.ID})
		if err != nil {
			return p, err
		}
		for _, r := range rows {
			if touched[r.ID] {
				p.locked = append(p.locked, dbq.LockGroupsRow{ID: r.ID, Name: r.Name, Capacity: r.Capacity, ArchivedAt: r.ArchivedAt, CreatedAt: r.CreatedAt})
			}
		}
	}
	found := map[uuid.UUID]dbq.LockGroupsRow{}
	for _, g := range p.locked {
		found[g.ID] = g
	}
	for _, pl := range in.Placements {
		if pl.GroupID == nil {
			continue
		}
		g, ok := found[*pl.GroupID]
		switch {
		case !ok && touched[*pl.GroupID]:
			return p, apperr.Missing("no such group in this set").With("group_id", *pl.GroupID)
		case ok && g.ArchivedAt != nil:
			return p, errGroupArchived(g.ID)
		}
	}
	if !in.AffectsWork {
		work, err := groupWork(ctx, q, groups, false)
		if err != nil {
			return p, err
		}
		if len(work) > 0 {
			return p, errGroupHasWork(ReasonGroupHasWork, msgPlacementHasWork, work)
		}
	}
	return p, nil
}

// moveStudent carries a move out, for set s, as ec's action: the stay that
// was ends, saying how, and a new one begins, how it was made; each said in
// the student's feed.
func moveStudent(ctx context.Context, ec *tool.ExecCtx, s dbq.GroupSet, mv move, how string) error {
	student := mv.student
	if mv.stay != nil {
		left := leftUnassigned
		switch {
		case mv.to != nil && how == joinedSignup:
			left = leftSwitched
		case mv.to != nil:
			left = leftMoved
		case how == joinedSignup:
			left = leftLeft
		case how == joinedSplit:
			left = leftSplit
		}
		n, err := ec.Q.EndMembership(ctx, dbq.EndMembershipParams{ID: *mv.stay, LeftAt: &ec.Now, LeftByMemberID: &ec.Member.ID,
			LeftHow: &left, LeftActionID: &ec.ActionID})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.Conflicts("the student's group changed just now; look again")
		}
		ec.Emit(events.Event{Type: EventGroupMemberRemoved, CourseID: &s.CourseID, SubjectType: subjectGroup, SubjectID: mv.from,
			StudentMemberID: &student, Payload: map[string]any{"set_id": s.ID, "group_id": *mv.from, "how": left}})
	}
	if mv.to == nil {
		return nil
	}
	if err := ec.Q.InsertMembership(ctx, dbq.InsertMembershipParams{ID: ids.New(), CourseID: s.CourseID, SetID: s.ID, GroupID: *mv.to,
		MemberID: student, JoinedAt: ec.Now, JoinedByMemberID: ec.Member.ID, JoinedHow: how, JoinedActionID: ec.ActionID}); err != nil {
		return err
	}
	payload := map[string]any{"set_id": s.ID, "group_id": *mv.to, "how": how}
	if mv.from != nil {
		payload["from_group_id"] = *mv.from
	}
	ec.Emit(events.Event{Type: EventGroupMemberAdded, CourseID: &s.CourseID, SubjectType: subjectGroup, SubjectID: mv.to,
		StudentMemberID: &student, Payload: payload})
	return nil
}

// ---------------------------------------------------------------------------
// The random split: group.split
// ---------------------------------------------------------------------------

type GroupSplitIn struct {
	GroupSetIDIn
	By         string  `json:"by" jsonschema:"size: groups of up to n students; count: until the set has n groups"`
	N          int     `json:"n" jsonschema:"1 to 500"`
	From       string  `json:"from" jsonschema:"unassigned: deal the students in no group of the set; all: empty every group with no work first, and deal everyone not in a group with work"`
	Seed       *string `json:"seed,omitempty" jsonschema:"1 to 64 printable ASCII characters: the same seed deals the same way. Made up and returned when absent; a proposal records it"`
	NamePrefix *string `json:"name_prefix,omitempty" jsonschema:"new groups are called this and the lowest number not in use; default 'Group '"`
	Capacity   *int32  `json:"capacity,omitempty" jsonschema:"1 to 500: the capacity of the groups it makes"`
}

type SplitGroupOut struct {
	GroupID uuid.UUID `json:"group_id"`
	Name    string    `json:"name"`
}

type SplitKeptOut struct {
	GroupID uuid.UUID `json:"group_id"`
	Reason  string    `json:"reason" jsonschema:"has_work: it has work for an assignment of the set, and kept its members"`
}

type SplitPlacedOut struct {
	StudentMemberID uuid.UUID `json:"student_member_id"`
	GroupID         uuid.UUID `json:"group_id"`
}

type GroupSplitOut struct {
	Seed    string           `json:"seed" jsonschema:"the seed it was dealt with: the same seed, students and groups deal the same way"`
	Created []SplitGroupOut  `json:"created"`
	Placed  []SplitPlacedOut `json:"placed"`
	Kept    []SplitKeptOut   `json:"kept"`
	Emptied int              `json:"emptied" jsonschema:"stays ended to be dealt again (from all)"`
}

const (
	splitFromUnassigned, splitFromAll = "unassigned", "all"
	keptHasWork                       = "has_work"
)

// madeBySplit is when the deal takes the groups a split makes to have been
// made: after every group there is.
var madeBySplit = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

func errBadSplit(format string, args ...any) error {
	return apperr.Invalid(format, args...).With("reason", ReasonBadSplit)
}

func groupSplit() tool.Tool {
	return tool.Define(tool.Spec[GroupSplitIn, GroupSplitOut]{
		Name: "group.split",
		Description: "Split the course's students at random into groups of a set, by size (groups of up to n) or by count " +
			"(until the set has n groups), from the students in no group (unassigned) or from everyone (all), which " +
			"empties every group with no work first. Groups with work for an assignment of the set are never touched: " +
			"they keep their members and are named in kept. New groups are made as needed, named name_prefix and a " +
			"number. The deal depends only on the seed, the students and the groups, so the same seed deals the same " +
			"way; it is worked out whole first, and refused (no_room) if anyone would have nowhere to go. Adjust it " +
			"afterwards with group.set_members, or split again. It reaches every student of the course.",
		Kind: tool.Write, Gate: formGroups,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/group-sets/{set_id}/split"},
		Check: func(in GroupSplitIn) error {
			switch {
			case in.By != string(groupsplit.BySize) && in.By != string(groupsplit.ByCount):
				return errBadSplit("by is size or count")
			case in.N < 1 || in.N > maxGroupSize:
				return errBadSplit("n is 1 to %d", maxGroupSize)
			case in.From != splitFromUnassigned && in.From != splitFromAll:
				return errBadSplit("from is unassigned or all")
			case in.Seed != nil && !groupsplit.ValidSeed(*in.Seed):
				return errBadSplit("a seed is 1 to %d printable ASCII characters", groupsplit.MaxSeed)
			}
			if in.NamePrefix != nil && (utf8.RuneCountInString(*in.NamePrefix) > maxGroupName-4 || strings.ContainsFunc(*in.NamePrefix, unicode.IsControl)) {
				return apperr.Invalid("name_prefix is at most %d characters on one line", maxGroupName-4)
			}
			return checkCapacity(in.Capacity)
		},
		Resolve: func(ctx context.Context, q dbq.Querier, in GroupSplitIn) (tool.Target, error) {
			t, err := setTarget(ctx, q, in.CourseID, in.SetID)
			if err != nil {
				return t, err
			}
			// It reaches every student of the course.
			students, err := q.ListLiveStudents(ctx, in.CourseID)
			for _, st := range students {
				t.Scope.StudentMemberIDs = append(t.Scope.StudentMemberIDs, st.ID)
			}
			return t, err
		},
		Pin: func(_ context.Context, _ dbq.Querier, _ *domain.Member, _ time.Time, in GroupSplitIn) (GroupSplitIn, error) {
			if in.Seed == nil {
				seed := groupsplit.NewSeed()
				in.Seed = &seed
			}
			return in, nil
		},
		Validate: func(ctx context.Context, q dbq.Querier, m *domain.Member, _ time.Time, in GroupSplitIn) error {
			s, err := loadSet(ctx, q, in.CourseID, in.SetID)
			if err != nil {
				return err
			}
			seed := "validate"
			if in.Seed != nil {
				seed = *in.Seed
			}
			_, err = in.deal(ctx, q, m, s, seed, false)
			return err
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in GroupSplitIn) (GroupSplitOut, error) {
			s, err := ec.Q.LockGroupSet(ctx, dbq.LockGroupSetParams{ID: in.SetID, CourseID: in.CourseID})
			if err != nil {
				return GroupSplitOut{}, err
			}
			seed := groupsplit.NewSeed()
			if in.Seed != nil {
				seed = *in.Seed
			}
			d, err := in.deal(ctx, ec.Q, ec.Member, s, seed, true)
			if err != nil {
				return GroupSplitOut{}, err
			}
			out := GroupSplitOut{Seed: seed, Created: []SplitGroupOut{}, Placed: []SplitPlacedOut{}, Kept: []SplitKeptOut{}}
			for _, g := range d.kept {
				out.Kept = append(out.Kept, SplitKeptOut{GroupID: g, Reason: keptHasWork})
			}
			for _, g := range d.made {
				if err := ec.Q.InsertGroup(ctx, dbq.InsertGroupParams{ID: g.ID, CourseID: in.CourseID, SetID: s.ID, Name: g.Name,
					Capacity: in.Capacity, CreatedByMemberID: ec.Member.ID, CreatedAt: ec.Now}); err != nil {
					return GroupSplitOut{}, err
				}
				id := g.ID
				out.Created = append(out.Created, SplitGroupOut{GroupID: id, Name: g.Name})
				ec.Emit(events.Event{Type: EventGroupCreated, CourseID: &in.CourseID, SubjectType: subjectGroup, SubjectID: &id,
					Payload: map[string]any{"set_id": s.ID, "how": joinedSplit}})
			}
			// Emptied first, then dealt: a student dealt back into the
			// group they were in has a stay ended and a new one begun, both
			// by the split.
			for _, st := range d.emptied {
				mv := move{student: st.MemberID, stay: &st.ID, from: &st.GroupID}
				if err := moveStudent(ctx, ec, s, mv, joinedSplit); err != nil {
					return GroupSplitOut{}, err
				}
				out.Emptied++
			}
			for _, pl := range d.placed {
				to := pl.Group
				if err := moveStudent(ctx, ec, s, move{student: pl.Student, to: &to}, joinedSplit); err != nil {
					return GroupSplitOut{}, err
				}
				out.Placed = append(out.Placed, SplitPlacedOut{StudentMemberID: pl.Student, GroupID: pl.Group})
			}
			return out, nil
		},
	})
}

// newGroup is a group a split makes.
type newGroup struct {
	ID   uuid.UUID
	Name string
}

// splitDeal is a split worked out whole: the groups it keeps, those it
// makes, the stays it ends, and where each student goes.
type splitDeal struct {
	kept    []uuid.UUID
	made    []newGroup
	emptied []dbq.ListLiveMembershipsOfSetRow
	placed  []groupsplit.Placement
}

// deal works the split out, as m, over set s, refusing it whole when it may
// not be made: an archived set, a seat that does not reach every student,
// and a student with nowhere to go. lock takes the set's groups FOR UPDATE,
// in id order, as their stays are read.
func (in GroupSplitIn) deal(ctx context.Context, q dbq.Querier, m *domain.Member, s dbq.GroupSet, seed string, lock bool) (splitDeal, error) {
	var d splitDeal
	if s.ArchivedAt != nil {
		return d, errSetArchived()
	}
	groups, err := q.ListGroupsOfSets(ctx, []uuid.UUID{s.ID})
	if err != nil {
		return d, err
	}
	all := make([]uuid.UUID, 0, len(groups))
	for _, g := range groups {
		all = append(all, g.ID)
	}
	if lock && len(all) > 0 {
		slices.SortFunc(all, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
		if _, err := q.LockGroups(ctx, dbq.LockGroupsParams{SetID: s.ID, Ids: all}); err != nil {
			return d, err
		}
		if groups, err = q.ListGroupsOfSets(ctx, []uuid.UUID{s.ID}); err != nil {
			return d, err
		}
	}
	students, err := q.ListLiveStudents(ctx, s.CourseID)
	if err != nil {
		return d, err
	}
	studentIDs := make([]uuid.UUID, len(students))
	for i, st := range students {
		studentIDs[i] = st.ID
	}
	// Every student it may deal, as they are now: those seated since the
	// call was authorized among them.
	if reason, err := authz.CheckScope(ctx, q, m, authz.Target{StudentMemberIDs: studentIDs}); err != nil {
		return d, err
	} else if reason != authz.ReasonNone {
		return d, apperr.Forbid("a split reaches every student of the course, and yours does not").With("reason", string(reason))
	}
	work, err := groupWork(ctx, q, all, false)
	if err != nil {
		return d, err
	}
	hasWork := map[uuid.UUID]bool{}
	for _, w := range work {
		hasWork[w.GroupID] = true
	}
	stays, err := q.ListLiveMembershipsOfSet(ctx, s.ID)
	if err != nil {
		return d, err
	}
	placedNow := map[uuid.UUID]uuid.UUID{} // student → group, counted live
	for _, st := range stays {
		placedNow[st.MemberID] = st.GroupID
	}
	members := map[uuid.UUID]int{} // what each eligible group keeps
	notArchived, names := 0, map[string]bool{}
	var eligible []groupsplit.Group
	for _, g := range groups {
		if g.ArchivedAt != nil {
			continue
		}
		notArchived++
		names[strings.ToLower(g.Name)] = true
		if hasWork[g.ID] {
			d.kept = append(d.kept, g.ID)
			continue
		}
		cp := 0
		if g.Capacity != nil {
			cp = int(*g.Capacity)
		}
		eligible = append(eligible, groupsplit.Group{ID: g.ID, CreatedAt: g.CreatedAt, Capacity: cp})
	}
	isEligible := map[uuid.UUID]bool{}
	for _, g := range eligible {
		isEligible[g.ID] = true
	}
	var toDeal []uuid.UUID
	for _, st := range studentIDs {
		g, placed := placedNow[st]
		switch {
		case !placed:
			toDeal = append(toDeal, st)
		case in.From == splitFromAll && isEligible[g]:
			toDeal = append(toDeal, st)
		case isEligible[g]:
			members[g]++
		}
	}
	if in.From == splitFromAll {
		for _, st := range stays {
			if isEligible[st.GroupID] {
				d.emptied = append(d.emptied, st)
			}
		}
	}
	kept := 0
	for i := range eligible {
		eligible[i].Members = members[eligible[i].ID]
		kept += eligible[i].Members
	}
	n := groupsplit.NewGroups(groupsplit.By(in.By), in.N, notArchived, len(eligible), kept, len(toDeal))
	prefix := defaultSplitPrefix
	if in.NamePrefix != nil {
		prefix = *in.NamePrefix
	}
	for k := 1; len(d.made) < n; k++ {
		name := cleanName(prefix + strconv.Itoa(k))
		if names[strings.ToLower(name)] {
			continue
		}
		names[strings.ToLower(name)] = true
		g := newGroup{ID: ids.New(), Name: name}
		d.made = append(d.made, g)
		cp := 0
		if in.Capacity != nil {
			cp = int(*in.Capacity)
		}
		// Made now, after every group there is, in the order they are
		// numbered.
		eligible = append(eligible, groupsplit.Group{ID: g.ID, CreatedAt: madeBySplit.Add(time.Duration(len(d.made))), Capacity: cp})
	}
	limit := 0
	if in.By == string(groupsplit.BySize) {
		limit = in.N
	}
	d.placed, err = groupsplit.Deal(toDeal, eligible, limit, seed)
	if errors.Is(err, groupsplit.ErrNoRoom) {
		return d, apperr.Precondition("%d students to place, and the groups that take students are full: raise their capacity, or split by size",
			len(toDeal)).With("reason", ReasonNoRoom)
	}
	return d, err
}

// ---------------------------------------------------------------------------
// Signing oneself up: group.sign_up
// ---------------------------------------------------------------------------

type GroupSignUpIn struct {
	GroupSetIDIn
	GroupID         *uuid.UUID `json:"group_id,omitempty" jsonschema:"the group to join, or to switch to from one's own; absent leaves one's group"`
	StudentMemberID *uuid.UUID `json:"student_member_id,omitempty" jsonschema:"the student signing up; defaults to the caller, or, for a student's own agent, its student"`
}

type GroupSignUpOut struct {
	GroupID     *uuid.UUID `json:"group_id,omitempty" jsonschema:"the group they are in now; absent when in none"`
	LeftGroupID *uuid.UUID `json:"left_group_id,omitempty" jsonschema:"the group they left"`
	Changed     bool       `json:"changed" jsonschema:"false when they were already where they asked to be"`
}

// signer is the student a sign-up is for: the one named, or the caller's own
// (for a student's agent, its student), whom the caller's scope must reach.
func (in GroupSignUpIn) signer(ctx context.Context, q dbq.Querier, m *domain.Member) (uuid.UUID, error) {
	if in.StudentMemberID != nil {
		return *in.StudentMemberID, nil
	}
	self := selfOf(m)
	if reason, err := authz.CheckScope(ctx, q, m, authz.Target{StudentMemberIDs: []uuid.UUID{self}}); err != nil {
		return self, err
	} else if reason != authz.ReasonNone {
		return self, apperr.Forbid("you are outside your own student scope").With("reason", string(reason))
	}
	return self, nil
}

func groupSignUp() tool.Tool {
	return tool.Define(tool.Spec[GroupSignUpIn, GroupSignUpOut]{
		Name: "group.sign_up",
		Description: "A student signs themselves up to a group of a set the teacher opened: joins it, switches to it from " +
			"their group, or, with no group_id, leaves their group. Only while the set's sign-up is open and before its " +
			"deadline (signup_closed), to a group below its capacity (group_full), and never out of or into a group that " +
			"has handed work in for an assignment of the set (your_group_has_work, group_has_work): who did handed-in " +
			"work is the teacher's to change. A student's own agent signs up for its student by proposal, which the " +
			"student confirms.",
		Kind: tool.Write, Gate: writeSubmissions,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/group-sets/{set_id}/sign-up"},
		Resolve: func(ctx context.Context, q dbq.Querier, in GroupSignUpIn) (tool.Target, error) {
			t, err := setTarget(ctx, q, in.CourseID, in.SetID)
			if in.StudentMemberID != nil {
				t.Scope.StudentMemberIDs = []uuid.UUID{*in.StudentMemberID}
			}
			// When it is the caller's own, who that is is known only as the
			// call runs; signer checks their scope itself.
			return t, err
		},
		Validate: func(ctx context.Context, q dbq.Querier, m *domain.Member, now time.Time, in GroupSignUpIn) error {
			s, err := loadSet(ctx, q, in.CourseID, in.SetID)
			if err != nil {
				return err
			}
			student, err := in.signer(ctx, q, m)
			if err != nil {
				return err
			}
			_, err = in.plan(ctx, q, s, student, now, false)
			return err
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in GroupSignUpIn) (GroupSignUpOut, error) {
			s, err := ec.Q.ShareGroupSet(ctx, dbq.ShareGroupSetParams{ID: in.SetID, CourseID: in.CourseID})
			if err != nil {
				return GroupSignUpOut{}, err
			}
			student, err := in.signer(ctx, ec.Q, ec.Member)
			if err != nil {
				return GroupSignUpOut{}, err
			}
			mv, err := in.plan(ctx, ec.Q, s, student, ec.Now, true)
			if err != nil {
				return GroupSignUpOut{}, err
			}
			if mv == nil {
				return GroupSignUpOut{GroupID: in.GroupID}, nil
			}
			if err := moveStudent(ctx, ec, s, *mv, joinedSignup); err != nil {
				return GroupSignUpOut{}, err
			}
			return GroupSignUpOut{GroupID: mv.to, LeftGroupID: mv.from, Changed: true}, nil
		},
	})
}

// plan works out student's sign-up to set s as of now, nil when there is
// nothing to do, refusing what sign-up may not do. lock takes the groups
// left and joined FOR UPDATE, in id order, and counts under it.
func (in GroupSignUpIn) plan(ctx context.Context, q dbq.Querier, s dbq.GroupSet, student uuid.UUID, now time.Time, lock bool) (*move, error) {
	if s.ArchivedAt != nil {
		return nil, errSetArchived()
	}
	if !s.SignupOpen || (s.SignupClosesAt != nil && !now.Before(*s.SignupClosesAt)) {
		e := apperr.Precondition("sign-up to this set is closed; the teacher places students now").With("reason", ReasonSignupClosed)
		if s.SignupClosesAt != nil {
			e = e.With("closes_at", *s.SignupClosesAt)
		}
		return nil, e
	}
	if err := liveStudents(ctx, q, s.CourseID, []uuid.UUID{student}); err != nil {
		return nil, err
	}
	mv := move{student: student, to: in.GroupID}
	stay, err := q.CurrentMembership(ctx, dbq.CurrentMembershipParams{SetID: s.ID, MemberID: student})
	switch {
	case err == nil:
		mv.stay, mv.from = &stay.ID, &stay.GroupID
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}
	if sameID(mv.from, mv.to) {
		return nil, nil
	}
	var touched []uuid.UUID
	for _, g := range []*uuid.UUID{mv.from, mv.to} {
		if g != nil {
			touched = append(touched, *g)
		}
	}
	slices.SortFunc(touched, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	if lock {
		if _, err := q.LockGroups(ctx, dbq.LockGroupsParams{SetID: s.ID, Ids: touched}); err != nil {
			return nil, err
		}
	}
	if mv.to != nil {
		g, err := loadGroup(ctx, q, s.CourseID, *mv.to)
		if err != nil || g.SetID != s.ID {
			return nil, apperr.Missing("no such group in this set")
		}
		if g.ArchivedAt != nil {
			return nil, errGroupArchived(g.ID)
		}
		if g.Capacity != nil && g.Size >= *g.Capacity {
			return nil, apperr.Precondition("the group is full").With("reason", ReasonGroupFull).With("capacity", *g.Capacity)
		}
	}
	if mv.from != nil {
		if work, err := groupWork(ctx, q, []uuid.UUID{*mv.from}, true); err != nil {
			return nil, err
		} else if len(work) > 0 {
			return nil, errGroupHasWork(ReasonYourGroupHasWork, msgYourGroupWork, work)
		}
	}
	if mv.to != nil {
		if work, err := groupWork(ctx, q, []uuid.UUID{*mv.to}, true); err != nil {
			return nil, err
		} else if len(work) > 0 {
			return nil, errGroupHasWork(ReasonGroupHasWork, msgTargetGroupWork, work)
		}
	}
	return &mv, nil
}

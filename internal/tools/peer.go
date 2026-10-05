package tools

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/events"
	"github.com/AIShie-Education/AIShie-Core/internal/peercalc"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

// Peer evaluation within a group (docs/schema.md §2.5b, Peer evaluation). A
// group assignment may have a peer form: the members of each group's circle
// evaluate each other's contribution — rating each on criteria, or splitting
// 100 points among them — within a window, each keeping one current sheet,
// replaced as a whole while the window is open. Those who grade read every
// sheet, with who wrote it; no student reads another's, or anything said of
// themselves, or who rated them. Counted at the form's weight, what each
// member received moves their grade from the group's, as an adjustment of
// its own kind (peer), worked out by package peercalc; a grader's own
// adjustment wins over it.

func peerTools() []tool.Tool {
	return []tool.Tool{peerFormGet(), peerFormSet(), peerReviewSubmit(), peerReviewResults(), gradeApplyPeer()}
}

const (
	EventPeerFormUpdated     = "peer_form.updated"
	EventPeerReviewSubmitted = "peer_review.submitted"
)

// Why a call about peer evaluation is refused, in error.details.reason.
const (
	ReasonNoPeerForm        = "no_peer_form"
	ReasonPeerFormDisabled  = "peer_form_disabled"
	ReasonPeerFormExists    = "peer_form_exists"
	ReasonFormInUse         = "form_in_use"
	ReasonWindowNotOpen     = "window_not_open"
	ReasonWindowClosed      = "window_closed"
	ReasonWindowOpen        = "window_open"
	ReasonNotInCircle       = "not_in_circle"
	ReasonPeerNotCounted    = "peer_not_counted"
	ReasonGradesChanged     = "grades_changed"
	ReasonSheetIncomplete   = "sheet_incomplete"
	ReasonBadShareTotal     = "bad_share_total"
	ReasonBadRating         = "bad_rating"
	ReasonSelfEvaluationOff = "self_evaluation_off"
	// PeerPeopleOnly: a peer evaluation is a person's judgment of their
	// classmates, and an agent writes none.
	PeerPeopleOnly = "people_only"
)

const (
	peerOpensOnHandIn, peerOpensAt     = "on_hand_in", "at"
	peerShareNone, peerShareOwnAverage = "none", "own_average"
	windowNotOpen, windowOpen          = "not_open", "open"
	windowClosed                       = "closed"
	// How peer evaluation stands in a grade written now (the peer field of
	// what grade.submit, .regrade and .adjust return).
	peerCounted = "counted"

	maxCriteria          = 10
	maxCriterionLabel    = 200
	maxCriterionDescribe = 1000
	maxSheetComment      = 2000
	maxEntryComment      = 1000
	maxScale             = 10
)

var criterionKey = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

var (
	criterionWeightMin, criterionWeightMax = decimal.RequireFromString("0.1"), decimal.NewFromInt(10)
)

// ---------------------------------------------------------------------------
// The form, as it is kept and shown
// ---------------------------------------------------------------------------

// PeerCriterion is one criterion of a rating form.
type PeerCriterion struct {
	Key         string           `json:"key" jsonschema:"1 to 32 of a-z, 0-9 and _, unique in the form: the ratings name it"`
	Label       string           `json:"label" jsonschema:"what is rated, 1 to 200 characters"`
	Description *string          `json:"description,omitempty" jsonschema:"at most 1000 characters"`
	Weight      *decimal.Decimal `json:"weight,omitempty" jsonschema:"0.1 to 10: its weight in what a rater gives a member, against the others'; 1 if omitted"`
}

// PeerFormView is a peer form, and what it tells everyone of who sees what.
type PeerFormView struct {
	AssignmentID      uuid.UUID       `json:"assignment_id"`
	Enabled           bool            `json:"enabled" jsonschema:"false: no sheet is written, and it does not count; what was written is kept"`
	Kind              string          `json:"kind" jsonschema:"rating: each member rated on each criterion, a whole number on the scale; share: each rater splits 100 points among those they evaluate"`
	Criteria          []PeerCriterion `json:"criteria,omitempty"`
	ScaleMin          *int32          `json:"scale_min,omitempty"`
	ScaleMax          *int32          `json:"scale_max,omitempty"`
	SelfEvaluation    bool            `json:"self_evaluation" jsonschema:"members evaluate themselves too"`
	Opens             string          `json:"opens" jsonschema:"on_hand_in: for each group once it has handed work in; at: at opens_at"`
	OpensAt           *time.Time      `json:"opens_at,omitempty"`
	ClosesAt          time.Time       `json:"closes_at"`
	Weight            int32           `json:"weight" jsonschema:"the percentage of each member's grade what they received moves; 0: reference only"`
	ShareWithStudents string          `json:"share_with_students" jsonschema:"none, or own_average: after it closes, a member reads their own average from two peers or more, never a comment"`
	Version           int32           `json:"version" jsonschema:"changed by every change: peer_form.set is made over it"`
	InUse             bool            `json:"in_use" jsonschema:"a sheet has been written: its kind, criteria, scale and self-evaluation no longer change"`
	// What the form tells everyone of who sees what (docs/schema.md §2.5b).
	VisibleTo   []string  `json:"visible_to" jsonschema:"who reads every sheet, with who wrote it: graders, those who grade; action_record, those who decide actions, in the action log"`
	StudentsSee []string  `json:"students_see" jsonschema:"what a student reads: own_sheet, their own current sheet; own_average, their own average once it closes; own_adjustment, their own grade's peer adjustment, where it counts. Never another's sheet, what was said of them, or who rated them"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func viewPeerForm(f dbq.PeerForm, inUse bool) (PeerFormView, error) {
	v := PeerFormView{AssignmentID: f.AssignmentID, Enabled: f.Enabled, Kind: f.Kind, ScaleMin: f.ScaleMin, ScaleMax: f.ScaleMax,
		SelfEvaluation: f.SelfEvaluation, Opens: f.Opens, OpensAt: f.OpensAt, ClosesAt: f.ClosesAt, Weight: f.Weight,
		ShareWithStudents: f.ShareWithStudents, Version: f.Version, InUse: inUse, UpdatedAt: f.UpdatedAt,
		VisibleTo: []string{"graders", "action_record"}, StudentsSee: []string{"own_sheet"}}
	if len(f.Criteria) > 0 {
		if err := json.Unmarshal(f.Criteria, &v.Criteria); err != nil {
			return v, err
		}
	}
	if f.ShareWithStudents == peerShareOwnAverage {
		v.StudentsSee = append(v.StudentsSee, "own_average")
	}
	if f.Enabled && f.Weight > 0 {
		v.StudentsSee = append(v.StudentsSee, "own_adjustment")
	}
	return v, nil
}

// calcForm is what peercalc needs of a form.
func calcForm(f dbq.PeerForm) (peercalc.Form, error) {
	out := peercalc.Form{Kind: f.Kind, SelfEvaluation: f.SelfEvaluation}
	if len(f.Criteria) == 0 {
		return out, nil
	}
	var criteria []PeerCriterion
	if err := json.Unmarshal(f.Criteria, &criteria); err != nil {
		return out, err
	}
	for _, c := range criteria {
		w := decimal.NewFromInt(1)
		if c.Weight != nil {
			w = *c.Weight
		}
		out.Criteria = append(out.Criteria, peercalc.Criterion{Key: c.Key, Weight: w})
	}
	return out, nil
}

// loadPeerForm is the assignment's form, nil when it has none.
func loadPeerForm(ctx context.Context, q dbq.Querier, courseID, assignmentID uuid.UUID) (*dbq.PeerForm, error) {
	f, err := q.GetPeerForm(ctx, dbq.GetPeerFormParams{AssignmentID: assignmentID, CourseID: courseID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// sharePeerForm is the assignment's form held FOR SHARE, nil when it has
// none.
func sharePeerForm(ctx context.Context, q dbq.Querier, courseID, assignmentID uuid.UUID) (*dbq.PeerForm, error) {
	f, err := q.SharePeerForm(ctx, dbq.SharePeerFormParams{AssignmentID: assignmentID, CourseID: courseID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

func errNoPeerForm() error {
	return apperr.Precondition("this assignment has no peer form").With("reason", ReasonNoPeerForm)
}

// counts says whether the form moves grades: enabled, with a weight.
func formCounts(f *dbq.PeerForm) bool { return f != nil && f.Enabled && f.Weight > 0 }

// isNoRows says whether err is a lookup that found nothing.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// formClosed says whether its window has closed, for every group.
func formClosed(f dbq.PeerForm, now time.Time) bool { return !now.Before(f.ClosesAt) }

// ---------------------------------------------------------------------------
// Circles and windows
// ---------------------------------------------------------------------------

// circle is a group's circle for an assignment: who evaluates whom. The
// members of its latest work that is not a draft (handed in or recorded
// missing), or, with none, its members now less those another group's work
// for the assignment names; handedIn says whether it has handed work in, on
// time or late, which opens a form that opens on hand-in.
type circle struct {
	group    uuid.UUID
	members  []uuid.UUID
	work     *dbq.LatestHandedWorkOfGroupRow
	handedIn bool
}

func (c circle) has(m uuid.UUID) bool { return slices.Contains(c.members, m) }

func circleOf(ctx context.Context, q dbq.Querier, assignment, group uuid.UUID) (circle, error) {
	c := circle{group: group}
	w, err := q.LatestHandedWorkOfGroup(ctx, dbq.LatestHandedWorkOfGroupParams{AssignmentID: assignment, GroupID: group})
	switch {
	case err == nil:
		c.work = &w
		if c.members, err = workStudents(ctx, q, w.ID); err != nil {
			return c, err
		}
	case errors.Is(err, pgx.ErrNoRows):
		if c.members, _, err = membersFor(ctx, q, assignment, group); err != nil {
			return c, err
		}
	default:
		return c, err
	}
	c.members = sortedMembers(c.members)
	c.handedIn, err = q.GroupHasHandedIn(ctx, dbq.GroupHasHandedInParams{AssignmentID: assignment, GroupID: group})
	return c, err
}

// circleFor is the circle a student evaluates in for assignment a: the one
// of a group whose work names them, or else of their group of its set now;
// nil when they are in neither. in says whether the circle holds them: one
// who joined a group after its work was handed in is in its group, and not
// in its circle.
func circleFor(ctx context.Context, q dbq.Querier, a dbq.GetAssignmentInCourseRow, student uuid.UUID) (c *circle, in bool, err error) {
	if a.GroupSetID == nil {
		return nil, false, nil
	}
	named, err := q.WorkGroupsNaming(ctx, dbq.WorkGroupsNamingParams{AssignmentID: a.ID, MemberID: student})
	if err != nil {
		return nil, false, err
	}
	for _, g := range named {
		got, err := circleOf(ctx, q, a.ID, g)
		if err != nil {
			return nil, false, err
		}
		if got.has(student) {
			return &got, true, nil
		}
	}
	now, err := groupOf(ctx, q, *a.GroupSetID, student)
	if err != nil || now == nil {
		return nil, false, err
	}
	got, err := circleOf(ctx, q, a.ID, now.ID)
	if err != nil {
		return nil, false, err
	}
	return &got, got.has(student), nil
}

// windowOf is the form's window for a group: open once it opens — at
// opens_at, or once the group has handed work in — until closes_at.
func windowOf(f dbq.PeerForm, handedIn bool, now time.Time) string {
	switch {
	case formClosed(f, now):
		return windowClosed
	case f.Opens == peerOpensAt && f.OpensAt != nil && now.Before(*f.OpensAt):
		return windowNotOpen
	case f.Opens == peerOpensOnHandIn && !handedIn:
		return windowNotOpen
	}
	return windowOpen
}

// PeerWindow is a form's window for a group.
type PeerWindow struct {
	State    string     `json:"state" jsonschema:"not_open, open or closed"`
	Opens    string     `json:"opens" jsonschema:"on_hand_in or at"`
	OpensAt  *time.Time `json:"opens_at,omitempty"`
	ClosesAt time.Time  `json:"closes_at"`
}

func viewWindow(f dbq.PeerForm, handedIn bool, now time.Time) PeerWindow {
	return PeerWindow{State: windowOf(f, handedIn, now), Opens: f.Opens, OpensAt: f.OpensAt, ClosesAt: f.ClosesAt}
}

// ---------------------------------------------------------------------------
// Sheets
// ---------------------------------------------------------------------------

// PeerEntryView is what a sheet gives one member.
type PeerEntryView struct {
	StudentMemberID uuid.UUID      `json:"student_member_id"`
	Ratings         map[string]int `json:"ratings,omitempty"`
	Share           *int32         `json:"share,omitempty"`
	Comment         *string        `json:"comment,omitempty"`
}

// PeerSheetView is a rater's current sheet.
type PeerSheetView struct {
	ReviewID      uuid.UUID       `json:"review_id"`
	RaterMemberID uuid.UUID       `json:"rater_member_id"`
	RaterName     *string         `json:"rater_name,omitempty" jsonschema:"to those who grade"`
	GroupID       uuid.UUID       `json:"group_id" jsonschema:"the group whose circle it was written in"`
	Comment       *string         `json:"comment,omitempty"`
	SubmittedAt   time.Time       `json:"submitted_at"`
	Entries       []PeerEntryView `json:"entries"`
}

// currentSheets are the current sheets of raters for assignment, in rater
// order, entries in member order.
func currentSheets(ctx context.Context, q dbq.Querier, assignment uuid.UUID, raters []uuid.UUID) ([]PeerSheetView, error) {
	if len(raters) == 0 {
		return nil, nil
	}
	rows, err := q.ListCurrentPeerSheets(ctx, dbq.ListCurrentPeerSheetsParams{AssignmentID: assignment, RaterIds: raters})
	if err != nil {
		return nil, err
	}
	var out []PeerSheetView
	for _, r := range rows {
		if len(out) == 0 || out[len(out)-1].ReviewID != r.ID {
			name := r.RaterName
			out = append(out, PeerSheetView{ReviewID: r.ID, RaterMemberID: r.RaterMemberID, RaterName: &name, GroupID: r.GroupID,
				Comment: r.Comment, SubmittedAt: r.CreatedAt})
		}
		e := PeerEntryView{StudentMemberID: r.RateeMemberID, Share: r.Share, Comment: r.EntryComment}
		if len(r.Ratings) > 0 {
			if err := json.Unmarshal(r.Ratings, &e.Ratings); err != nil {
				return nil, err
			}
		}
		s := &out[len(out)-1]
		s.Entries = append(s.Entries, e)
	}
	return out, nil
}

// calcSheets are sheets as peercalc takes them.
func calcSheets(sheets []PeerSheetView) []peercalc.Sheet {
	out := make([]peercalc.Sheet, 0, len(sheets))
	for _, s := range sheets {
		cs := peercalc.Sheet{Rater: s.RaterMemberID, Entries: map[uuid.UUID]peercalc.Entry{}}
		for _, e := range s.Entries {
			ce := peercalc.Entry{Ratings: e.Ratings}
			if e.Share != nil {
				ce.Share = int(*e.Share)
			}
			cs.Entries[e.StudentMemberID] = ce
		}
		out = append(out, cs)
	}
	return out
}

// ---------------------------------------------------------------------------
// Counting it in a member's grade
// ---------------------------------------------------------------------------

// peerCount is how peer evaluation counts in the grades of one group's work
// written now: not at all (state ""), not yet (window_open), or counted, from
// what the group's circle received.
type peerCount struct {
	state   string
	weight  int32
	version int32
	result  peercalc.Result
}

// loadPeerCount works out how peer evaluation counts in the grades of
// group's work for assignment a written at now, as a call writes them:
// counted when the form is enabled, has a weight and its window has closed.
func loadPeerCount(ctx context.Context, q dbq.Querier, courseID uuid.UUID, a uuid.UUID, group *uuid.UUID, now time.Time) (peerCount, error) {
	if group == nil {
		return peerCount{}, nil
	}
	// Held FOR SHARE: the grade is written against the form as it stands,
	// which a change of it waits for.
	f, err := sharePeerForm(ctx, q, courseID, a)
	if err != nil || !formCounts(f) {
		return peerCount{}, err
	}
	if !formClosed(*f, now) {
		return peerCount{state: ReasonWindowOpen}, nil
	}
	return countFor(ctx, q, *f, *group)
}

// countFor is what group's circle received on form f, counted.
func countFor(ctx context.Context, q dbq.Querier, f dbq.PeerForm, group uuid.UUID) (peerCount, error) {
	pc := peerCount{state: peerCounted, weight: f.Weight, version: f.Version}
	form, err := calcForm(f)
	if err != nil {
		return pc, err
	}
	c, err := circleOf(ctx, q, f.AssignmentID, group)
	if err != nil {
		return pc, err
	}
	sheets, err := currentSheets(ctx, q, f.AssignmentID, c.members)
	if err != nil {
		return pc, err
	}
	pc.result = peercalc.Compute(form, c.members, calcSheets(sheets))
	return pc, nil
}

// adjust is member's adjustment once peer evaluation counts in it: a
// grader's own adjustment wins; otherwise, counted, what the member received
// moves their score from group score g at the form's weight, held to zero
// and to points unless allowExtra. A member nobody rated is given none.
func (p peerCount) adjust(member uuid.UUID, own adjustment, g, points decimal.Decimal, allowExtra bool) adjustment {
	if !own.none() || p.state != peerCounted {
		return own
	}
	m, ok := p.result.Members[member]
	if !ok || m.Raters == 0 {
		return adjustment{}
	}
	return peerAdjustment(g, points, allowExtra, m.Factor, p.weight, m.Raters, p.version)
}

func peerAdjustment(g, points decimal.Decimal, allowExtra bool, factor decimal.Decimal, weight int32, raters int, version int32) adjustment {
	s := peercalc.Score(g, points, weight, factor, allowExtra)
	return adjustment{kind: adjustPeer, points: s.Sub(g), detail: &PeerDetail{Factor: factor, Weight: weight, Raters: &raters, FormVersion: &version}}
}

// rescaled is a peer adjustment carried to group score g out of points: the
// score worked out again from its recorded factor, at its recorded weight;
// one with no record (the release before's) in proportion, as a delta.
func (a adjustment) rescaledPeer(g, points decimal.Decimal, allowExtra bool, from, to decimal.Decimal) adjustment {
	if a.detail == nil {
		a.points = rescaledScore(a.points, from, to)
		return a
	}
	raters, version := 0, int32(0)
	if a.detail.Raters != nil {
		raters = *a.detail.Raters
	}
	if a.detail.FormVersion != nil {
		version = *a.detail.FormVersion
	}
	return peerAdjustment(g, points, allowExtra, a.detail.Factor, a.detail.Weight, raters, version)
}

// ---------------------------------------------------------------------------
// What a form's arguments say alone
// ---------------------------------------------------------------------------

func checkCriteria(criteria []PeerCriterion) error {
	if len(criteria) == 0 || len(criteria) > maxCriteria {
		return apperr.Invalid("a rating form has 1 to %d criteria", maxCriteria)
	}
	seen := map[string]bool{}
	for _, c := range criteria {
		if !criterionKey.MatchString(c.Key) {
			return apperr.Invalid("a criterion's key is 1 to 32 of a-z, 0-9 and _: %q", c.Key)
		}
		if seen[c.Key] {
			return apperr.Invalid("two criteria have the key %q", c.Key)
		}
		seen[c.Key] = true
		label := strings.TrimSpace(c.Label)
		if label == "" || utf8.RuneCountInString(label) > maxCriterionLabel {
			return apperr.Invalid("a criterion's label is 1 to %d characters", maxCriterionLabel)
		}
		if c.Description != nil && utf8.RuneCountInString(*c.Description) > maxCriterionDescribe {
			return apperr.Invalid("a criterion's description is at most %d characters", maxCriterionDescribe)
		}
		if c.Weight != nil && (c.Weight.LessThan(criterionWeightMin) || c.Weight.GreaterThan(criterionWeightMax)) {
			return apperr.Invalid("a criterion's weight is 0.1 to 10")
		}
	}
	return nil
}

// keptCriteria are criteria as they are kept: labels trimmed, an empty
// description none, every weight said.
func keptCriteria(criteria []PeerCriterion) ([]byte, error) {
	if len(criteria) == 0 {
		return nil, nil
	}
	out := make([]PeerCriterion, len(criteria))
	for i, c := range criteria {
		w := decimal.NewFromInt(1)
		if c.Weight != nil {
			w = *c.Weight
		}
		k := PeerCriterion{Key: c.Key, Label: strings.TrimSpace(c.Label), Weight: &w}
		if c.Description != nil && strings.TrimSpace(*c.Description) != "" {
			d := *c.Description
			k.Description = &d
		}
		out[i] = k
	}
	return json.Marshal(out)
}

// ---------------------------------------------------------------------------
// peer_form.get
// ---------------------------------------------------------------------------

// CircleMember is a member of a student's circle, by name: people who hand
// work in together know each other's names.
type CircleMember struct {
	MemberID    uuid.UUID `json:"member_id"`
	DisplayName string    `json:"display_name"`
}

// OwnAverage is what a member received from their peers, shown to them once
// the window has closed, from two peers or more, where the form shares it:
// for a rating form their average on each criterion, for a share form what
// they received against an even share, as a percentage. Never a comment, and
// never who gave what.
type OwnAverage struct {
	Averages     map[string]decimal.Decimal `json:"averages,omitempty" jsonschema:"a rating form: the average from peers on each criterion, to two places"`
	SharePercent *decimal.Decimal           `json:"share_percent,omitempty" jsonschema:"a share form: what peers gave, against an even share, as a percentage; 100 is even"`
}

// PeerTask is a student's part in peer evaluation: their group's circle,
// whom they evaluate, the window, their own current sheet and, where shared,
// their own average.
type PeerTask struct {
	GroupID    uuid.UUID      `json:"group_id"`
	GroupName  string         `json:"group_name"`
	InCircle   bool           `json:"in_circle" jsonschema:"whether the student evaluates here: one who joined the group after its work was handed in does not"`
	Circle     []CircleMember `json:"circle" jsonschema:"who evaluates whom: the members of the group's latest work handed in or recorded missing, or, with none, its members now"`
	ToEvaluate []uuid.UUID    `json:"to_evaluate" jsonschema:"whom the student's sheet covers, exactly: every other member of the circle, and themselves with self-evaluation on"`
	Window     PeerWindow     `json:"window"`
	Sheet      *PeerSheetView `json:"sheet,omitempty" jsonschema:"their current sheet, if they have written one"`
	OwnAverage *OwnAverage    `json:"own_average,omitempty"`
}

type PeerFormGetOut struct {
	Form *PeerFormView `json:"form" jsonschema:"null when the assignment has none"`
	Task *PeerTask     `json:"task,omitempty" jsonschema:"for a student in a group of the assignment's set, or a student's own agent: the student's part"`
}

func peerFormGet() tool.Tool {
	return tool.Define(tool.Spec[AssignmentIDIn, PeerFormGetOut]{
		Name: "peer_form.get",
		Description: "An assignment's peer form, if it has one: how members of a group evaluate each other (rating on criteria, or " +
			"splitting 100 points), the window, the weight it counts at in each member's grade, and who sees what: those who " +
			"grade read every sheet with who wrote it, as does the action log; a student reads their own sheet, their own " +
			"average once it closes if the form shares it, and their own grade's adjustment where it counts, never another's " +
			"sheet, what was said of them, or who rated them. For a student in a group of the assignment's set (or a student's " +
			"own agent), their task: the circle, whom they evaluate, the window's state, their current sheet.",
		Kind: tool.Read, Gate: tool.Gate{Perms: []domain.Perm{domain.PermDocumentRead}},
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/assignments/{assignment_id}/peer-form"},
		Resolve: func(ctx context.Context, q dbq.Querier, in AssignmentIDIn) (tool.Target, error) {
			return assignmentTarget(ctx, q, in.CourseID, in.AssignmentID)
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in AssignmentIDIn) (PeerFormGetOut, error) {
			a, err := rc.Q.GetAssignmentInCourse(ctx, dbq.GetAssignmentInCourseParams{ID: in.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return PeerFormGetOut{}, goneIfNoRows(ctx, rc.Q, in.CourseID, in.AssignmentID, err)
			}
			if a.PublishedAt == nil && !canSeeUnpublished(rc.Member) {
				return PeerFormGetOut{}, apperr.Missing("no such assignment in this course")
			}
			f, err := loadPeerForm(ctx, rc.Q, in.CourseID, a.ID)
			if err != nil || f == nil {
				return PeerFormGetOut{}, err
			}
			inUse, err := rc.Q.PeerFormInUse(ctx, a.ID)
			if err != nil {
				return PeerFormGetOut{}, err
			}
			v, err := viewPeerForm(*f, inUse)
			if err != nil {
				return PeerFormGetOut{}, err
			}
			out := PeerFormGetOut{Form: &v}
			out.Task, err = peerTask(ctx, rc, a, *f)
			return out, err
		},
	})
}

// peerTask is the caller's own part in peer evaluation, or a student's own
// agent's student's: nil for anyone in no group of the set.
func peerTask(ctx context.Context, rc *tool.ReadCtx, a dbq.GetAssignmentInCourseRow, f dbq.PeerForm) (*PeerTask, error) {
	self := selfOf(rc.Member)
	c, in, err := circleFor(ctx, rc.Q, a, self)
	if err != nil || c == nil {
		return nil, err
	}
	g, err := loadGroup(ctx, rc.Q, a.CourseID, c.group)
	if err != nil {
		return nil, err
	}
	t := &PeerTask{GroupID: g.ID, GroupName: g.Name, InCircle: in, Circle: []CircleMember{}, ToEvaluate: []uuid.UUID{},
		Window: viewWindow(f, c.handedIn, rc.Now)}
	if len(c.members) > 0 {
		names, err := rc.Q.NamesOfMembers(ctx, c.members)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			t.Circle = append(t.Circle, CircleMember{MemberID: n.ID, DisplayName: n.DisplayName})
		}
	}
	if !in {
		return t, nil
	}
	t.ToEvaluate = toEvaluate(f, c.members, self)
	sheets, err := currentSheets(ctx, rc.Q, a.ID, []uuid.UUID{self})
	if err != nil {
		return nil, err
	}
	if len(sheets) == 1 {
		s := sheets[0]
		s.RaterName = nil
		t.Sheet = &s
	}
	if f.ShareWithStudents == peerShareOwnAverage && formClosed(f, rc.Now) {
		form, err := calcForm(f)
		if err != nil {
			return nil, err
		}
		all, err := currentSheets(ctx, rc.Q, a.ID, c.members)
		if err != nil {
			return nil, err
		}
		// From two peers or more, so that no one rater's judgment is told.
		if got := peercalc.ReceivedBy(form, c.members, calcSheets(all))[self]; got.PeerRaters >= 2 {
			t.OwnAverage = &OwnAverage{Averages: got.Averages, SharePercent: got.SharePercent}
		}
	}
	return t, nil
}

// toEvaluate is whom rater's sheet covers in a circle of members: every
// other member, and themselves with self-evaluation on.
func toEvaluate(f dbq.PeerForm, members []uuid.UUID, rater uuid.UUID) []uuid.UUID {
	out := []uuid.UUID{}
	for _, m := range members {
		if m != rater || f.SelfEvaluation {
			out = append(out, m)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// peer_form.set
// ---------------------------------------------------------------------------

type PeerFormSetIn struct {
	tool.InCourse
	AssignmentID      uuid.UUID       `json:"assignment_id" jsonschema:"a group assignment"`
	Enabled           *bool           `json:"enabled,omitempty" jsonschema:"false stops new sheets and stops it counting, keeping what was written; true if omitted on a new form, and as it was on a form there is"`
	Kind              string          `json:"kind" jsonschema:"rating: each member rated on each criterion, a whole number on the scale; share: each rater splits 100 points among those they evaluate"`
	Criteria          []PeerCriterion `json:"criteria,omitempty" jsonschema:"rating: 1 to 10 criteria"`
	ScaleMin          *int32          `json:"scale_min,omitempty" jsonschema:"rating: 0 or 1"`
	ScaleMax          *int32          `json:"scale_max,omitempty" jsonschema:"rating: above scale_min, at most 10"`
	SelfEvaluation    *bool           `json:"self_evaluation,omitempty" jsonschema:"members evaluate themselves too; false if omitted on a new form, and as it was on a form there is. A pair is moderated only with it on"`
	Opens             string          `json:"opens" jsonschema:"on_hand_in: for each group once it has handed work in; at: at opens_at"`
	OpensAt           *time.Time      `json:"opens_at,omitempty" jsonschema:"with opens at; before closes_at"`
	ClosesAt          time.Time       `json:"closes_at" jsonschema:"when no more sheets are written, for every group; what counts in grades is worked out once it has passed"`
	Weight            int32           `json:"weight" jsonschema:"0 to 100: the percentage of each member's grade what they received moves; 0 for reference only. A change rewrites no grade until grade.apply_peer"`
	ShareWithStudents *string         `json:"share_with_students,omitempty" jsonschema:"none, or own_average: after it closes a member reads their own average from two peers or more; none if omitted on a new form, and as it was on a form there is"`
	Version           *int32          `json:"version,omitempty" jsonschema:"the version you read (peer_form.get), 0 for none: the change is made only over it (version_mismatch). Omitted, only a new form is made. Over REST the If-Match header may carry it. A proposal records it"`
}

// Check holds the form to what it says alone.
func (in PeerFormSetIn) check() error {
	switch in.Kind {
	case peercalc.Rating:
		if err := checkCriteria(in.Criteria); err != nil {
			return err
		}
		if in.ScaleMin == nil || in.ScaleMax == nil || (*in.ScaleMin != 0 && *in.ScaleMin != 1) || *in.ScaleMax <= *in.ScaleMin || *in.ScaleMax > maxScale {
			return apperr.Invalid("a rating form's scale is from scale_min, 0 or 1, to scale_max, above it and at most %d", maxScale)
		}
	case peercalc.Share:
		if len(in.Criteria) > 0 || in.ScaleMin != nil || in.ScaleMax != nil {
			return apperr.Invalid("a share form has no criteria and no scale")
		}
	default:
		return apperr.Invalid("kind is rating or share")
	}
	switch in.Opens {
	case peerOpensAt:
		if in.OpensAt == nil || !in.OpensAt.Before(in.ClosesAt) {
			return apperr.Invalid("a form that opens at a time says when, opens_at, before closes_at")
		}
	case peerOpensOnHandIn:
		if in.OpensAt != nil {
			return apperr.Invalid("opens_at goes with opens at")
		}
	default:
		return apperr.Invalid("opens is on_hand_in or at")
	}
	if in.Weight < 0 || in.Weight > 100 {
		return apperr.Invalid("weight is 0 to 100")
	}
	if s := in.ShareWithStudents; s != nil && *s != peerShareNone && *s != peerShareOwnAverage {
		return apperr.Invalid("share_with_students is none or own_average")
	}
	if in.Version != nil && *in.Version < 0 {
		return apperr.Invalid("version is the one you read, 0 for none")
	}
	return nil
}

func errPeerFormVersion(current int32) error {
	return apperr.Conflicts("the peer form has changed since you read it (version %d now); read it again", current).
		With("reason", "version_mismatch").With("current_version", current)
}

// settable refuses in's form for assignment a, whose form is now (nil for
// none): an individual assignment; a version other than the one read; a
// shape other than the one in use.
func (in PeerFormSetIn) settable(ctx context.Context, q dbq.Querier, a dbq.GetAssignmentInCourseRow, now *dbq.PeerForm) error {
	if a.GroupSetID == nil {
		return apperr.Precondition("a peer form is for a group assignment: make it one first (group_set_id)").
			With("reason", ReasonNotAGroupAssignment)
	}
	current := int32(0)
	if now != nil {
		current = now.Version
	}
	if (in.Version == nil && now != nil) || (in.Version != nil && *in.Version != current) {
		return errPeerFormVersion(current)
	}
	if now == nil {
		return nil
	}
	next, err := in.applied(now)
	if err != nil {
		return err
	}
	if next.Kind == now.Kind && sameJSON(next.Criteria, now.Criteria) && sameInt(next.ScaleMin, now.ScaleMin) &&
		sameInt(next.ScaleMax, now.ScaleMax) && next.SelfEvaluation == now.SelfEvaluation {
		return nil
	}
	inUse, err := q.PeerFormInUse(ctx, a.ID)
	if err != nil {
		return err
	}
	if inUse {
		return apperr.Precondition("sheets have been written on this form: its kind, criteria, scale and self-evaluation no longer change; its dates, weight and sharing do").
			With("reason", ReasonFormInUse)
	}
	return nil
}

// applied is the form once in is applied to now's (nil for none).
func (in PeerFormSetIn) applied(now *dbq.PeerForm) (dbq.PeerForm, error) {
	next := dbq.PeerForm{AssignmentID: in.AssignmentID, CourseID: in.CourseID, Enabled: true, Kind: in.Kind, ScaleMin: in.ScaleMin,
		ScaleMax: in.ScaleMax, Opens: in.Opens, OpensAt: in.OpensAt, ClosesAt: in.ClosesAt, Weight: in.Weight,
		ShareWithStudents: peerShareNone}
	if now != nil {
		next.Enabled, next.SelfEvaluation, next.ShareWithStudents = now.Enabled, now.SelfEvaluation, now.ShareWithStudents
	}
	if in.Enabled != nil {
		next.Enabled = *in.Enabled
	}
	if in.SelfEvaluation != nil {
		next.SelfEvaluation = *in.SelfEvaluation
	}
	if in.ShareWithStudents != nil {
		next.ShareWithStudents = *in.ShareWithStudents
	}
	var err error
	next.Criteria, err = keptCriteria(in.Criteria)
	return next, err
}

func sameInt(a, b *int32) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// sameJSON: the same JSON value, however it is spelt.
func sameJSON(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ja, _ := json.Marshal(x)
	jb, _ := json.Marshal(y)
	return string(ja) == string(jb)
}

func peerFormSet() tool.Tool {
	return tool.Define(tool.Spec[PeerFormSetIn, PeerFormView]{
		Name: "peer_form.set",
		Description: "Make or change a group assignment's peer form, over the version you read (version, or If-Match; omitted, " +
			"only a new form is made): how members of each group evaluate each other's contribution, rating each on criteria " +
			"(kind rating, criteria and a scale) or splitting 100 points among them (kind share), themselves too with " +
			"self_evaluation; when (opens on_hand_in, for each group once it has handed work in, or at opens_at; closes_at); " +
			"at what weight it moves each member's grade (0 for reference only), and whether a member reads their own " +
			"average after it closes (share_with_students). Once a sheet is written its kind, criteria, scale and " +
			"self-evaluation are fixed (form_in_use); its dates, weight and sharing still change, and enabled false stops it, " +
			"keeping what was written. A change rewrites no grade: grade.apply_peer does. Refused on an individual " +
			"assignment (not_a_group_assignment).",
		Kind: tool.Write, Gate: writeAssignments,
		HTTP:  tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/assignments/{assignment_id}/peer-form", IfMatch: "version"},
		Check: func(in PeerFormSetIn) error { return in.check() },
		Resolve: func(ctx context.Context, q dbq.Querier, in PeerFormSetIn) (tool.Target, error) {
			return assignmentTarget(ctx, q, in.CourseID, in.AssignmentID)
		},
		Validate: func(ctx context.Context, q dbq.Querier, _ *domain.Member, _ time.Time, in PeerFormSetIn) error {
			a, err := q.GetAssignmentInCourse(ctx, dbq.GetAssignmentInCourseParams{ID: in.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return goneIfNoRows(ctx, q, in.CourseID, in.AssignmentID, err)
			}
			now, err := loadPeerForm(ctx, q, in.CourseID, a.ID)
			if err != nil {
				return err
			}
			return in.settable(ctx, q, a, now)
		},
		// The form as it is when proposed, and what is left as it is said:
		// approving it makes, over that version, the form proposed.
		Pin: func(ctx context.Context, q dbq.Querier, _ *domain.Member, _ time.Time, in PeerFormSetIn) (PeerFormSetIn, error) {
			now, err := loadPeerForm(ctx, q, in.CourseID, in.AssignmentID)
			if err != nil {
				return in, err
			}
			if in.Version == nil {
				v := int32(0)
				if now != nil {
					v = now.Version
				}
				in.Version = &v
			}
			next, err := in.applied(now)
			in.Enabled, in.SelfEvaluation, in.ShareWithStudents = &next.Enabled, &next.SelfEvaluation, &next.ShareWithStudents
			return in, err
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in PeerFormSetIn) (PeerFormView, error) {
			// The assignment held still, as a change of its set holds it
			// FOR UPDATE, and then the form.
			sa, err := ec.Q.ShareAssignmentForGrading(ctx, dbq.ShareAssignmentForGradingParams{ID: in.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return PeerFormView{}, goneIfNoRows(ctx, ec.Q, in.CourseID, in.AssignmentID, err)
			}
			a := dbq.GetAssignmentInCourseRow(sa)
			var now *dbq.PeerForm
			locked, err := ec.Q.LockPeerForm(ctx, dbq.LockPeerFormParams{AssignmentID: a.ID, CourseID: in.CourseID})
			switch {
			case err == nil:
				now = &locked
			case !errors.Is(err, pgx.ErrNoRows):
				return PeerFormView{}, err
			}
			if err := in.settable(ctx, ec.Q, a, now); err != nil {
				return PeerFormView{}, err
			}
			next, err := in.applied(now)
			if err != nil {
				return PeerFormView{}, err
			}
			changed := changedFields(now, next)
			if now == nil {
				err = ec.Q.InsertPeerForm(ctx, dbq.InsertPeerFormParams{AssignmentID: a.ID, CourseID: in.CourseID, Enabled: next.Enabled,
					Kind: next.Kind, Criteria: next.Criteria, ScaleMin: next.ScaleMin, ScaleMax: next.ScaleMax,
					SelfEvaluation: next.SelfEvaluation, Opens: next.Opens, OpensAt: next.OpensAt, ClosesAt: next.ClosesAt,
					Weight: next.Weight, ShareWithStudents: next.ShareWithStudents, MemberID: ec.Member.ID, Now: ec.Now})
			} else if len(changed) > 0 {
				_, err = ec.Q.UpdatePeerForm(ctx, dbq.UpdatePeerFormParams{AssignmentID: a.ID, Enabled: next.Enabled, Kind: next.Kind,
					Criteria: next.Criteria, ScaleMin: next.ScaleMin, ScaleMax: next.ScaleMax, SelfEvaluation: next.SelfEvaluation,
					Opens: next.Opens, OpensAt: next.OpensAt, ClosesAt: next.ClosesAt, Weight: next.Weight,
					ShareWithStudents: next.ShareWithStudents, MemberID: ec.Member.ID, Now: ec.Now})
			}
			if err != nil {
				return PeerFormView{}, err
			}
			f, err := ec.Q.GetPeerForm(ctx, dbq.GetPeerFormParams{AssignmentID: a.ID, CourseID: in.CourseID})
			if err != nil {
				return PeerFormView{}, err
			}
			if now == nil || len(changed) > 0 {
				ec.Emit(events.Event{Type: EventPeerFormUpdated, CourseID: &in.CourseID, SubjectType: "assignment", SubjectID: &a.ID,
					AssignmentID: &a.ID, Payload: map[string]any{"created": now == nil, "changed": changed, "enabled": f.Enabled,
						"version": f.Version}})
			}
			inUse, err := ec.Q.PeerFormInUse(ctx, a.ID)
			if err != nil {
				return PeerFormView{}, err
			}
			return viewPeerForm(f, inUse)
		},
	})
}

// changedFields are the fields of the form next changes, of now's.
func changedFields(now *dbq.PeerForm, next dbq.PeerForm) []string {
	out := []string{}
	if now == nil {
		return out
	}
	for _, c := range []struct {
		name string
		same bool
	}{
		{"enabled", now.Enabled == next.Enabled},
		{"kind", now.Kind == next.Kind},
		{"criteria", sameJSON(now.Criteria, next.Criteria)},
		{"scale", sameInt(now.ScaleMin, next.ScaleMin) && sameInt(now.ScaleMax, next.ScaleMax)},
		{"self_evaluation", now.SelfEvaluation == next.SelfEvaluation},
		{"opens", now.Opens == next.Opens && sameTime(now.OpensAt, next.OpensAt)},
		{"closes_at", now.ClosesAt.Equal(next.ClosesAt)},
		{"weight", now.Weight == next.Weight},
		{"share_with_students", now.ShareWithStudents == next.ShareWithStudents},
	} {
		if !c.same {
			out = append(out, c.name)
		}
	}
	return out
}

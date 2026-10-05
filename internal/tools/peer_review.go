package tools

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/authz"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/events"
	"github.com/AIShie-Education/AIShie-Core/internal/ids"
	"github.com/AIShie-Education/AIShie-Core/internal/peercalc"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

// A rater's sheet (docs/schema.md §2.5b): written by a person, a member of a
// group's circle, while the form's window is open for the group, covering
// exactly whom they evaluate; written again while it is open, the new
// sheet superseding the old, which is kept.

// ---------------------------------------------------------------------------
// peer_review.submit
// ---------------------------------------------------------------------------

// PeerEntryIn is what a sheet gives one member.
type PeerEntryIn struct {
	StudentMemberID uuid.UUID      `json:"student_member_id" jsonschema:"a member of the circle the rater evaluates"`
	Ratings         map[string]int `json:"ratings,omitempty" jsonschema:"a rating form: a whole number on the scale for every criterion, by its key"`
	Share           *int           `json:"share,omitempty" jsonschema:"a share form: 0 to 100; the sheet's shares add up to exactly 100"`
	Comment         *string        `json:"comment,omitempty" jsonschema:"at most 1000 characters; read by those who grade, never by the member"`
}

type PeerReviewSubmitIn struct {
	tool.InCourse
	AssignmentID uuid.UUID     `json:"assignment_id"`
	Entries      []PeerEntryIn `json:"entries" jsonschema:"one for each member the rater evaluates (peer_form.get's task.to_evaluate), and nobody else"`
	Comment      *string       `json:"comment,omitempty" jsonschema:"on the group's work as a whole, at most 2000 characters; read by those who grade"`
}

type PeerReviewSubmitOut struct {
	ReviewID uuid.UUID  `json:"review_id"`
	Replaces *uuid.UUID `json:"replaces,omitempty" jsonschema:"the rater's sheet it replaced, kept as history"`
	GroupID  uuid.UUID  `json:"group_id"`
	ClosesAt time.Time  `json:"closes_at" jsonschema:"until when it may be written again"`
}

// check holds a sheet to what it says alone: entries, each one member's,
// each a rating or a share, and shares adding up to 100.
func (in PeerReviewSubmitIn) check() error {
	if len(in.Entries) == 0 || len(in.Entries) > maxGroupSize {
		return apperr.Invalid("a sheet has 1 to %d entries, one for each member evaluated", maxGroupSize).With("reason", ReasonSheetIncomplete)
	}
	if in.Comment != nil && utf8.RuneCountInString(*in.Comment) > maxSheetComment {
		return apperr.Invalid("a sheet's comment is at most %d characters", maxSheetComment)
	}
	seen := map[uuid.UUID]bool{}
	shares, total := 0, 0
	for _, e := range in.Entries {
		if seen[e.StudentMemberID] {
			return apperr.Invalid("member %s is evaluated twice", e.StudentMemberID).With("reason", ReasonSheetIncomplete)
		}
		seen[e.StudentMemberID] = true
		switch {
		case (e.Share == nil) == (len(e.Ratings) == 0):
			return apperr.Invalid("an entry gives ratings or a share, one of them").With("reason", ReasonBadRating)
		case e.Share != nil:
			if *e.Share < 0 || *e.Share > peercalc.ShareTotal {
				return apperr.Invalid("a share is 0 to %d", peercalc.ShareTotal).With("reason", ReasonBadShareTotal)
			}
			shares++
			total += *e.Share
		}
		if e.Comment != nil && utf8.RuneCountInString(*e.Comment) > maxEntryComment {
			return apperr.Invalid("an entry's comment is at most %d characters", maxEntryComment)
		}
	}
	if shares > 0 && shares != len(in.Entries) {
		return apperr.Invalid("a sheet gives ratings or shares, not both").With("reason", ReasonBadRating)
	}
	if shares > 0 && total != peercalc.ShareTotal {
		return apperr.Invalid("the shares add up to %d, not %d", total, peercalc.ShareTotal).With("reason", ReasonBadShareTotal).
			With("total", total)
	}
	return nil
}

// sheetFor is where rater's sheet for assignment a goes, refused as
// peer_review.submit is refused: an agent's (people_only); no form, or one
// not enabled; a rater in no circle; the window not open for their group; a
// sheet that does not cover exactly whom they evaluate, or rates what the
// form does not.
type sheetFor struct {
	form   dbq.PeerForm
	circle circle
}

func (in PeerReviewSubmitIn) sheetFor(ctx context.Context, q dbq.Querier, m *domain.Member, a dbq.GetAssignmentInCourseRow,
	f *dbq.PeerForm, now time.Time) (sheetFor, error) {
	var s sheetFor
	// A peer evaluation is a person's judgment of their classmates: no
	// agent writes one, its owner's or any other's, whatever it holds. This
	// reads kind to refuse, as a password reset does; nothing that grants
	// reads it.
	agent, err := callerIsAgent(ctx, q, m.ActorID)
	if err != nil {
		return s, err
	}
	if agent || m.PrincipalID != nil {
		return s, apperr.Forbid("a peer evaluation is a person's judgment of their classmates; an agent writes none").
			With("reason", PeerPeopleOnly)
	}
	if reason, err := authz.CheckScope(ctx, q, m, authz.Target{StudentMemberIDs: []uuid.UUID{m.ID}, AssignmentIDs: []uuid.UUID{a.ID}}); err != nil {
		return s, err
	} else if reason != authz.ReasonNone {
		return s, apperr.Forbid("you are outside your own scope").With("reason", string(reason))
	}
	if f == nil {
		return s, errNoPeerForm()
	}
	if !f.Enabled {
		return s, apperr.Precondition("the peer form is switched off").With("reason", ReasonPeerFormDisabled)
	}
	s.form = *f
	c, in2, err := circleFor(ctx, q, a, m.ID)
	if err != nil {
		return s, err
	}
	if c == nil || !in2 {
		return s, apperr.Precondition("you are in no group's circle for this assignment: nobody here for you to evaluate").
			With("reason", ReasonNotInCircle)
	}
	s.circle = *c
	switch windowOf(*f, c.handedIn, now) {
	case windowClosed:
		return s, apperr.Precondition("peer evaluation closed at %s", f.ClosesAt.Format(time.RFC3339)).
			With("reason", ReasonWindowClosed).With("closes_at", f.ClosesAt)
	case windowNotOpen:
		e := apperr.Precondition("peer evaluation is not open yet for your group").With("reason", ReasonWindowNotOpen).With("opens", f.Opens)
		if f.OpensAt != nil {
			e = e.With("opens_at", *f.OpensAt)
		}
		return s, e
	}
	return s, in.covers(*f, *c, m.ID)
}

// covers refuses a sheet that does not cover exactly whom rater evaluates in
// circle c on form f, or gives what the form does not take.
func (in PeerReviewSubmitIn) covers(f dbq.PeerForm, c circle, rater uuid.UUID) error {
	want := toEvaluate(f, c.members, rater)
	for _, e := range in.Entries {
		if e.StudentMemberID == rater && !f.SelfEvaluation {
			return apperr.Invalid("members do not evaluate themselves on this form").With("reason", ReasonSelfEvaluationOff)
		}
	}
	got := make([]uuid.UUID, len(in.Entries))
	for i, e := range in.Entries {
		got[i] = e.StudentMemberID
	}
	if !sameMembers(got, want) {
		return apperr.Invalid("a sheet covers exactly the members you evaluate, and nobody else").With("reason", ReasonSheetIncomplete).
			With("to_evaluate", want)
	}
	if f.Kind == peercalc.Share {
		for _, e := range in.Entries {
			if e.Share == nil {
				return apperr.Invalid("this form splits %d points among those evaluated: give each a share", peercalc.ShareTotal).
					With("reason", ReasonBadShareTotal)
			}
		}
		return nil
	}
	form, err := calcForm(f)
	if err != nil {
		return err
	}
	for _, e := range in.Entries {
		if e.Share != nil || len(e.Ratings) != len(form.Criteria) {
			return apperr.Invalid("rate every criterion of the form, and nothing else").With("reason", ReasonBadRating)
		}
		for _, c := range form.Criteria {
			r, ok := e.Ratings[c.Key]
			if !ok || r < int(*f.ScaleMin) || r > int(*f.ScaleMax) {
				return apperr.Invalid("a rating of %s is a whole number from %d to %d", c.Key, *f.ScaleMin, *f.ScaleMax).
					With("reason", ReasonBadRating).With("criterion", c.Key)
			}
		}
	}
	return nil
}

func peerReviewSubmit() tool.Tool {
	load := func(ctx context.Context, q dbq.Querier, in PeerReviewSubmitIn) (dbq.GetAssignmentInCourseRow, *dbq.PeerForm, error) {
		a, err := q.GetAssignmentInCourse(ctx, dbq.GetAssignmentInCourseParams{ID: in.AssignmentID, CourseID: in.CourseID})
		if err != nil {
			return a, nil, goneIfNoRows(ctx, q, in.CourseID, in.AssignmentID, err)
		}
		f, err := loadPeerForm(ctx, q, in.CourseID, a.ID)
		return a, f, err
	}
	return tool.Define(tool.Spec[PeerReviewSubmitIn, PeerReviewSubmitOut]{
		Name: "peer_review.submit",
		Description: "A student's peer evaluation of the members of their group: one entry for each member they evaluate " +
			"(peer_form.get's task.to_evaluate), each a rating of every criterion or a share of 100 points, the shares adding " +
			"up to 100, with comments if they like. Only by the student, a person: an agent writes none, its owner's or any " +
			"other's (people_only). Only while the window is open for their group (window_not_open, window_closed), and only " +
			"by a member of the group's circle (not_in_circle). Written again while it is open, the new sheet replaces the " +
			"old, which is kept. Those who grade read it, with who wrote it; no other student does.",
		Kind: tool.Write, Gate: writeSubmissions,
		HTTP:  tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/assignments/{assignment_id}/peer-reviews"},
		Check: func(in PeerReviewSubmitIn) error { return in.check() },
		// The rater is the caller, known only as the call runs: sheetFor
		// checks their own scope.
		Resolve: func(ctx context.Context, q dbq.Querier, in PeerReviewSubmitIn) (tool.Target, error) {
			return assignmentTarget(ctx, q, in.CourseID, in.AssignmentID)
		},
		Validate: func(ctx context.Context, q dbq.Querier, m *domain.Member, now time.Time, in PeerReviewSubmitIn) error {
			a, f, err := load(ctx, q, in)
			if err != nil {
				return err
			}
			_, err = in.sheetFor(ctx, q, m, a, f, now)
			return err
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in PeerReviewSubmitIn) (PeerReviewSubmitOut, error) {
			// The assignment held still, as a change of its set holds it
			// FOR UPDATE; the form, as a change of it holds it FOR UPDATE;
			// the rater's sheet; and the group, as a move of its members
			// holds it FOR UPDATE.
			sa, err := ec.Q.ShareAssignmentForGrading(ctx, dbq.ShareAssignmentForGradingParams{ID: in.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return PeerReviewSubmitOut{}, goneIfNoRows(ctx, ec.Q, in.CourseID, in.AssignmentID, err)
			}
			a := dbq.GetAssignmentInCourseRow(sa)
			f, err := sharePeerForm(ctx, ec.Q, in.CourseID, a.ID)
			if err != nil {
				return PeerReviewSubmitOut{}, err
			}
			if err := ec.Q.LockPeerSheet(ctx, dbq.LockPeerSheetParams{AssignmentID: a.ID, RaterMemberID: ec.Member.ID}); err != nil {
				return PeerReviewSubmitOut{}, err
			}
			s, err := in.sheetFor(ctx, ec.Q, ec.Member, a, f, ec.Now)
			if err != nil {
				return PeerReviewSubmitOut{}, err
			}
			if _, err := ec.Q.ShareGroup(ctx, dbq.ShareGroupParams{ID: s.circle.group, CourseID: in.CourseID}); err != nil {
				return PeerReviewSubmitOut{}, err
			}
			if s, err = in.sheetFor(ctx, ec.Q, ec.Member, a, f, ec.Now); err != nil {
				return PeerReviewSubmitOut{}, err
			}
			id := ids.New()
			out := PeerReviewSubmitOut{ReviewID: id, GroupID: s.circle.group, ClosesAt: s.form.ClosesAt}
			// The old sheet first, naming the new one (a deferred key), so
			// that there are never two current sheets.
			old, err := ec.Q.SupersedePeerSheet(ctx, dbq.SupersedePeerSheetParams{NewID: &id, AssignmentID: a.ID, RaterMemberID: ec.Member.ID})
			switch {
			case err == nil:
				out.Replaces = &old
			case !isNoRows(err):
				return PeerReviewSubmitOut{}, err
			}
			if err := ec.Q.InsertPeerReview(ctx, dbq.InsertPeerReviewParams{ID: id, CourseID: in.CourseID, AssignmentID: a.ID,
				GroupID: s.circle.group, RaterMemberID: ec.Member.ID, Comment: keptComment(in.Comment), CreatedByActionID: ec.ActionID,
				CreatedAt: ec.Now}); err != nil {
				return PeerReviewSubmitOut{}, err
			}
			for _, e := range in.Entries {
				row := dbq.InsertPeerReviewEntryParams{ReviewID: id, CourseID: in.CourseID, RateeMemberID: e.StudentMemberID,
					Comment: keptComment(e.Comment)}
				if e.Share != nil {
					share := int32(*e.Share) //nolint:gosec // 0 to 100, checked
					row.Share = &share
				} else if row.Ratings, err = json.Marshal(e.Ratings); err != nil {
					return PeerReviewSubmitOut{}, err
				}
				if err := ec.Q.InsertPeerReviewEntry(ctx, row); err != nil {
					return PeerReviewSubmitOut{}, err
				}
			}
			// Filed under the rater: nobody else of the group learns of it.
			rater := ec.Member.ID
			payload := map[string]any{"group_id": s.circle.group}
			if out.Replaces != nil {
				payload["replaces"] = *out.Replaces
			}
			ec.Emit(events.Event{Type: EventPeerReviewSubmitted, CourseID: &in.CourseID, SubjectType: "peer_review", SubjectID: &id,
				StudentMemberID: &rater, AssignmentID: &a.ID, Payload: payload})
			return out, nil
		},
	})
}

// keptComment is a comment as it is kept: none for one that says nothing.
func keptComment(c *string) *string {
	if c == nil || strings.TrimSpace(*c) == "" {
		return nil
	}
	return c
}

// ---------------------------------------------------------------------------
// peer_review.results
// ---------------------------------------------------------------------------

type PeerResultsIn struct {
	tool.InCourse
	AssignmentID uuid.UUID  `json:"assignment_id"`
	GroupID      *uuid.UUID `json:"group_id,omitempty" jsonschema:"one group's alone"`
}

// ShareReceived is a share one rater gave a member.
type ShareReceived struct {
	RaterMemberID uuid.UUID `json:"rater_member_id"`
	Share         int32     `json:"share"`
}

// PeerMemberGrade is a member's live grade on the circle's work, beside what
// counting peer evaluation would give.
type PeerMemberGrade struct {
	GradeID        uuid.UUID       `json:"grade_id"`
	Score          decimal.Decimal `json:"score"`
	State          string          `json:"state" jsonschema:"draft or posted"`
	AdjustmentKind *string         `json:"adjustment_kind,omitempty" jsonschema:"replace or delta: a grader's own, which peer evaluation does not change; peer: peer evaluation's"`
}

// PeerMemberResult is what one member of a circle did and received.
type PeerMemberResult struct {
	MemberID    uuid.UUID                  `json:"member_id"`
	DisplayName string                     `json:"display_name"`
	Submitted   bool                       `json:"submitted" jsonschema:"whether they have a current sheet"`
	SubmittedAt *time.Time                 `json:"submitted_at,omitempty"`
	RatedBy     []uuid.UUID                `json:"rated_by" jsonschema:"the raters whose sheets rate them, themselves among them with self-evaluation"`
	Averages    map[string]decimal.Decimal `json:"averages,omitempty" jsonschema:"a rating form: the average from their peers on each criterion"`
	Shares      []ShareReceived            `json:"shares,omitempty" jsonschema:"a share form: what each peer gave them"`
	Self        *PeerEntryView             `json:"self,omitempty" jsonschema:"what they gave themselves, with self-evaluation on"`
	Factor      decimal.Decimal            `json:"factor" jsonschema:"what they received against an even share from the same raters: 1 is even, and 1 when nobody rated them"`
	PeerFactor  *decimal.Decimal           `json:"peer_factor,omitempty" jsonschema:"the factor from their peers alone"`
	SelfFactor  *decimal.Decimal           `json:"self_factor,omitempty" jsonschema:"what they gave themselves against an even share"`
	Score       *decimal.Decimal           `json:"score,omitempty" jsonschema:"what the factor gives at the form's weight from the group's score, held to zero and the points possible; absent before the group is graded"`
	Grade       *PeerMemberGrade           `json:"grade,omitempty" jsonschema:"their live grade on the work now"`
	Flags       []string                   `json:"flags" jsonschema:"low (factor below 0.8), high (above 1.2), self_above_peers (gave themselves 0.3 or more above what their peers gave them); as a rater, uniform (gave everyone the same) and missing (no sheet)"`
}

// PeerGroupResult is one group's results.
type PeerGroupResult struct {
	GroupID        uuid.UUID          `json:"group_id"`
	Name           string             `json:"name"`
	SubmissionID   *uuid.UUID         `json:"submission_id,omitempty" jsonschema:"the work whose members are the circle; absent while it has none handed in or recorded missing"`
	Window         PeerWindow         `json:"window"`
	GroupScore     *decimal.Decimal   `json:"group_score,omitempty" jsonschema:"the group's score as it stands, from its live group grade"`
	PointsPossible decimal.Decimal    `json:"points_possible"`
	Flags          []string           `json:"flags" jsonschema:"pair_without_self_evaluation: a pair with self-evaluation off, whose factors are always 1"`
	Members        []PeerMemberResult `json:"members"`
	Sheets         []PeerSheetView    `json:"sheets" jsonschema:"every current sheet of the circle, with who wrote it, its entries and comments"`
}

type PeerResultsOut struct {
	Form   PeerFormView      `json:"form"`
	Groups []PeerGroupResult `json:"groups" jsonschema:"each group whose circle lies wholly within your student scope"`
}

// peerResultsTarget is what reading the results reaches: the assignment,
// and, for one group, every member of its circle.
func peerResultsTarget(ctx context.Context, q dbq.Querier, in PeerResultsIn) (tool.Target, error) {
	t, err := assignmentTarget(ctx, q, in.CourseID, in.AssignmentID)
	if err != nil {
		return t, err
	}
	// Either permission governs: holding one is enough to read evidence for
	// grading.
	t.Perms = nil
	if in.GroupID == nil {
		return t, nil
	}
	if _, err := loadGroup(ctx, q, in.CourseID, *in.GroupID); err != nil {
		return t, err
	}
	c, err := circleOf(ctx, q, in.AssignmentID, *in.GroupID)
	if err != nil {
		return t, err
	}
	t.Scope.StudentMemberIDs = c.members
	return t, nil
}

func peerReviewResults() tool.Tool {
	return tool.Define(tool.Spec[PeerResultsIn, PeerResultsOut]{
		Name: "peer_review.results",
		Description: "Peer evaluation's results for an assignment, for those who grade, for each group whose circle lies " +
			"wholly within your student scope (or one group's): for each member whether they wrote a sheet and when, who " +
			"rated them, what their peers gave them (the average on each criterion, or each share), what they gave " +
			"themselves, their factor against an even share and the score it would give at the form's weight, their live " +
			"grade, and flags (low, high, self_above_peers; as a rater, uniform and missing); and every sheet with who wrote " +
			"it, its entries and comments. Evidence for grading: never tell a student what a peer said of them.",
		Kind: tool.Read, Gate: tool.Gate{Any: true, Perms: []domain.Perm{domain.PermGradeSubmit, domain.PermGradePost}},
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/assignments/{assignment_id}/peer-results"},
		Resolve: func(ctx context.Context, q dbq.Querier, in PeerResultsIn) (tool.Target, error) {
			return peerResultsTarget(ctx, q, in)
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in PeerResultsIn) (PeerResultsOut, error) {
			a, err := rc.Q.GetAssignmentInCourse(ctx, dbq.GetAssignmentInCourseParams{ID: in.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return PeerResultsOut{}, goneIfNoRows(ctx, rc.Q, in.CourseID, in.AssignmentID, err)
			}
			f, err := loadPeerForm(ctx, rc.Q, in.CourseID, a.ID)
			if err != nil {
				return PeerResultsOut{}, err
			}
			if f == nil {
				return PeerResultsOut{}, errNoPeerForm()
			}
			inUse, err := rc.Q.PeerFormInUse(ctx, a.ID)
			if err != nil {
				return PeerResultsOut{}, err
			}
			out := PeerResultsOut{Groups: []PeerGroupResult{}}
			if out.Form, err = viewPeerForm(*f, inUse); err != nil {
				return out, err
			}
			var groups []uuid.UUID
			if in.GroupID != nil {
				groups = []uuid.UUID{*in.GroupID}
			} else if a.GroupSetID != nil {
				rows, err := rc.Q.ListGroupsOfSets(ctx, []uuid.UUID{*a.GroupSetID})
				if err != nil {
					return out, err
				}
				for _, g := range rows {
					if g.ArchivedAt == nil {
						groups = append(groups, g.ID)
					}
				}
			}
			for _, g := range groups {
				r, ok, err := groupResult(ctx, rc, a, *f, g)
				if err != nil {
					return out, err
				}
				if ok {
					out.Groups = append(out.Groups, r)
				}
			}
			return out, nil
		},
	})
}

// groupResult is group's results on form f, if its circle has anyone and lies
// wholly within the reader's student scope.
func groupResult(ctx context.Context, rc *tool.ReadCtx, a dbq.GetAssignmentInCourseRow, f dbq.PeerForm, group uuid.UUID) (PeerGroupResult, bool, error) {
	q := rc.Q
	c, err := circleOf(ctx, q, a.ID, group)
	if err != nil || len(c.members) == 0 {
		return PeerGroupResult{}, false, err
	}
	reach, err := reached(ctx, q, rc.Scope, c.members)
	if err != nil {
		return PeerGroupResult{}, false, err
	}
	for _, m := range c.members {
		if !reach[m] {
			return PeerGroupResult{}, false, nil
		}
	}
	g, err := loadGroup(ctx, q, a.CourseID, group)
	if err != nil {
		return PeerGroupResult{}, false, err
	}
	form, err := calcForm(f)
	if err != nil {
		return PeerGroupResult{}, false, err
	}
	sheets, err := currentSheets(ctx, q, a.ID, c.members)
	if err != nil {
		return PeerGroupResult{}, false, err
	}
	calc := calcSheets(sheets)
	res := peercalc.Compute(form, c.members, calc)
	received := peercalc.ReceivedBy(form, c.members, calc)
	r := PeerGroupResult{GroupID: g.ID, Name: g.Name, Window: viewWindow(f, c.handedIn, rc.Now), PointsPossible: a.PointsPossible,
		Flags: append([]string{}, res.Flags...), Members: []PeerMemberResult{}, Sheets: []PeerSheetView{}}
	// Sheets about the circle only: what a rater gave someone since gone
	// from it does not count, and is not shown as if it did.
	for _, s := range sheets {
		kept := s
		kept.Entries = nil
		for _, e := range s.Entries {
			if c.has(e.StudentMemberID) {
				kept.Entries = append(kept.Entries, e)
			}
		}
		if len(kept.Entries) > 0 {
			r.Sheets = append(r.Sheets, kept)
		}
	}
	grades := map[uuid.UUID]dbq.ListLiveMemberGradesRow{}
	var gg *dbq.GroupGrade
	if c.work != nil {
		r.SubmissionID = &c.work.ID
		live, err := q.LiveGroupGradeOfWork(ctx, c.work.ID)
		switch {
		case err == nil:
			gg = &live
			r.GroupScore = &live.Score
		case !isNoRows(err):
			return r, false, err
		}
		rows, err := q.ListLiveMemberGrades(ctx, &c.work.ID)
		if err != nil {
			return r, false, err
		}
		for _, row := range rows {
			if _, seen := grades[row.StudentMemberID]; !seen {
				grades[row.StudentMemberID] = row
			}
		}
	}
	names, err := q.NamesOfMembers(ctx, c.members)
	if err != nil {
		return r, false, err
	}
	for _, n := range names {
		m := n.ID
		mm := res.Members[m]
		mr := PeerMemberResult{MemberID: m, DisplayName: n.DisplayName, RatedBy: []uuid.UUID{}, Factor: mm.Factor,
			PeerFactor: mm.PeerFactor, SelfFactor: mm.SelfFactor, Averages: received[m].Averages,
			Flags: peercalc.SortedFlags(append(slices.Clone(mm.Flags), res.Raters[m].Flags...))}
		for _, s := range sheets {
			if s.RaterMemberID == m {
				at := s.SubmittedAt
				mr.Submitted, mr.SubmittedAt = true, &at
			}
			for _, e := range s.Entries {
				if e.StudentMemberID != m || !c.has(s.RaterMemberID) || (s.RaterMemberID == m && !f.SelfEvaluation) {
					continue
				}
				mr.RatedBy = append(mr.RatedBy, s.RaterMemberID)
				switch {
				case s.RaterMemberID == m:
					self := e
					mr.Self = &self
				case e.Share != nil:
					mr.Shares = append(mr.Shares, ShareReceived{RaterMemberID: s.RaterMemberID, Share: *e.Share})
				}
			}
		}
		if gg != nil {
			s := peercalc.Score(gg.Score, a.PointsPossible, f.Weight, mm.Factor, gg.AllowExtra)
			mr.Score = &s
		}
		if row, ok := grades[m]; ok {
			state := "draft"
			if row.PostedAt != nil {
				state = "posted"
			}
			mr.Grade = &PeerMemberGrade{GradeID: row.ID, Score: row.Score, State: state, AdjustmentKind: row.AdjustKind}
		}
		r.Members = append(r.Members, mr)
	}
	return r, true, nil
}

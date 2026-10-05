package tools_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// Peer evaluation within a group (docs/schema.md §2.5b): a group
// assignment's peer form; its members' sheets, written while the window is
// open for their group, anonymous to their peers and read in full by those
// who grade; and what each received, counted at the form's weight, moving
// their grade from the group's, a grader's own adjustment winning.

// peerGroups is a group assignment of two groups: the Team, Yuki, Ken, Aoi
// and Ren (the design's A, B, C and D), and the Duo, Hana and Mio. Its peer
// form, once set, is a share form, opening on hand-in, self-evaluation off,
// at a weight of 20 %, each member shown their own average once it closes.
type peerGroups struct {
	*built
	aoi, ren, hana, mio     uuid.UUID
	aoiM, renM, hanaM, mioM uuid.UUID
	set, team, duo          uuid.UUID
	hw                      uuid.UUID
	version                 int32
}

func buildPeerGroups(t *testing.T) *peerGroups {
	t.Helper()
	b := build(t)
	actors, s := b.seatStudents(t, "Aoi", "Ren", "Hana", "Mio")
	w := &peerGroups{built: b, aoi: actors["Aoi"], ren: actors["Ren"], hana: actors["Hana"], mio: actors["Mio"],
		aoiM: s["Aoi"], renM: s["Ren"], hanaM: s["Hana"], mioM: s["Mio"]}
	w.set = b.groupSet(t, "Projects", nil)
	g := b.groupsIn(t, w.set, m{"name": "Team"}, m{"name": "Duo"})
	w.team, w.duo = g[0], g[1]
	b.place(t, w.set, false, b.yukiM, w.team, b.kenM, w.team, w.aoiM, w.team, w.renM, w.team, w.hanaM, w.duo, w.mioM, w.duo)
	w.hw = b.groupAssignment(t, "Project", w.set)
	return w
}

// setForm sets the project's peer form as Sato, over the version he read.
func (w *peerGroups) setForm(t *testing.T, args m) tools.PeerFormView {
	t.Helper()
	args["course_id"], args["assignment_id"] = w.course, w.hw
	if w.version > 0 {
		args["version"] = w.version
	}
	f := testkit.Result[tools.PeerFormView](t, w.do(t, w.sato, "peer_form.set", args))
	w.version = f.Version
	return f
}

// shareForm is the project's share form, closing at closes.
func (w *peerGroups) shareForm(t *testing.T, closes time.Time) tools.PeerFormView {
	t.Helper()
	return w.setForm(t, m{"kind": "share", "opens": "on_hand_in", "closes_at": closes, "weight": 20, "share_with_students": "own_average"})
}

// closeForm moves its window's close to a minute ago.
func (w *peerGroups) closeForm(t *testing.T) {
	t.Helper()
	w.setForm(t, m{"kind": "share", "opens": "on_hand_in", "closes_at": time.Now().Add(-time.Minute), "weight": 20})
}

// handIn starts and hands in a group's work, as actor.
func (w *peerGroups) handIn(t *testing.T, actor uuid.UUID, body string) uuid.UUID {
	t.Helper()
	id := testkit.Result[tools.SubmissionCreateOut](t, w.do(t, actor, "submission.create",
		m{"course_id": w.course, "assignment_id": w.hw, "body": body})).SubmissionID
	w.do(t, actor, "submission.submit", m{"course_id": w.course, "submission_id": id})
	return id
}

// sheetArgs are a sheet of shares, member and share in turn.
func (w *peerGroups) sheetArgs(pairs ...any) m {
	entries := []m{}
	for i := 0; i < len(pairs); i += 2 {
		entries = append(entries, m{"student_member_id": pairs[i], "share": pairs[i+1]})
	}
	return m{"course_id": w.course, "assignment_id": w.hw, "entries": entries}
}

func (w *peerGroups) sheet(t *testing.T, actor uuid.UUID, pairs ...any) tools.PeerReviewSubmitOut {
	t.Helper()
	return testkit.Result[tools.PeerReviewSubmitOut](t, w.do(t, actor, "peer_review.submit", w.sheetArgs(pairs...)))
}

// designSheets are the design's example: A gives B 40, C 40, D 20; B gives
// A 40, C 40, D 20; C gives A 40, B 40, D 20; D gives A 40, B 30, C 30.
func (w *peerGroups) designSheets(t *testing.T) {
	t.Helper()
	w.sheet(t, w.yuki, w.kenM, 40, w.aoiM, 40, w.renM, 20)
	w.sheet(t, w.ken, w.yukiM, 40, w.aoiM, 40, w.renM, 20)
	w.sheet(t, w.aoi, w.yukiM, 40, w.kenM, 40, w.renM, 20)
	w.sheet(t, w.ren, w.yukiM, 40, w.kenM, 30, w.aoiM, 30)
}

func (w *peerGroups) peerForm(t *testing.T, actor uuid.UUID) tools.PeerFormGetOut {
	t.Helper()
	return testkit.Result[tools.PeerFormGetOut](t, w.do(t, actor, "peer_form.get", m{"course_id": w.course, "assignment_id": w.hw}))
}

func (w *peerGroups) results(t *testing.T, actor uuid.UUID, args m) tools.PeerResultsOut {
	t.Helper()
	if args == nil {
		args = m{}
	}
	args["course_id"], args["assignment_id"] = w.course, w.hw
	return testkit.Result[tools.PeerResultsOut](t, w.do(t, actor, "peer_review.results", args))
}

func groupNamed(t *testing.T, r tools.PeerResultsOut, group uuid.UUID) tools.PeerGroupResult {
	t.Helper()
	for _, g := range r.Groups {
		if g.GroupID == group {
			return g
		}
	}
	t.Fatalf("no results for group %s: %+v", group, r.Groups)
	return tools.PeerGroupResult{}
}

func memberResult(t *testing.T, g tools.PeerGroupResult, who uuid.UUID) tools.PeerMemberResult {
	t.Helper()
	for _, mr := range g.Members {
		if mr.MemberID == who {
			return mr
		}
	}
	t.Fatalf("no result for %s: %+v", who, g.Members)
	return tools.PeerMemberResult{}
}

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// A peer form is a group assignment's, set over the version read; its shape
// is fixed once a sheet names it; switched off it takes no sheet; and an
// assignment is made individual work only once its form is off.
func TestAPeerFormIsSetOnAGroupAssignment(t *testing.T) {
	w := buildPeerGroups(t)
	b := w.built
	soon := time.Now().Add(time.Hour)
	share := m{"course_id": b.course, "assignment_id": w.hw, "kind": "share", "opens": "on_hand_in", "closes_at": soon, "weight": 20}

	b.refusedAs(t, b.sato, "peer_form.set", m{"course_id": b.course, "assignment_id": b.hw3, "kind": "share", "opens": "on_hand_in",
		"closes_at": soon, "weight": 20}, apperr.FailedPrecondition, tools.ReasonNotAGroupAssignment)
	b.refusedAs(t, b.yuki, "peer_form.set", share, apperr.Forbidden, "")
	if got := w.peerForm(t, b.yuki); got.Form != nil || got.Task != nil {
		t.Fatalf("a form before there is one: %+v", got)
	}

	// Made: version 1, saying who sees what.
	f := w.setForm(t, m{"kind": "share", "opens": "on_hand_in", "closes_at": soon, "weight": 20})
	if f.Version != 1 || !f.Enabled || f.InUse || f.ShareWithStudents != "none" || f.SelfEvaluation ||
		!slices.Equal(f.VisibleTo, []string{"graders", "action_record"}) || !slices.Equal(f.StudentsSee, []string{"own_sheet", "own_adjustment"}) {
		t.Fatalf("the form made: %+v", f)
	}
	// Made again, as if there were none; and over a version that is not it.
	b.refusedAs(t, b.sato, "peer_form.set", share, apperr.Conflict, "version_mismatch")
	share["version"] = 7
	b.refusedAs(t, b.sato, "peer_form.set", share, apperr.Conflict, "version_mismatch")

	// Changed to a rating form while no sheet names it, opening at a time
	// past; criteria weigh 1 unless said.
	f = w.setForm(t, m{"kind": "rating", "criteria": []m{{"key": "effort", "label": " Effort "}, {"key": "quality",
		"label": "Quality of work", "description": "What it adds to the report", "weight": 2}}, "scale_min": 1, "scale_max": 5,
		"opens": "at", "opens_at": time.Now().Add(-time.Minute), "closes_at": soon, "weight": 0, "share_with_students": "own_average"})
	if f.Version != 2 || f.Kind != "rating" || len(f.Criteria) != 2 || f.Criteria[0].Label != "Effort" || f.Criteria[0].Weight == nil ||
		!f.Criteria[0].Weight.Equal(dec("1")) || !f.Criteria[1].Weight.Equal(dec("2")) || *f.ScaleMax != 5 ||
		!slices.Equal(f.StudentsSee, []string{"own_sheet", "own_average"}) {
		t.Fatalf("the rating form: %+v", f)
	}
	// Said again, nothing changes, and nothing is told.
	if again := w.setForm(t, m{"kind": "rating", "criteria": []m{{"key": "effort", "label": "Effort"}, {"key": "quality",
		"label": "Quality of work", "description": "What it adds to the report", "weight": 2}}, "scale_min": 1, "scale_max": 5,
		"opens": "at", "opens_at": f.OpensAt, "closes_at": soon, "weight": 0}); again.Version != 2 {
		t.Fatalf("the same form again: %+v", again)
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'peer_form.updated' AND assignment_id = $1`, w.hw); n != 2 {
		t.Fatalf("%d peer_form.updated events, want 2", n)
	}

	// Yuki's task: the Team, whom she evaluates, open; a sheet of ratings.
	task := w.peerForm(t, b.yuki).Task
	if task == nil || task.GroupID != w.team || !task.InCircle || len(task.Circle) != 4 || len(task.ToEvaluate) != 3 ||
		slices.Contains(task.ToEvaluate, b.yukiM) || task.Window.State != "open" || task.Sheet != nil {
		t.Fatalf("Yuki's task: %+v", task)
	}
	rate := func(e, q int) m { return m{"effort": e, "quality": q} }
	ratings := func(pairs ...any) m {
		entries := []m{}
		for i := 0; i < len(pairs); i += 2 {
			entries = append(entries, m{"student_member_id": pairs[i], "ratings": pairs[i+1]})
		}
		return m{"course_id": b.course, "assignment_id": w.hw, "entries": entries}
	}
	b.refusedAs(t, b.yuki, "peer_review.submit", ratings(b.kenM, rate(5, 9), w.aoiM, rate(3, 3), w.renM, rate(4, 4)),
		apperr.InvalidArgument, tools.ReasonBadRating)
	b.refusedAs(t, b.yuki, "peer_review.submit", ratings(b.kenM, m{"effort": 5}, w.aoiM, rate(3, 3), w.renM, rate(4, 4)),
		apperr.InvalidArgument, tools.ReasonBadRating)
	b.refusedAs(t, b.yuki, "peer_review.submit", ratings(b.kenM, m{"effort": 5, "quality": 4, "humour": 5}, w.aoiM, rate(3, 3),
		w.renM, rate(4, 4)), apperr.InvalidArgument, tools.ReasonBadRating)
	b.refusedAs(t, b.yuki, "peer_review.submit", w.sheetArgs(b.kenM, 40, w.aoiM, 40, w.renM, 20), apperr.InvalidArgument, tools.ReasonBadRating)
	b.do(t, b.yuki, "peer_review.submit", ratings(b.kenM, rate(5, 4), w.aoiM, rate(3, 3), w.renM, rate(4, 4)))
	if mine := w.peerForm(t, b.yuki).Task.Sheet; mine == nil || len(mine.Entries) != 3 || mine.Entries[0].Ratings["effort"] == 0 {
		t.Fatalf("Yuki's sheet as she reads it: %+v", mine)
	}

	// In use: the shape is fixed, the dates, weight and sharing are not.
	b.refusedAs(t, b.sato, "peer_form.set", m{"course_id": b.course, "assignment_id": w.hw, "kind": "share", "opens": "on_hand_in",
		"closes_at": soon, "weight": 20, "version": w.version}, apperr.FailedPrecondition, tools.ReasonFormInUse)
	f = w.setForm(t, m{"kind": "rating", "criteria": []m{{"key": "effort", "label": "Effort"}, {"key": "quality",
		"label": "Quality of work", "description": "What it adds to the report", "weight": 2}}, "scale_min": 1, "scale_max": 5,
		"opens": "at", "opens_at": f.OpensAt, "closes_at": soon.Add(time.Hour), "weight": 30})
	if f.Version != 3 || !f.InUse || f.Weight != 30 {
		t.Fatalf("the form's dates and weight changed: %+v", f)
	}
	var shape string
	if err := b.Pool.QueryRow(t.Context(), `SELECT kind FROM peer_form WHERE assignment_id = $1`, w.hw).Scan(&shape); err != nil || shape != "rating" {
		t.Fatalf("its kind: %q %v", shape, err)
	}
	// Switched off, it takes no sheet; what was written stays.
	f = w.setForm(t, m{"enabled": false, "kind": "rating", "criteria": []m{{"key": "effort", "label": "Effort"}, {"key": "quality",
		"label": "Quality of work", "description": "What it adds to the report", "weight": 2}}, "scale_min": 1, "scale_max": 5,
		"opens": "at", "opens_at": f.OpensAt, "closes_at": soon.Add(time.Hour), "weight": 30})
	if f.Enabled || slices.Contains(f.StudentsSee, "own_adjustment") {
		t.Fatalf("switched off: %+v", f)
	}
	b.refusedAs(t, b.ken, "peer_review.submit", ratings(b.yukiM, rate(5, 4), w.aoiM, rate(3, 3), w.renM, rate(4, 4)),
		apperr.FailedPrecondition, tools.ReasonPeerFormDisabled)
	if n := b.Count(`SELECT count(*) FROM peer_review WHERE assignment_id = $1`, w.hw); n != 1 {
		t.Fatalf("%d sheets kept, want Yuki's", n)
	}

	// A lab nobody has started on is made individual work only once its
	// form is off; and then takes no form.
	lab := b.groupAssignment(t, "Lab", w.set)
	labForm := m{"course_id": b.course, "assignment_id": lab, "kind": "share", "opens": "on_hand_in", "closes_at": soon, "weight": 0}
	b.do(t, b.sato, "peer_form.set", labForm)
	b.refusedAs(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": lab, "clear_group_set": true},
		apperr.FailedPrecondition, tools.ReasonPeerFormExists)
	labForm["enabled"], labForm["version"] = false, 1
	b.do(t, b.sato, "peer_form.set", labForm)
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": lab, "clear_group_set": true})
	labForm["version"] = 2
	b.refusedAs(t, b.sato, "peer_form.set", labForm, apperr.FailedPrecondition, tools.ReasonNotAGroupAssignment)
}

// The members of a group's circle evaluate each other once the window opens
// for their group, each sheet covering exactly whom its writer evaluates;
// written again, a sheet replaces the one before, which is kept. A student
// reads their own sheet, never another's; those who grade read every sheet
// of the groups their scope reaches wholly, with who wrote it.
func TestAGroupsMembersEvaluateEachOther(t *testing.T) {
	w := buildPeerGroups(t)
	b := w.built
	w.shareForm(t, time.Now().Add(time.Hour))

	// Before the Team hands in, its window is not open.
	task := w.peerForm(t, b.yuki).Task
	if task == nil || task.Window.State != "not_open" || task.Window.Opens != "on_hand_in" || len(task.Circle) != 4 {
		t.Fatalf("Yuki's task before the hand-in: %+v", task)
	}
	b.refusedAs(t, b.yuki, "peer_review.submit", w.sheetArgs(b.kenM, 40, w.aoiM, 40, w.renM, 20), apperr.FailedPrecondition, tools.ReasonWindowNotOpen)

	// Ken hands the Team's work in: open for the Team, and not the Duo.
	w.handIn(t, b.ken, "The Team's report")
	if task := w.peerForm(t, b.yuki).Task; task.Window.State != "open" {
		t.Fatalf("Yuki's task after the hand-in: %+v", task)
	}
	if task := w.peerForm(t, w.hana).Task; task == nil || task.GroupID != w.duo || task.Window.State != "not_open" || len(task.ToEvaluate) != 1 ||
		len(task.Circle) != 2 || task.Circle[0].MemberID == b.yukiM || task.Circle[1].MemberID == b.yukiM {
		t.Fatalf("Hana's task, the Duo's alone: %+v", task)
	}

	// Refused: a sheet that leaves someone out, one that evaluates its writer
	// on a form without self-evaluation, shares not adding up to 100, and
	// one by somebody in no circle.
	b.refusedAs(t, b.yuki, "peer_review.submit", w.sheetArgs(b.kenM, 50, w.aoiM, 50), apperr.InvalidArgument, tools.ReasonSheetIncomplete)
	b.refusedAs(t, b.yuki, "peer_review.submit", w.sheetArgs(b.yukiM, 25, b.kenM, 25, w.aoiM, 25, w.renM, 25),
		apperr.InvalidArgument, tools.ReasonSelfEvaluationOff)
	b.refusedAs(t, b.yuki, "peer_review.submit", w.sheetArgs(b.kenM, 40, w.aoiM, 40, w.renM, 10), apperr.InvalidArgument, tools.ReasonBadShareTotal)
	b.refusedAs(t, b.sato, "peer_review.submit", w.sheetArgs(b.kenM, 40, w.aoiM, 40, w.renM, 20), apperr.FailedPrecondition, tools.ReasonNotInCircle)
	w.handIn(t, w.hana, "The Duo's report")
	b.refusedAs(t, w.hana, "peer_review.submit", w.sheetArgs(b.kenM, 40, w.aoiM, 40, w.renM, 20), apperr.InvalidArgument, tools.ReasonSheetIncomplete)

	// The design's sheets, Yuki writing hers twice.
	first := w.sheet(t, b.yuki, b.kenM, 34, w.aoiM, 33, w.renM, 33)
	w.designSheets(t)
	if n := b.Count(`SELECT count(*) FROM peer_review WHERE assignment_id = $1`, w.hw); n != 5 {
		t.Fatalf("%d sheets, want five, Yuki's first among them", n)
	}
	var replacedBy uuid.UUID
	if err := b.Pool.QueryRow(t.Context(), `SELECT superseded_by FROM peer_review WHERE id = $1`, first.ReviewID).Scan(&replacedBy); err != nil {
		t.Fatalf("Yuki's first sheet: %v", err)
	}
	// A sheet's entries are written with it, and never after.
	if _, err := b.Pool.Exec(t.Context(), `INSERT INTO peer_review_entry (review_id, course_id, ratee_member_id, share) VALUES ($1, $2, $3, 0)`,
		replacedBy, b.course, b.yukiM); err == nil {
		t.Fatal("an entry was added to a sheet after it was written")
	}

	// Each reads their own sheet, the current one, and nobody's name on it.
	mine := w.peerForm(t, b.yuki).Task
	if mine.Sheet == nil || mine.Sheet.ReviewID != replacedBy || mine.Sheet.RaterName != nil || len(mine.Sheet.Entries) != 3 || mine.OwnAverage != nil {
		t.Fatalf("Yuki's task with her sheet: %+v %+v", mine, mine.Sheet)
	}
	if kens := w.peerForm(t, b.ken).Task.Sheet; kens == nil || kens.RaterMemberID != b.kenM {
		t.Fatalf("Ken's sheet as he reads it: %+v", kens)
	}
	// Nor does the feed tell a student of another's sheet.
	for _, who := range []struct{ actor, member uuid.UUID }{{b.ken, b.kenM}, {w.hana, w.hanaM}} {
		own := 0
		for _, e := range feed(t, b, who.actor) {
			if e.Type != tools.EventPeerReviewSubmitted {
				continue
			}
			if e.StudentMemberID == nil || *e.StudentMemberID != who.member {
				t.Fatalf("%s is told of another's sheet: %+v", who.member, e)
			}
			var payload map[string]any
			if err := json.Unmarshal(e.Payload, &payload); err != nil || payload["group_id"] == nil || payload["entries"] != nil {
				t.Fatalf("the event says more than ids: %s", e.Payload)
			}
			own++
		}
		if who.member == b.kenM && own != 1 {
			t.Fatalf("Ken is told of his own sheet %d times", own)
		}
	}

	// The results are for those who grade: not a student.
	b.refusedAs(t, b.yuki, "peer_review.results", m{"course_id": b.course, "assignment_id": w.hw}, apperr.Forbidden, "")
	r := w.results(t, b.sato, nil)
	team := groupNamed(t, r, w.team)
	for _, c := range []struct {
		who    uuid.UUID
		factor string
		flags  []string
	}{{b.yukiM, "1.2", []string{}}, {b.kenM, "1.1", []string{}}, {w.aoiM, "1.1", []string{}}, {w.renM, "0.6", []string{"low"}}} {
		mr := memberResult(t, team, c.who)
		if !mr.Factor.Equal(dec(c.factor)) || !slices.Equal(mr.Flags, c.flags) || !mr.Submitted || len(mr.RatedBy) != 3 || len(mr.Shares) != 3 ||
			mr.Score != nil || mr.Self != nil {
			t.Fatalf("%s's results: %+v", c.who, mr)
		}
	}
	if len(team.Sheets) != 4 || team.Sheets[0].RaterName == nil || team.Window.State != "open" || team.SubmissionID == nil {
		t.Fatalf("the Team's sheets: %+v", team)
	}
	duo := groupNamed(t, r, w.duo)
	if !slices.Equal(duo.Flags, []string{"pair_without_self_evaluation"}) || len(duo.Sheets) != 0 ||
		!slices.Equal(memberResult(t, duo, w.mioM).Flags, []string{"missing"}) || !memberResult(t, duo, w.mioM).Factor.Equal(dec("1")) {
		t.Fatalf("the Duo's results: %+v", duo)
	}
	// A TA, who enters grades and posts none, reads them; the grader, listed
	// for HW3 alone, does not.
	mei := b.person(t, "Mei", "")
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": mei, "preset": "ta"})
	if r := w.results(t, mei, nil); len(r.Groups) != 2 {
		t.Fatalf("the TA's results: %+v", r.Groups)
	}
	b.refusedAs(t, b.grader, "peer_review.results", m{"course_id": b.course, "assignment_id": w.hw}, apperr.Forbidden, "")
	// A TA listed for Yuki alone reaches no circle wholly: none is shown,
	// and the Team's asked for is refused.
	jun := b.person(t, "Jun", "")
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": jun, "preset": "ta", "student_scope": "listed", "listed_students": []uuid.UUID{b.yukiM}})
	if r := w.results(t, jun, nil); len(r.Groups) != 0 {
		t.Fatalf("Jun's results: %+v", r.Groups)
	}
	b.refusedAs(t, jun, "peer_review.results", m{"course_id": b.course, "assignment_id": w.hw, "group_id": w.team}, apperr.Forbidden, "student_out_of_scope")
	if one := w.results(t, b.sato, m{"group_id": w.duo}); len(one.Groups) != 1 || one.Groups[0].GroupID != w.duo {
		t.Fatalf("the Duo's alone: %+v", one.Groups)
	}
}

// Counted, peer evaluation moves each member's grade from the group's at the
// form's weight, once the window has closed: the design's example. A
// grader's own adjustment wins; a regrade and a rescale work it out again;
// each member reads their own, and their own average, never another's.
func TestPeerEvaluationCountsInGrades(t *testing.T) {
	w := buildPeerGroups(t)
	b := w.built
	w.shareForm(t, time.Now().Add(time.Hour))
	work := w.handIn(t, b.ken, "The Team's report")
	duoWork := w.handIn(t, w.hana, "The Duo's report")
	w.designSheets(t)

	// Graded while the window is open: nothing counted yet, said so.
	graded := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": work, "score": 80}))
	if graded.Peer != "window_open" {
		t.Fatalf("graded while open: %+v", graded)
	}
	for _, mg := range graded.MemberGrades {
		if !mg.Score.Equal(dec("80")) || mg.Adjustment != nil {
			t.Fatalf("a member's grade while open: %+v", mg)
		}
	}
	apply := m{"course_id": b.course, "assignment_id": w.hw}
	b.refusedAs(t, b.sato, "grade.apply_peer", apply, apperr.FailedPrecondition, tools.ReasonWindowOpen)
	if r := groupNamed(t, w.results(t, b.sato, nil), w.team); r.GroupScore == nil || !r.GroupScore.Equal(dec("80")) ||
		!memberResult(t, r, w.renM).Score.Equal(dec("73.6")) || memberResult(t, r, w.renM).Grade == nil {
		t.Fatalf("what counting it would give: %+v", r)
	}

	// Closed: sheets refused, and counted.
	w.closeForm(t)
	b.refusedAs(t, b.yuki, "peer_review.submit", w.sheetArgs(b.kenM, 40, w.aoiM, 40, w.renM, 20), apperr.FailedPrecondition, tools.ReasonWindowClosed)
	out := testkit.Result[tools.GradeApplyPeerOut](t, b.do(t, b.sato, "grade.apply_peer", apply))
	want := map[uuid.UUID][2]string{b.yukiM: {"83.2", "1.2"}, b.kenM: {"81.6", "1.1"}, w.aoiM: {"81.6", "1.1"}, w.renM: {"73.6", "0.6"}}
	sum := decimal.Zero
	for _, wr := range out.Written {
		wnt, ok := want[wr.StudentMemberID]
		if !ok || !wr.Score.Equal(dec(wnt[0])) || wr.Posted || !wr.Before.Equal(dec("80")) || wr.Adjustment == nil || wr.Adjustment.Kind != "peer" ||
			wr.Adjustment.Detail == nil || !wr.Adjustment.Detail.Factor.Equal(dec(wnt[1])) || wr.Adjustment.Detail.Weight != 20 {
			t.Fatalf("written: %+v", wr)
		}
		sum = sum.Add(wr.Adjustment.Points)
	}
	if len(out.Written) != 4 || !sum.IsZero() || out.Snapshots != 0 {
		t.Fatalf("counted: %+v, the adjustments adding up to %s", out, sum)
	}
	if again := testkit.Result[tools.GradeApplyPeerOut](t, b.do(t, b.sato, "grade.apply_peer", apply)); len(again.Written) != 0 {
		t.Fatalf("counted twice: %+v", again)
	}

	// Sato's own adjustment of Ren wins, and stays through counting again;
	// taken away, peer evaluation's comes back.
	renGrade := func() uuid.UUID {
		var id uuid.UUID
		if err := b.Pool.QueryRow(t.Context(), `SELECT id FROM grade WHERE submission_id = $1 AND student_member_id = $2 AND superseded_by IS NULL`,
			work, w.renM).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	adj := testkit.Result[tools.GradeAdjustOut](t, b.do(t, b.sato, "grade.adjust", m{"course_id": b.course, "grade_id": renGrade(),
		"kind": "replace", "points": 76, "reason": "Did the fieldwork alone"}))
	if !adj.Score.Equal(dec("76")) || adj.Adjustment == nil || adj.Adjustment.Kind != "replace" || adj.Peer != "counted" {
		t.Fatalf("Ren adjusted by Sato: %+v", adj)
	}
	if again := testkit.Result[tools.GradeApplyPeerOut](t, b.do(t, b.sato, "grade.apply_peer", apply)); len(again.Written) != 0 {
		t.Fatalf("counting it again over Sato's adjustment: %+v", again)
	}
	adj = testkit.Result[tools.GradeAdjustOut](t, b.do(t, b.sato, "grade.adjust", m{"course_id": b.course, "grade_id": renGrade(), "kind": "none"}))
	if !adj.Score.Equal(dec("73.6")) || adj.Adjustment == nil || adj.Adjustment.Kind != "peer" || !adj.Changed {
		t.Fatalf("Ren's adjustment taken away: %+v", adj)
	}
	if same := testkit.Result[tools.GradeAdjustOut](t, b.do(t, b.sato, "grade.adjust", m{"course_id": b.course, "grade_id": adj.GradeID,
		"kind": "none"})); same.Changed {
		t.Fatalf("taken away again: %+v", same)
	}

	// The Duo, whom nobody rated, is given the group's score.
	duo := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": duoWork, "score": 70}))
	if duo.Peer != "counted" || len(duo.MemberGrades) != 2 || duo.MemberGrades[0].Adjustment != nil || !duo.MemberGrades[0].Score.Equal(dec("70")) {
		t.Fatalf("the Duo graded: %+v", duo)
	}

	// Posted, each reads their own: the group's score, and their peer
	// adjustment, its factor and the weight; not how many rated them.
	b.do(t, b.sato, "grade.post", apply)
	yuki := testkit.Result[tools.GradeListOut](t, b.do(t, b.yuki, "grade.list", m{"course_id": b.course, "assignment_id": w.hw})).Grades
	if len(yuki) != 1 || !yuki[0].Score.Equal(dec("83.2")) || yuki[0].Group == nil || !yuki[0].Group.Score.Equal(dec("80")) {
		t.Fatalf("Yuki's grades: %+v", yuki)
	}
	a := yuki[0].Group.Adjustment
	if a == nil || a.Kind != "peer" || !a.Points.Equal(dec("3.2")) || a.Reason != nil || a.ByMemberID != nil || a.Detail == nil ||
		!a.Detail.Factor.Equal(dec("1.2")) || a.Detail.Weight != 20 || a.Detail.Raters != nil || a.Detail.FormVersion != nil {
		t.Fatalf("Yuki's adjustment as she reads it: %+v %+v", a, a.Detail)
	}
	b.refusedAs(t, w.ren, "grade.get", m{"course_id": b.course, "grade_id": yuki[0].ID}, apperr.Forbidden, "student_out_of_scope")
	staff := testkit.Result[tools.GradeView](t, b.do(t, b.sato, "grade.get", m{"course_id": b.course, "grade_id": yuki[0].ID}))
	if d := staff.Group.Adjustment.Detail; d.Raters == nil || *d.Raters != 3 || d.FormVersion == nil || *d.FormVersion != w.version {
		t.Fatalf("Yuki's adjustment as Sato reads it: %+v", d)
	}
	// Their own averages, once closed, from two peers or more.
	for who, pct := range map[uuid.UUID]string{b.yuki: "120", w.ren: "60"} {
		got := w.peerForm(t, who).Task.OwnAverage
		if got == nil || got.SharePercent == nil || !got.SharePercent.Equal(dec(pct)) || got.Averages != nil {
			t.Fatalf("%s's own average: %+v", who, got)
		}
	}
	if got := w.peerForm(t, w.hana).Task.OwnAverage; got != nil {
		t.Fatalf("Hana, rated by nobody, is shown an average: %+v", got)
	}

	// A regrade of the Team works it out again from the new score; Ren's
	// grade is regraded with the rest.
	re := testkit.Result[tools.GradeRegradeOut](t, b.do(t, b.sato, "grade.regrade", m{"course_id": b.course, "grade_id": yuki[0].ID, "score": 90}))
	scores := map[uuid.UUID]string{}
	for _, mg := range re.MemberGrades {
		scores[mg.StudentMemberID] = mg.Score.String()
	}
	if re.Peer != "counted" || scores[b.yukiM] != "93.6" || scores[b.kenM] != "91.8" || scores[w.aoiM] != "91.8" || scores[w.renM] != "82.8" {
		t.Fatalf("the Team regraded: %+v %v", re, scores)
	}
	// A weight changed rewrites nothing until it is counted again; then the
	// posted grades are regraded, the totals with them.
	w.setForm(t, m{"kind": "share", "opens": "on_hand_in", "closes_at": time.Now().Add(-time.Minute), "weight": 50})
	out = testkit.Result[tools.GradeApplyPeerOut](t, b.do(t, b.sato, "grade.apply_peer", apply))
	if len(out.Written) != 4 || out.Snapshots == 0 || !out.Written[0].Posted {
		t.Fatalf("counted at 50 %%: %+v", out)
	}
	// Halving the points works each peer adjustment out again from its
	// factor: 45 for the group, Yuki 45 × 1.1 and Ren 45 × 0.8.
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": w.hw, "points_possible": 50, "existing_grades": "rescale"})
	for who, want := range map[uuid.UUID]string{b.yuki: "49.5", w.ren: "36"} {
		g := testkit.Result[tools.GradeListOut](t, b.do(t, who, "grade.list", m{"course_id": b.course, "assignment_id": w.hw})).Grades
		if len(g) != 1 || !g[0].Score.Equal(dec(want)) || !g[0].Group.Score.Equal(dec("45")) || g[0].Group.Adjustment.Kind != "peer" {
			t.Fatalf("%s's grade rescaled: %+v", who, g)
		}
	}
	// At a weight of 0 it counts in nothing.
	w.setForm(t, m{"kind": "share", "opens": "on_hand_in", "closes_at": time.Now().Add(-time.Minute), "weight": 0})
	b.refusedAs(t, b.sato, "grade.apply_peer", apply, apperr.FailedPrecondition, tools.ReasonPeerNotCounted)
}

// A peer evaluation is a person's: a student's own agent, which works on her
// group's work for her, reads her task and writes no sheet, refused as
// people_only and recorded.
func TestAnAgentWritesNoPeerEvaluation(t *testing.T) {
	w := buildPeerGroups(t)
	b := w.built
	w.shareForm(t, time.Now().Add(time.Hour))
	w.handIn(t, b.ken, "The Team's report")
	bot := b.agent(t, b.yuki, "Yuki's helper")
	seat := b.delegate(t, b.yuki, bot, m{})
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": seat, "perms": m{"submission_write": "confirm_required"}})

	if task := w.peerForm(t, bot).Task; task == nil || task.GroupID != w.team || !task.InCircle || len(task.ToEvaluate) != 3 {
		t.Fatalf("Yuki's task as her agent reads it: %+v", task)
	}
	out := b.MustCall(bot, "peer_review.submit", w.sheetArgs(b.kenM, 40, w.aoiM, 40, w.renM, 20), "bot-sheet")
	if out.Status != domain.StatusFailed || out.Error == nil || out.Error.Code != apperr.Forbidden || reason(out) != tools.PeerPeopleOnly {
		t.Fatalf("the agent's sheet: %+v", out)
	}
	if n := b.Count(`SELECT count(*) FROM peer_review`); n != 0 {
		t.Fatalf("%d sheets written", n)
	}
	// Nor does the grader, an agent that grades, write one for anyone.
	b.refusedAs(t, b.grader, "peer_review.submit", w.sheetArgs(b.kenM, 40, w.aoiM, 40, w.renM, 20), apperr.Forbidden, "")
}

// Counting peer evaluation by proposal records each member's factor and the
// form's version; approving it is refused once either has changed.
func TestCountingPeerEvaluationIsProposedWithItsFactors(t *testing.T) {
	w := buildPeerGroups(t)
	b := w.built
	w.shareForm(t, time.Now().Add(time.Hour))
	work := w.handIn(t, b.ken, "The Team's report")
	w.designSheets(t)
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": work, "score": 80})
	w.closeForm(t)
	// Mei enters and posts grades, each by proposal.
	mei := b.person(t, "Mei", "")
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": mei, "preset": "ta",
		"perms": m{"grade_submit": "confirm_required", "grade_post": "confirm_required"}})
	propose := func(key string) pipeline.Outcome {
		out := b.MustCall(mei, "grade.apply_peer", m{"course_id": b.course, "assignment_id": w.hw}, key)
		if out.Status != domain.StatusProposed {
			t.Fatalf("Mei counting it: %+v", out)
		}
		return out
	}
	b.refusedAs(t, b.sato, "grade.apply_peer", m{"course_id": b.course, "assignment_id": w.hw, "form_version": 1}, apperr.InvalidArgument, "")

	first := propose("mei-1")
	var payload tools.GradeApplyPeerIn
	var raw []byte
	if err := b.Pool.QueryRow(t.Context(), `SELECT payload FROM action WHERE id = $1`, first.ActionID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.FormVersion == nil || *payload.FormVersion != w.version || len(payload.Grades) != 4 {
		t.Fatalf("the proposal records %s: %v", raw, err)
	}
	for _, g := range payload.Grades {
		if g.StudentMemberID == w.renM && (g.Factor == nil || !g.Factor.Equal(dec("0.6"))) {
			t.Fatalf("Ren's factor recorded: %+v", g)
		}
	}
	// The weight changes meanwhile: approving it is refused.
	w.setForm(t, m{"kind": "share", "opens": "on_hand_in", "closes_at": time.Now().Add(-time.Minute), "weight": 25})
	res := testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": first.ActionID, "decision": "approve"}))
	if res.Outcome != domain.StatusFailed {
		t.Fatalf("approving after the form changed: %+v", res)
	}
	var why string
	if err := b.Pool.QueryRow(t.Context(), `SELECT result->'error'->'details'->>'reason' FROM action WHERE id = $1`, first.ActionID).Scan(&why); err != nil ||
		why != tools.ReasonGradesChanged {
		t.Fatalf("why: %q %v", why, err)
	}
	// Proposed again, and approved: written as proposed.
	second := propose("mei-2")
	res = testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": second.ActionID, "decision": "approve"}))
	var written tools.GradeApplyPeerOut
	if err := json.Unmarshal(res.Result, &written); res.Outcome != domain.StatusExecuted || err != nil || len(written.Written) != 4 {
		t.Fatalf("approved: %+v %v", res, err)
	}
	// Nothing left to count: a proposal is not made.
	out := b.MustCall(mei, "grade.apply_peer", m{"course_id": b.course, "assignment_id": w.hw}, "mei-3")
	if out.Status != domain.StatusFailed || out.Error == nil || out.Error.Code != apperr.FailedPrecondition {
		t.Fatalf("proposing it with nothing to write: %+v", out)
	}
}

// An assignment deleted for good takes its peer form and every sheet with
// it, counted; an agent deletes none with a peer evaluation, which is a
// person's; its groups, the course's, stay.
func TestAnAssignmentWithPeerEvaluationIsDeletedForGood(t *testing.T) {
	w := buildPeerGroups(t)
	b := w.built
	// A lab whose form opened before anyone handed anything in: sheets and no
	// work.
	lab := b.groupAssignment(t, "Lab", w.set)
	b.do(t, b.sato, "peer_form.set", m{"course_id": b.course, "assignment_id": lab, "kind": "share", "opens": "at",
		"opens_at": time.Now().Add(-time.Minute), "closes_at": time.Now().Add(time.Hour), "weight": 0})
	sheet := func(actor uuid.UUID, pairs ...any) {
		args := w.sheetArgs(pairs...)
		args["assignment_id"] = lab
		b.do(t, actor, "peer_review.submit", args)
	}
	sheet(w.hana, w.mioM, 100)
	sheet(w.mio, w.hanaM, 100)
	sheet(w.mio, w.hanaM, 100)

	bot := b.agent(t, b.sato, "Sato's assistant")
	b.delegate(t, b.sato, bot, m{"perms": m{"assignment_write": "autonomous"}, "student_scope": "all"})
	p := b.deletionPreview(t, bot, lab)
	if refusal(p) != tools.DeletePeopleOnly || p.Counts.Submissions != 0 || p.Counts.PeerReviews != 2 {
		t.Fatalf("the agent's preview of the lab: %+v", p)
	}
	// A page that knew nothing of peer evaluation confirms none, and is
	// refused; shown them, Sato deletes it.
	b.refusedAs(t, b.sato, "assignment.delete", deleteArgs(b, lab, tools.DeletionCounts{}), apperr.Conflict, tools.DeleteConfirmStale)
	p = b.deletionPreview(t, b.sato, lab)
	gone := testkit.Result[tools.AssignmentDeleteOut](t, b.do(t, b.sato, "assignment.delete", deleteArgs(b, lab, p.Counts)))
	if gone.Removed.PeerReviews != 2 {
		t.Fatalf("deleted: %+v", gone)
	}
	for _, table := range []string{"peer_form", "peer_review"} {
		if n := b.Count(`SELECT count(*) FROM `+table+` WHERE assignment_id = $1`, lab); n != 0 {
			t.Fatalf("%d rows of %s left", n, table)
		}
	}
	if n := b.Count(`SELECT count(*) FROM peer_review_entry`); n != 0 {
		t.Fatalf("%d entries left", n)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE action_type = 'peer_review.submit' AND payload <> '{}'::jsonb`); n != 0 {
		t.Fatalf("%d sheets still in the action log", n)
	}
	if n := b.Count(`SELECT count(*) FROM course_group WHERE set_id = $1`, w.set); n != 2 {
		t.Fatalf("the groups went with it: %d", n)
	}
}

// Two sheets of one rater at once are written one after the other, the
// second replacing the first: never two current.
func TestARatersSheetsAtOnceAreOneAfterTheOther(t *testing.T) {
	w := buildPeerGroups(t)
	b := w.built
	w.shareForm(t, time.Now().Add(time.Hour))
	w.handIn(t, b.ken, "The Team's report")
	release := heldBy(t, b, `SELECT 1 FROM peer_form WHERE assignment_id = $1 FOR UPDATE`, w.hw)
	one := b.inFlight(b.yuki, "peer_review.submit", w.sheetArgs(b.kenM, 40, w.aoiM, 40, w.renM, 20), "yuki-one")
	two := b.inFlight(b.yuki, "peer_review.submit", w.sheetArgs(b.kenM, 20, w.aoiM, 40, w.renM, 40), "yuki-two")
	b.waitingFor(t, 2, one, two)
	release()
	replaced := 0
	for _, out := range []pipeline.Outcome{settled(t, one), settled(t, two)} {
		if out.Status != domain.StatusExecuted {
			t.Fatalf("a sheet: %+v", out)
		}
		if testkit.Result[tools.PeerReviewSubmitOut](t, out).Replaces != nil {
			replaced++
		}
	}
	if replaced != 1 || b.Count(`SELECT count(*) FROM peer_review WHERE superseded_by IS NULL`) != 1 || b.Count(`SELECT count(*) FROM peer_review`) != 2 {
		t.Fatalf("two sheets at once: %d replaced one", replaced)
	}
}

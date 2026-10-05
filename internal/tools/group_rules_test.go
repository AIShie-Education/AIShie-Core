package tools_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// twoGroups is a group assignment worth 100 with two groups that have each
// handed work in, with a file, and been graded, Team A's feedback a file of
// its own, and posted: Yuki and Ken in Team A, Aoi in Team B.
type twoGroups struct {
	*built
	aoi, aoiM           uuid.UUID
	set, teamA, teamB   uuid.UUID
	hw                  uuid.UUID
	workA, workB        uuid.UUID
	fileA, feedbackA    uuid.UUID
	groupGradeA         uuid.UUID
	gradesA, gradesB    []uuid.UUID
	yukiGrade, kenGrade uuid.UUID
	aoiGrade            uuid.UUID
}

func buildTwoGroups(t *testing.T) *twoGroups {
	t.Helper()
	b := build(t)
	actors, s := b.seatStudents(t, "Aoi")
	w := &twoGroups{built: b, aoi: actors["Aoi"], aoiM: s["Aoi"]}
	w.set = b.groupSet(t, "Projects", nil)
	g := b.groupsIn(t, w.set, m{"name": "Team A"}, m{"name": "Team B"})
	w.teamA, w.teamB = g[0], g[1]
	b.place(t, w.set, false, b.yukiM, w.teamA, b.kenM, w.teamA, w.aoiM, w.teamB)
	w.hw = b.groupAssignment(t, "Project", w.set)

	w.workA = testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create",
		m{"course_id": b.course, "assignment_id": w.hw, "body": "Team A's report"})).SubmissionID
	w.fileA = testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.ken, "document.create", m{"course_id": b.course,
		"kind": "submission", "submission_id": w.workA, "title": "report.pdf", "files": b.fileOn(t, b.ken, "submission", "report.pdf")})).DocumentID
	b.do(t, b.ken, "submission.submit", m{"course_id": b.course, "submission_id": w.workA})
	w.workB = testkit.Result[tools.SubmissionCreateOut](t, b.do(t, w.aoi, "submission.create",
		m{"course_id": b.course, "assignment_id": w.hw, "body": "Team B's report"})).SubmissionID
	b.do(t, w.aoi, "submission.submit", m{"course_id": b.course, "submission_id": w.workB})

	ga := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": w.workA,
		"score": 80, "feedback": "Strong analysis.", "feedback_files": []m{{"title": "Marked report", "upload_token": b.upload(t, b.sato, "feedback", "application/pdf", []byte("%PDF marked"))}},
		"adjustments": []m{{"student_member_id": b.kenM, "kind": "delta", "points": -5, "reason": "Late with his part"}}}))
	gb := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": w.workB, "score": 60}))
	w.groupGradeA = *ga.GroupGradeID
	for _, mg := range ga.MemberGrades {
		w.gradesA = append(w.gradesA, mg.GradeID)
		switch mg.StudentMemberID {
		case b.yukiM:
			w.yukiGrade = mg.GradeID
		case b.kenM:
			w.kenGrade = mg.GradeID
		}
	}
	for _, mg := range gb.MemberGrades {
		w.gradesB = append(w.gradesB, mg.GradeID)
		w.aoiGrade = mg.GradeID
	}
	if err := b.Pool.QueryRow(t.Context(), `SELECT id FROM document WHERE group_grade_id = $1`, w.groupGradeA).Scan(&w.feedbackA); err != nil {
		t.Fatalf("Team A's feedback file: %v", err)
	}
	return w
}

// No group learns of another's work: a member of Team B, through every read
// and the feed, finds nothing of Team A's work, its files, its grades, its
// feedback, its members or their adjustments.
func TestNoGroupLearnsOfAnothersWork(t *testing.T) {
	w := buildTwoGroups(t)
	b := w.built

	// Before anything is posted, Team A's feedback is nobody's to read but
	// those who grade.
	b.refusedAs(t, b.yuki, "document.get", m{"course_id": b.course, "document_id": w.feedbackA}, apperr.NotFound, "")
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "assignment_id": w.hw})
	// Posted, each of Team A reads it; Aoi does not.
	b.do(t, b.yuki, "document.get", m{"course_id": b.course, "document_id": w.feedbackA})
	yukisGrade := testkit.Result[tools.GradeView](t, b.do(t, b.yuki, "grade.get", m{"course_id": b.course, "grade_id": w.yukiGrade}))
	if len(yukisGrade.FeedbackFiles) != 1 || yukisGrade.FeedbackFiles[0].DocumentID != w.feedbackA {
		t.Fatalf("Yuki's grade's feedback files: %+v", yukisGrade.FeedbackFiles)
	}

	secrets := []string{w.workA.String(), w.fileA.String(), w.feedbackA.String(), w.groupGradeA.String(),
		w.yukiGrade.String(), w.kenGrade.String(), b.yukiM.String(), b.kenM.String(), "Team A's report", "Strong analysis.",
		"Late with his part"}
	leaks := func(where string, v any) {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		for _, sec := range secrets {
			if strings.Contains(string(raw), sec) {
				t.Errorf("%s shows Aoi %q: %s", where, sec, raw)
			}
		}
	}
	reads := map[string]m{
		"submission.list":   {},
		"grade.list":        {},
		"assignment.get":    {"assignment_id": w.hw},
		"group_set.list":    {},
		"group_set.get":     {"set_id": w.set, "include_history": true},
		"submission.get":    {"submission_id": w.workB},
		"submission.roster": {"assignment_id": w.hw},
		"grade.get":         {"grade_id": w.aoiGrade},
		"gradebook.get":     {"student_member_id": w.aoiM},
	}
	for tool, args := range reads {
		args["course_id"] = b.course
		out := b.MustCall(w.aoi, tool, args, "")
		if out.Status != domain.StatusExecuted {
			t.Fatalf("Aoi's %s: %+v", tool, out)
		}
		leaks(tool, out.Result)
	}
	leaks("the feed", feed(t, b, w.aoi))
	// And each of Team A's things refuses her, saying nothing more.
	for tool, args := range map[string]m{
		"submission.get": {"submission_id": w.workA},
		"grade.get":      {"grade_id": w.yukiGrade},
		"document.get":   {"document_id": w.fileA},
	} {
		args["course_id"] = b.course
		b.refusedAs(t, w.aoi, tool, args, apperr.Forbidden, "student_out_of_scope")
	}
	b.refusedAs(t, w.aoi, "document.get", m{"course_id": b.course, "document_id": w.feedbackA}, apperr.Forbidden, "student_out_of_scope")
	// Within Team A, Yuki reads nothing of Ken's grade.
	b.refusedAs(t, b.yuki, "grade.get", m{"course_id": b.course, "grade_id": w.kenGrade}, apperr.Forbidden, "student_out_of_scope")
	for _, e := range feed(t, b, b.yuki) {
		if e.StudentMemberID != nil && *e.StudentMemberID != b.yukiM {
			t.Errorf("Yuki's feed shows an event of %s: %+v", e.StudentMemberID, e)
		}
	}
	// The feed tells each member of their group's hand-in, under their own
	// seat.
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'submission.submitted' AND subject_id = $1`, w.workA); n != 2 {
		t.Fatalf("%d hand-in events for Team A, want one per member", n)
	}
}

// A student's own agent reaches its student's group's work as its student
// does, and signs up for her by proposal, which she confirms.
func TestAStudentsAgentWorksForItsStudentsGroup(t *testing.T) {
	b := build(t)
	set := b.groupSet(t, "Projects", m{"signup_open": true})
	g := b.groupsIn(t, set, m{"name": "Team A"}, m{"name": "Team B"})
	b.place(t, set, false, b.kenM, g[0])
	hw := b.groupAssignment(t, "Project", set)
	bot := b.agent(t, b.yuki, "Yuki's helper")
	seat := b.delegate(t, b.yuki, bot, m{})
	// Sato lets it write her work, by proposal.
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": seat, "perms": m{"submission_write": "confirm_required"}})

	// Its sign-up is a proposal, which Yuki approves.
	out := b.MustCall(bot, "group.sign_up", m{"course_id": b.course, "set_id": set, "group_id": g[0]}, "bot-signup")
	if out.Status != domain.StatusProposed {
		t.Fatalf("the agent's sign-up: %+v", out)
	}
	b.do(t, b.yuki, "action.decide", m{"course_id": b.course, "action_id": out.ActionID, "decision": "approve"})
	if v := b.setView(t, b.yuki, set, false); v.MyGroupID == nil || *v.MyGroupID != g[0] {
		t.Fatalf("Yuki's group after her agent's sign-up: %+v", v.MyGroupID)
	}
	// It reads her group's draft, Ken's start of it.
	work := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.ken, "submission.create", m{"course_id": b.course, "assignment_id": hw, "body": "Ken's start"}))
	read := testkit.Result[tools.SubmissionView](t, b.do(t, bot, "submission.get", m{"course_id": b.course, "submission_id": work.SubmissionID}))
	if read.Body == nil || *read.Body != "Ken's start" {
		t.Fatalf("the agent reads %+v", read)
	}
	// Its hand-in is a proposal, which records whom it is for, and is
	// refused on approval once the group's members have changed.
	prop := b.MustCall(bot, "submission.submit", m{"course_id": b.course, "submission_id": work.SubmissionID}, "bot-submit")
	if prop.Status != domain.StatusProposed {
		t.Fatalf("the agent's hand-in: %+v", prop)
	}
	var members []uuid.UUID
	var raw []byte
	if err := b.Pool.QueryRow(t.Context(), `SELECT payload->'members' FROM action WHERE id = $1`, prop.ActionID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &members); err != nil || !sameSet(members, []uuid.UUID{b.yukiM, b.kenM}) {
		t.Fatalf("the proposal records %s: %v", raw, err)
	}
	_, s := b.seatStudents(t, "Aoi")
	b.place(t, set, true, s["Aoi"], g[0])
	// Yuki would be refused carrying it out, and so does not decide it as
	// its owner; Sato, who decides the course's actions, approves it, and
	// it fails.
	decided := b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": prop.ActionID, "decision": "approve"})
	if res := testkit.Result[pipeline.DecideOut](t, decided); res.Outcome != domain.StatusFailed {
		t.Fatalf("approving the hand-in after the members changed: %+v", res)
	}
	var reason string
	if err := b.Pool.QueryRow(t.Context(), `SELECT result->'error'->'details'->>'reason' FROM action WHERE id = $1`, prop.ActionID).Scan(&reason); err != nil || reason != tools.ReasonMembersChanged {
		t.Fatalf("why: %q %v", reason, err)
	}
}

// A grade proposed for a group's work records whose work it is and every
// member's adjustment as it will be written; approving it after the work's
// members changed is refused.
func TestAGroupGradeProposalRecordsItsMembers(t *testing.T) {
	w := buildTwoGroups(t)
	b := w.built
	// Team A's work, graded again by the grader before anything is posted:
	// it is listed for HW3 alone, so Sato lists it for the project too.
	b.do(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": b.graderM, "assignment_scope": "listed",
		"listed_assignments": []uuid.UUID{b.hw3, w.hw}})
	prop := b.MustCall(b.grader, "grade.submit", m{"course_id": b.course, "submission_id": w.workA, "score": 70}, "grader-a")
	if prop.Status != domain.StatusProposed {
		t.Fatalf("the grader's grade: %+v", prop)
	}
	var payload struct {
		Members     []uuid.UUID          `json:"members"`
		Adjustments []tools.AdjustmentIn `json:"adjustments"`
	}
	var raw []byte
	if err := b.Pool.QueryRow(t.Context(), `SELECT payload FROM action WHERE id = $1`, prop.ActionID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &payload); err != nil || !sameSet(payload.Members, []uuid.UUID{b.yukiM, b.kenM}) || len(payload.Adjustments) != 2 {
		t.Fatalf("the proposal records %s: %v", raw, err)
	}
	carried := false
	for _, a := range payload.Adjustments {
		if a.StudentMemberID == b.kenM && a.Kind == "delta" && a.Points.Equal(decimal.NewFromInt(-5)) {
			carried = true
		}
	}
	if !carried {
		t.Fatalf("Ken's adjustment is not carried into the proposal: %+v", payload.Adjustments)
	}
	// Mio is added to Team A's work meanwhile: approving it is refused.
	_, s := b.seatStudents(t, "Mio")
	b.do(t, b.sato, "submission.set_members", m{"course_id": b.course, "submission_id": w.workA, "add": []uuid.UUID{s["Mio"]}})
	res := testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": prop.ActionID, "decision": "approve"}))
	if res.Outcome != domain.StatusFailed {
		t.Fatalf("approving after the members changed: %+v", res)
	}
}

// Deleting a group assignment takes its groups' work, the group grades and
// their feedback files with it, and leaves the groups, which are the
// course's.
func TestAGroupAssignmentIsDeletedForGood(t *testing.T) {
	w := buildTwoGroups(t)
	b := w.built
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "assignment_id": w.hw})
	p := b.deletionPreview(t, b.sato, w.hw)
	if p.Counts.Submissions != 2 || p.Counts.HandedIn != 2 || p.Counts.Grades != 3 || p.Counts.Posted != 3 || p.Counts.Files != 2 {
		t.Fatalf("what goes: %+v", p.Counts)
	}
	out := testkit.Result[tools.AssignmentDeleteOut](t, b.do(t, b.sato, "assignment.delete", deleteArgs(b, w.hw, p.Counts)))
	if !out.Deleted || out.FilesQueued < 2 {
		t.Fatalf("the deletion: %+v", out)
	}
	for _, q := range []string{
		`SELECT count(*) FROM submission WHERE assignment_id = $1`,
		`SELECT count(*) FROM submission_member WHERE assignment_id = $1`,
		`SELECT count(*) FROM group_grade gg WHERE gg.id IN (SELECT group_grade_id FROM grade WHERE group_grade_id IS NOT NULL) AND $1::uuid IS NOT NULL`,
	} {
		if n := b.Count(q, w.hw); n != 0 {
			t.Fatalf("%d left: %s", n, q)
		}
	}
	if n := b.Count(`SELECT count(*) FROM group_grade WHERE id = $1`, w.groupGradeA); n != 0 {
		t.Fatal("Team A's group grade is still there")
	}
	if n := b.Count(`SELECT count(*) FROM document WHERE id = ANY($1)`, []uuid.UUID{w.fileA, w.feedbackA}); n != 0 {
		t.Fatal("a file of the groups' work is still there")
	}
	if n := b.Count(`SELECT count(*) FROM course_group WHERE set_id = $1`, w.set); n != 2 {
		t.Fatal("the groups went with the assignment")
	}
	if v := b.setView(t, b.sato, w.set, false); !sameSet(membersOf(v, w.teamA), []uuid.UUID{b.yukiM, b.kenM}) {
		t.Fatalf("Team A after the deletion: %+v", v.Groups)
	}
}

// What the work is worth changes after a group is graded: the group grade
// and each member's grade from it are rescaled together, adjustments in
// proportion; keep_scores is refused if the group's score would be above the
// new points.
func TestAGroupGradeIsRescaled(t *testing.T) {
	w := buildTwoGroups(t)
	b := w.built
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "assignment_id": w.hw})
	b.refusedAs(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": w.hw, "points_possible": 50,
		"existing_grades": "keep_scores"}, apperr.FailedPrecondition, "score_above_points")
	out := testkit.Result[tools.SchemeChangeOut](t, b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": w.hw,
		"points_possible": 50, "existing_grades": "rescale"}))
	if out.Rescaled != 3 {
		t.Fatalf("rescaled %+v", out)
	}
	ken := testkit.Result[tools.GradeListOut](t, b.do(t, b.ken, "grade.list", m{"course_id": b.course, "assignment_id": w.hw})).Grades
	if len(ken) != 1 || !ken[0].Score.Equal(decimal.NewFromFloat(37.5)) || ken[0].Group == nil || !ken[0].Group.Score.Equal(decimal.NewFromInt(40)) ||
		ken[0].Group.Adjustment == nil || !ken[0].Group.Adjustment.Points.Equal(decimal.NewFromFloat(-2.5)) {
		t.Fatalf("Ken's grade rescaled: %+v %+v", ken, ken[0].Group)
	}
	// The group's feedback goes with its rescaled grade.
	var on uuid.UUID
	if err := b.Pool.QueryRow(t.Context(), `SELECT group_grade_id FROM document WHERE id = $1`, w.feedbackA).Scan(&on); err != nil || on == w.groupGradeA {
		t.Fatalf("Team A's feedback stayed on the old group grade: %v", err)
	}
	if n := b.Count(`SELECT count(*) FROM group_grade WHERE submission_id = $1`, w.workA); n != 2 {
		t.Fatalf("%d group grades of Team A's work, want the first and the rescaled", n)
	}
}

// Two students signing up at once for the last place in a group: the group
// is held while one counts and joins, and the other, counting after, finds
// it full.
func TestTheLastPlaceInAGroupGoesToOneStudent(t *testing.T) {
	b := build(t)
	set := b.groupSet(t, "Projects", m{"signup_open": true})
	group := b.groupsIn(t, set, m{"name": "Solo", "capacity": 1})[0]
	release := heldBy(t, b, `SELECT 1 FROM course_group WHERE id = $1 FOR UPDATE`, group)
	args := m{"course_id": b.course, "set_id": set, "group_id": group}
	yuki := b.inFlight(b.yuki, "group.sign_up", args, "yuki-solo")
	ken := b.inFlight(b.ken, "group.sign_up", args, "ken-solo")
	b.waitingFor(t, 2, yuki, ken)
	release()
	executed, full := 0, 0
	for _, out := range []pipeline.Outcome{settled(t, yuki), settled(t, ken)} {
		switch {
		case out.Status == domain.StatusExecuted:
			executed++
		case out.Error != nil && out.Error.Details["reason"] == tools.ReasonGroupFull:
			full++
		default:
			t.Fatalf("a sign-up: %+v", out)
		}
	}
	if executed != 1 || full != 1 {
		t.Fatalf("%d joined and %d found it full, want one of each", executed, full)
	}
	if v := b.setView(t, b.sato, set, false); v.Groups[0].Size != 1 {
		t.Fatalf("the group's size: %+v", v.Groups[0])
	}
}

// A member's draft grade posted while one who grades but does not post
// adjusts it is not replaced by them: the call was decided as an adjustment
// of a draft, and is refused once it finds the grade posted. Called again,
// it is decided as the regrade it now is, and denied them. Sato, who posts,
// is told the same in the same race, and adjusts it called again.
func TestADraftPostedMeanwhileIsAdjustedOnlyAsARegrade(t *testing.T) {
	w := buildTwoGroups(t)
	b := w.built
	tomo := b.person(t, "Tomo", "")
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": tomo, "preset": "ta"})
	liveScore := func(student uuid.UUID) (decimal.Decimal, bool) {
		t.Helper()
		var score decimal.Decimal
		var posted bool
		if err := b.Pool.QueryRow(t.Context(), `SELECT score, posted_at IS NOT NULL FROM grade
			WHERE submission_id = $1 AND student_member_id = $2 AND superseded_by IS NULL`, w.workA, student).Scan(&score, &posted); err != nil {
			t.Fatal(err)
		}
		return score, posted
	}
	race := func(actor, grade uuid.UUID, key string) pipeline.Outcome {
		t.Helper()
		release := heldBy(t, b, `UPDATE grade SET posted_at = now(), posted_by_member_id = $2 WHERE id = $1`, grade, b.satoM)
		adjust := b.inFlight(actor, "grade.adjust", m{"course_id": b.course, "grade_id": grade, "kind": "replace", "points": 100,
			"reason": "Did it all"}, key)
		b.waitingFor(t, 1, adjust)
		release()
		return settled(t, adjust)
	}

	if out := race(tomo, w.yukiGrade, "tomo-adjust"); out.Status != domain.StatusFailed || out.Error == nil ||
		out.Error.Details["reason"] != tools.ReasonPostedMeanwhile {
		t.Fatalf("Tomo's adjustment of a draft posted meanwhile: %+v", out)
	}
	if score, posted := liveScore(b.yukiM); !score.Equal(decimal.NewFromInt(80)) || !posted {
		t.Fatalf("Yuki's grade after Tomo's adjustment: %s, posted %v", score, posted)
	}
	b.refusedAs(t, tomo, "grade.adjust", m{"course_id": b.course, "grade_id": w.yukiGrade, "kind": "replace", "points": 100,
		"reason": "Did it all"}, apperr.Forbidden, "")

	if out := race(b.sato, w.kenGrade, "sato-adjust"); out.Status != domain.StatusFailed || out.Error == nil ||
		out.Error.Details["reason"] != tools.ReasonPostedMeanwhile {
		t.Fatalf("Sato's adjustment of a draft posted meanwhile: %+v", out)
	}
	again := testkit.Result[tools.GradeAdjustOut](t, b.do(t, b.sato, "grade.adjust", m{"course_id": b.course, "grade_id": w.kenGrade,
		"kind": "replace", "points": 100, "reason": "Did it all"}))
	if !again.Changed || again.Snapshots == 0 {
		t.Fatalf("Sato's adjustment called again: %+v", again)
	}
	if score, posted := liveScore(b.kenM); !score.Equal(decimal.NewFromInt(100)) || !posted {
		t.Fatalf("Ken's grade after Sato's adjustment: %s, posted %v", score, posted)
	}
}

// A placement and a write to a group's work are one after the other: a
// student moved out of her group while she starts its work, edits its draft
// or hands it in writes nothing of the group she left, and nothing of hers
// lands where the students moved in would read it.
func TestAMoveAndAWriteToAGroupsWorkAreOneAfterTheOther(t *testing.T) {
	b := build(t)
	actors, s := b.seatStudents(t, "Aoi", "Mio")
	set := b.groupSet(t, "Projects", nil)
	g := b.groupsIn(t, set, m{"name": "Team A"}, m{"name": "Team B"})
	teamA, teamB := g[0], g[1]
	b.place(t, set, false, b.yukiM, teamA, b.kenM, teamA, s["Aoi"], teamB, s["Mio"], teamB)
	hw := b.groupAssignment(t, "Project", set)
	// race holds group as a placement does, starts Sato's placements, which
	// wait for it first, and then call, which waits after them; and returns
	// what each came back with.
	race := func(group uuid.UUID, placements []m, affectsWork bool, actor uuid.UUID, name string, args m) (pipeline.Outcome, pipeline.Outcome) {
		t.Helper()
		release := heldBy(t, b, `SELECT 1 FROM course_group WHERE id = $1 FOR UPDATE`, group)
		move := b.inFlight(b.sato, "group.set_members", m{"course_id": b.course, "set_id": set, "placements": placements,
			"affects_work": affectsWork}, "move-"+uuid.NewString())
		b.waitingFor(t, 1, move)
		write := b.inFlight(actor, name, args, "write-"+uuid.NewString())
		b.waitingFor(t, 2, move, write)
		release()
		return settled(t, move), settled(t, write)
	}

	// Sato swaps Yuki and Aoi, with no work to acknowledge, while Yuki starts
	// Team A's: she is in Team B by the time it would start, and starts
	// nothing.
	moved, started := race(teamA, []m{{"student_member_id": b.yukiM, "group_id": teamB}, {"student_member_id": s["Aoi"], "group_id": teamA}},
		false, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": hw, "body": "Yuki's private plan"})
	if moved.Status != domain.StatusExecuted {
		t.Fatalf("the swap: %+v", moved)
	}
	if started.Status != domain.StatusFailed || started.Error == nil || started.Error.Details["reason"] != tools.ReasonGroupChanged {
		t.Fatalf("Yuki's start of the work of the group she was moved out of: %+v", started)
	}
	if n := b.Count(`SELECT count(*) FROM submission WHERE assignment_id = $1`, hw); n != 0 {
		t.Fatalf("%d submissions after Yuki's start was refused", n)
	}

	// Ken starts Team A's draft; Sato moves him to Team B, the draft
	// following the group, while he edits it: the edit is refused.
	draft := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.ken, "submission.create",
		m{"course_id": b.course, "assignment_id": hw, "body": "Ken's start"})).SubmissionID
	moved, edited := race(teamA, []m{{"student_member_id": b.kenM, "group_id": teamB}}, true,
		b.ken, "submission.update_draft", m{"course_id": b.course, "submission_id": draft, "body": "Ken's notes", "base_revision": 1})
	if moved.Status != domain.StatusExecuted {
		t.Fatalf("Ken's move: %+v", moved)
	}
	if edited.Status != domain.StatusFailed || edited.Error == nil || edited.Error.Code != apperr.Forbidden {
		t.Fatalf("Ken's edit of the draft of the group he was moved out of: %+v", edited)
	}
	read := testkit.Result[tools.SubmissionView](t, b.do(t, actors["Aoi"], "submission.get", m{"course_id": b.course, "submission_id": draft}))
	if read.Body == nil || *read.Body != "Ken's start" || read.Revision != 1 {
		t.Fatalf("Team A's draft as Aoi reads it: %+v", read)
	}

	// Mio writes Team B's draft; Sato moves her to Team A while she hands
	// it in: the hand-in is refused, and the draft is still Team B's to
	// hand in.
	other := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, actors["Mio"], "submission.create",
		m{"course_id": b.course, "assignment_id": hw, "body": "Team B's report"})).SubmissionID
	moved, handed := race(teamB, []m{{"student_member_id": s["Mio"], "group_id": teamA}}, true,
		actors["Mio"], "submission.submit", m{"course_id": b.course, "submission_id": other})
	if moved.Status != domain.StatusExecuted {
		t.Fatalf("Mio's move: %+v", moved)
	}
	if handed.Status != domain.StatusFailed || handed.Error == nil || handed.Error.Code != apperr.Forbidden {
		t.Fatalf("Mio's hand-in of the draft of the group she was moved out of: %+v", handed)
	}
	if n := b.Count(`SELECT count(*) FROM submission WHERE id = $1 AND state = 'draft'`, other); n != 1 {
		t.Fatal("Team B's draft was handed in by Mio after she left it")
	}
	b.do(t, b.yuki, "submission.submit", m{"course_id": b.course, "submission_id": other})

	// Aoi adds a file to Team A's draft while Sato moves her to Team B: the
	// file is refused.
	moved, filed := race(teamA, []m{{"student_member_id": s["Aoi"], "group_id": teamB}}, true,
		actors["Aoi"], "document.create", m{"course_id": b.course, "kind": "submission", "submission_id": draft, "title": "notes.pdf",
			"files": b.fileOn(t, actors["Aoi"], "submission", "notes.pdf")})
	if moved.Status != domain.StatusExecuted {
		t.Fatalf("Aoi's move: %+v", moved)
	}
	if filed.Status != domain.StatusFailed || filed.Error == nil || filed.Error.Code != apperr.Forbidden {
		t.Fatalf("Aoi's file to the draft of the group she was moved out of: %+v", filed)
	}
	if n := b.Count(`SELECT count(*) FROM document WHERE submission_id = $1`, draft); n != 0 {
		t.Fatalf("%d files on Team A's draft after Aoi's was refused", n)
	}
}

// A student moved into a group after it handed work in is shown nothing of
// that work, in group_set.get or submission.roster: a member reads the
// group's draft and the attempts they are part of. Those who were part of
// it, and Sato, are shown it still; the group's next attempt is hers.
func TestAMemberWhoJoinedLaterIsShownNoEarlierWork(t *testing.T) {
	w := buildTwoGroups(t)
	b := w.built
	b.place(t, w.set, true, w.aoiM, w.teamA)
	workOf := func(v tools.GroupSetView, group uuid.UUID) []uuid.UUID {
		for _, g := range v.Groups {
			if g.ID == group {
				out := []uuid.UUID{}
				for _, wk := range g.Work {
					out = append(out, wk.SubmissionID)
				}
				return out
			}
		}
		return nil
	}
	rosterOf := func(actor uuid.UUID) (tools.SubmissionRosterOut, tools.RosterGroup) {
		t.Helper()
		out := testkit.Result[tools.SubmissionRosterOut](t, b.do(t, actor, "submission.roster", m{"course_id": b.course, "assignment_id": w.hw}))
		for _, g := range out.Groups {
			if g.GroupID == w.teamA {
				return out, g
			}
		}
		t.Fatalf("no Team A in the roster: %+v", out.Groups)
		return out, tools.RosterGroup{}
	}

	if work := workOf(b.setView(t, w.aoi, w.set, false), w.teamA); len(work) != 0 {
		t.Fatalf("Team A's work as Aoi is shown it: %v", work)
	}
	all, teamA := rosterOf(w.aoi)
	if teamA.SubmissionID != nil || teamA.State != "not_started" || teamA.SubmittedByMemberID != nil {
		t.Fatalf("Team A in Aoi's roster: %+v", teamA)
	}
	if raw, _ := json.Marshal(all); strings.Contains(string(raw), w.workA.String()) {
		t.Fatalf("Aoi's roster names Team A's work: %s", raw)
	}
	b.refusedAs(t, w.aoi, "submission.get", m{"course_id": b.course, "submission_id": w.workA}, apperr.Forbidden, "student_out_of_scope")
	for _, who := range []uuid.UUID{b.yuki, b.sato} {
		if work := workOf(b.setView(t, who, w.set, false), w.teamA); !slices.Equal(work, []uuid.UUID{w.workA}) {
			t.Fatalf("Team A's work as %s is shown it: %v", who, work)
		}
		if _, g := rosterOf(who); g.SubmissionID == nil || *g.SubmissionID != w.workA || g.State != "submitted" {
			t.Fatalf("Team A in the roster of %s: %+v", who, g)
		}
	}

	// The group's next attempt, a draft, is hers to see.
	next := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, w.aoi, "submission.create",
		m{"course_id": b.course, "assignment_id": w.hw, "body": "Attempt two"}))
	if next.Attempt != 2 {
		t.Fatalf("Team A's next attempt: %+v", next)
	}
	if work := workOf(b.setView(t, w.aoi, w.set, false), w.teamA); !slices.Equal(work, []uuid.UUID{next.SubmissionID}) {
		t.Fatalf("Team A's work as Aoi is shown it once it has a draft: %v", work)
	}
	if _, g := rosterOf(w.aoi); g.SubmissionID == nil || *g.SubmissionID != next.SubmissionID || g.State != "draft" {
		t.Fatalf("Team A in Aoi's roster once it has a draft: %+v", g)
	}
}

// Approving a proposal to regrade a group's grade is refused once a grade
// it was proposed to replace has been replaced since: Yuki's, adjusted
// meanwhile, which approving it would otherwise write away.
func TestAGroupRegradeProposalIsRefusedOnceAGradeItReplacesChanged(t *testing.T) {
	w := buildTwoGroups(t)
	b := w.built
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "assignment_id": w.hw})
	b.do(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": b.graderM, "assignment_scope": "listed",
		"listed_assignments": []uuid.UUID{b.hw3, w.hw}})
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.graderM, "perms": m{"grade_post": "confirm_required"}})
	prop := b.MustCall(b.grader, "grade.regrade", m{"course_id": b.course, "grade_id": w.kenGrade, "score": 82}, "grader-regrade")
	if prop.Status != domain.StatusProposed {
		t.Fatalf("the grader's regrade: %+v", prop)
	}
	var replaces []uuid.UUID
	var raw []byte
	if err := b.Pool.QueryRow(t.Context(), `SELECT payload->'replaces_grades' FROM action WHERE id = $1`, prop.ActionID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &replaces); err != nil || !sameSet(replaces, []uuid.UUID{w.yukiGrade, w.kenGrade}) {
		t.Fatalf("the proposal records the grades it replaces as %s: %v", raw, err)
	}
	adj := testkit.Result[tools.GradeAdjustOut](t, b.do(t, b.sato, "grade.adjust", m{"course_id": b.course, "grade_id": w.yukiGrade,
		"kind": "delta", "points": 3, "reason": "Presented it"}))
	res := testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": prop.ActionID, "decision": "approve"}))
	if res.Outcome != domain.StatusFailed || res.Error == nil || res.Error.Details["reason"] != tools.ReasonGradesChanged {
		t.Fatalf("approving the regrade after Yuki's adjustment: %+v", res)
	}
	yuki := testkit.Result[tools.GradeView](t, b.do(t, b.sato, "grade.get", m{"course_id": b.course, "grade_id": adj.GradeID}))
	if yuki.SupersededBy != nil || !yuki.Score.Equal(decimal.NewFromInt(83)) || yuki.Group == nil || yuki.Group.Adjustment == nil {
		t.Fatalf("Yuki's grade after the refused approval: %+v %+v", yuki, yuki.Group)
	}
}

// Once a group's grade is posted, a student added to its work is given the
// group's grade by regrading it, posted with the others'; a new group grade
// beside the posted one, whose drafts could never be posted, is refused, so
// that nothing holds up posting the assignment.
func TestAStudentAddedAfterPostingIsGivenTheGroupsGrade(t *testing.T) {
	w := buildTwoGroups(t)
	b := w.built
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "assignment_id": w.hw})
	actors, s := b.seatStudents(t, "Mio", "Hana")
	mio := s["Mio"]
	b.do(t, b.sato, "submission.set_members", m{"course_id": b.course, "submission_id": w.workA, "add": []uuid.UUID{mio}})

	b.refusedAs(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": w.workA, "score": 80},
		apperr.FailedPrecondition, tools.ReasonGroupGradePosted)
	re := testkit.Result[tools.GradeRegradeOut](t, b.do(t, b.sato, "grade.regrade", m{"course_id": b.course, "grade_id": w.yukiGrade,
		"score": 80, "feedback": "Strong analysis."}))
	scores := map[uuid.UUID]decimal.Decimal{}
	for _, mg := range re.MemberGrades {
		scores[mg.StudentMemberID] = mg.Score
	}
	if len(scores) != 3 || !scores[mio].Equal(decimal.NewFromInt(80)) || !scores[b.kenM].Equal(decimal.NewFromInt(75)) ||
		!scores[b.yukiM].Equal(decimal.NewFromInt(80)) {
		t.Fatalf("the regrade with Mio added: %+v", re.MemberGrades)
	}
	if n := b.Count(`SELECT count(*) FROM grade WHERE submission_id = $1 AND superseded_by IS NULL AND posted_at IS NULL`, w.workA); n != 0 {
		t.Fatalf("%d drafts left on Team A's work", n)
	}
	mine := testkit.Result[tools.GradeListOut](t, b.do(t, actors["Mio"], "grade.list", m{"course_id": b.course, "assignment_id": w.hw})).Grades
	if len(mine) != 1 || mine[0].State != "posted" || !mine[0].Score.Equal(decimal.NewFromInt(80)) || mine[0].Group == nil ||
		!mine[0].Group.Score.Equal(decimal.NewFromInt(80)) {
		t.Fatalf("Mio's grades: %+v", mine)
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'grade.posted' AND student_member_id = $1`, mio); n != 1 {
		t.Fatalf("%d grade.posted events told Mio, want 1", n)
	}

	// Posting the assignment is held up by nothing: a third team's grade is
	// posted with it.
	teamC := b.groupsIn(t, w.set, m{"name": "Team C"})[0]
	b.place(t, w.set, false, s["Hana"], teamC)
	workC := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, actors["Hana"], "submission.create",
		m{"course_id": b.course, "assignment_id": w.hw, "body": "Team C's report"})).SubmissionID
	b.do(t, actors["Hana"], "submission.submit", m{"course_id": b.course, "submission_id": workC})
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": workC, "score": 70})
	if posted := testkit.Result[tools.GradePostOut](t, b.do(t, b.sato, "grade.post", m{"course_id": b.course, "assignment_id": w.hw})); len(posted.Posted) != 1 {
		t.Fatalf("posting the assignment: %+v", posted)
	}
}

// A group grade entered while a member's draft is replaced or posted waits
// for that call, and is then refused, writing nothing: written, it would
// leave the member a draft beside the one written meanwhile, or beside
// their posted grade, where it could never be posted and would hold up
// posting the assignment, with nothing to clear it. Yuki's draft is
// adjusted while Tomo enters Team A's grade (grades_changed); then Team A's
// drafts are posted while he enters it again, the post held once it has
// posted them, on the lock of the first of its students' totals
// (group_grade_posted).
func TestAGroupGradeEnteredWhileAMembersDraftChangesIsRefused(t *testing.T) {
	w := buildTwoGroups(t)
	b := w.built
	tomo := b.person(t, "Tomo", "")
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": tomo, "preset": "ta"})
	live := func() (drafts, posted int) {
		t.Helper()
		if err := b.Pool.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE posted_at IS NULL), count(*) FILTER (WHERE posted_at IS NOT NULL)
			FROM grade WHERE submission_id = $1 AND superseded_by IS NULL`, w.workA).Scan(&drafts, &posted); err != nil {
			t.Fatal(err)
		}
		return drafts, posted
	}
	enter := func(key string) <-chan callResult {
		return b.inFlight(tomo, "grade.submit", m{"course_id": b.course, "submission_id": w.workA, "score": 90}, key)
	}

	// Yuki's draft adjusted as grade.adjust writes it: replaced by one from
	// the same group grade, held until Tomo's call waits for it.
	adjusted := uuid.New()
	release := heldBy(t, b, `WITH old AS (UPDATE grade SET superseded_by = $2 WHERE id = $1 RETURNING *)
		INSERT INTO grade (id, student_member_id, submission_id, origin, score, feedback, grader_member_id, created_by_action_id,
		                   group_grade_id, adjust_kind, adjust_points, adjust_reason, adjust_by_member_id)
		SELECT $2, student_member_id, submission_id, origin, 95, feedback, grader_member_id, created_by_action_id,
		       group_grade_id, 'replace', 95, 'Did the most', grader_member_id
		FROM old`, w.yukiGrade, adjusted)
	entering := enter("enter-while-adjusted")
	b.waitingFor(t, 1, entering)
	release()
	if out := settled(t, entering); out.Status != domain.StatusFailed || out.Error == nil ||
		out.Error.Details["reason"] != tools.ReasonGradesChanged {
		t.Fatalf("Tomo's group grade, entered while Yuki's draft was adjusted: %+v", out)
	}
	if drafts, posted := live(); drafts != 2 || posted != 0 {
		t.Fatalf("Team A's work after the refusal: %d live drafts, %d posted; want Yuki's adjusted one and Ken's", drafts, posted)
	}

	// A post takes its students in the order of their ids (see snapshot),
	// each under the lock on their totals (LockStudentTotals).
	first := b.yukiM
	if b.kenM.String() < first.String() {
		first = b.kenM
	}
	release = heldBy(t, b, `SELECT pg_advisory_xact_lock(hashtextextended('totals:' || $1::uuid::text || ':' || $2::uuid::text, 0))`, b.course, first)
	posting := b.inFlight(b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{adjusted, w.kenGrade}}, "post-team-a")
	b.waitingFor(t, 1, posting)
	entering = enter("enter-while-posted")
	b.waitingFor(t, 2, posting, entering)
	release()
	if out := settled(t, posting); out.Status != domain.StatusExecuted {
		t.Fatalf("the post of Team A's drafts: %+v", out)
	}
	if out := settled(t, entering); out.Status != domain.StatusFailed || out.Error == nil ||
		out.Error.Details["reason"] != tools.ReasonGroupGradePosted {
		t.Fatalf("Tomo's group grade, entered while Team A's drafts were posted: %+v", out)
	}
	if drafts, posted := live(); drafts != 0 || posted != 2 {
		t.Fatalf("Team A's work after the refusal: %d live drafts, %d posted; want its two posted alone", drafts, posted)
	}
	if n := b.Count(`SELECT count(*) FROM group_grade WHERE submission_id = $1`, w.workA); n != 1 {
		t.Fatalf("%d group grades on Team A's work, want the first alone", n)
	}
	// Nothing holds up posting the assignment: Team B's draft goes out.
	out := testkit.Result[tools.GradePostOut](t, b.do(t, b.sato, "grade.post", m{"course_id": b.course, "assignment_id": w.hw}))
	if !slices.Equal(out.Posted, w.gradesB) {
		t.Fatalf("posting the assignment: %v, want Team B's %v", out.Posted, w.gradesB)
	}
}

// A sign-up refused for work handed in says why, and names of that work only
// what the caller may read (§2.5, What each member sees): Hana, asking to
// join Team A, and Mio, placed in it after it handed in and asking to leave,
// are shown nothing of it, as submission.get refuses it them, and Aoi,
// asking to switch to it, only her own group's; Yuki, who handed it in, and
// Sato, asking for Mio, are shown it.
func TestASignUpRefusedForHandedInWorkNamesOnlyWhatTheCallerReads(t *testing.T) {
	w := buildTwoGroups(t)
	b := w.built
	actors, s := b.seatStudents(t, "Mio", "Hana")
	b.place(t, w.set, true, s["Mio"], w.teamA)
	b.do(t, b.sato, "group_set.update", m{"course_id": b.course, "set_id": w.set, "signup_open": true})
	b.refusedAs(t, actors["Mio"], "submission.get", m{"course_id": b.course, "submission_id": w.workA}, apperr.Forbidden, "student_out_of_scope")

	for _, c := range []struct {
		who   string
		actor uuid.UUID
		args  m
		why   string
		shown []uuid.UUID
	}{
		{"Hana joining Team A", actors["Hana"], m{"group_id": w.teamA}, tools.ReasonGroupHasWork, nil},
		{"Mio leaving Team A", actors["Mio"], m{}, tools.ReasonYourGroupHasWork, nil},
		{"Aoi switching to Team A", w.aoi, m{"group_id": w.teamA}, tools.ReasonYourGroupHasWork, []uuid.UUID{w.workB}},
		{"Yuki leaving Team A", b.yuki, m{}, tools.ReasonYourGroupHasWork, []uuid.UUID{w.workA}},
		{"Sato taking Mio out of Team A", b.sato, m{"student_member_id": s["Mio"]}, tools.ReasonYourGroupHasWork, []uuid.UUID{w.workA}},
	} {
		c.args["course_id"], c.args["set_id"] = b.course, w.set
		e := b.refusedAs(t, c.actor, "group.sign_up", c.args, apperr.FailedPrecondition, c.why)
		raw, err := json.Marshal(e.Details)
		if err != nil {
			t.Fatal(err)
		}
		var details struct {
			Work []tools.WorkDetail `json:"work"`
		}
		if err := json.Unmarshal(raw, &details); err != nil {
			t.Fatal(err)
		}
		var shown []uuid.UUID
		for _, wk := range details.Work {
			shown = append(shown, wk.SubmissionID)
		}
		if !slices.Equal(shown, c.shown) {
			t.Errorf("%s is refused naming %v, want %v: %s", c.who, shown, c.shown, raw)
		}
		if c.shown == nil && strings.Contains(string(raw), w.workA.String()) {
			t.Errorf("%s is shown Team A's work: %s", c.who, raw)
		}
	}
}

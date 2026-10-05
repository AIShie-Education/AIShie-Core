package tools_test

import (
	"encoding/json"
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
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "assignment_id": w.hw})
	// Team B's work, graded again by the grader: it is listed for HW3
	// alone, so Sato lists it for the project too.
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

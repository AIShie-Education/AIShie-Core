-- Lookups are always "in this course": an id from another course is not found.

-- name: GetSubmissionInCourse :one
SELECT id, assignment_id, course_id, student_member_id, group_id, attempt, state, submitted_at
FROM submission
WHERE id = $1 AND course_id = $2;

-- name: GetAssignmentInCourse :one
SELECT id, course_id, component_id, title, instructions_document_id, rubric_document_id,
       points_possible, due_at, published_at, group_set_id
FROM assignment
WHERE id = $1 AND course_id = $2;

-- name: GetComponentInCourse :one
SELECT id, course_id, parent_id, name, weight, drop_lowest, points_possible
FROM grade_component
WHERE id = $1 AND course_id = $2;

-- name: CountComponentChildren :one
SELECT count(*) FROM grade_component WHERE parent_id = $1;

-- name: CountComponentAssignments :one
SELECT count(*) FROM assignment WHERE component_id = $1;

-- name: GetRosterEntry :one
-- Roster facts about a member. This is not authorization: that a grade can
-- only be given to someone on the roster as a student is a rule about grades.
SELECT id, course_id, role, status
FROM course_member
WHERE id = $1 AND course_id = $2;

-- name: GetDocumentPublishedVersion :one
SELECT published_version_id FROM document WHERE id = $1;

-- name: GetDocumentVersionOwner :one
SELECT document_id FROM document_version WHERE id = $1;

-- name: InsertGrade :exec
INSERT INTO grade (id, student_member_id, submission_id, component_id, origin, score, feedback, breakdown,
                   rubric_version_id, grader_member_id, created_by_action_id, posted_at, posted_by_member_id, created_at,
                   override_score, override_reason, override_by_member_id, overridden_at,
                   group_grade_id, adjust_kind, adjust_points, adjust_reason, adjust_by_member_id, adjust_detail)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18,
        sqlc.narg(group_grade_id), sqlc.narg(adjust_kind), sqlc.narg(adjust_points), sqlc.narg(adjust_reason),
        sqlc.narg(adjust_by_member_id), sqlc.narg(adjust_detail));

-- name: SupersedeSubmissionDrafts :exec
-- A new draft replaces the student's earlier drafts for the same submission:
-- on a group's work, each member's by that member's new one.
UPDATE grade SET superseded_by = sqlc.arg(new_id)
WHERE submission_id = sqlc.arg(submission_id) AND student_member_id = sqlc.arg(student_member_id)
  AND posted_at IS NULL AND superseded_by IS NULL;

-- name: SupersedeComponentDrafts :exec
UPDATE grade SET superseded_by = sqlc.arg(new_id)
WHERE component_id = sqlc.arg(component_id) AND student_member_id = sqlc.arg(student_member_id)
  AND origin = 'entered' AND posted_at IS NULL AND superseded_by IS NULL;

-- name: SupersedeGrade :execrows
UPDATE grade SET superseded_by = sqlc.arg(new_id)
WHERE id = sqlc.arg(id) AND superseded_by IS NULL;

-- name: GetGradesInCourse :many
-- Grades by id, with the assignment each belongs to (null for a component
-- grade). A grade's course is its student's course.
SELECT g.id, g.student_member_id, g.submission_id, g.component_id, g.origin, g.score,
       g.posted_at, g.superseded_by, s.assignment_id, g.group_grade_id
FROM grade g
JOIN course_member m ON m.id = g.student_member_id
LEFT JOIN submission s ON s.id = g.submission_id
WHERE g.id = ANY(sqlc.arg(ids)::uuid[]) AND m.course_id = sqlc.arg(course_id)
ORDER BY g.id;

-- name: LockGradesInCourse :many
SELECT g.id
FROM grade g
JOIN course_member m ON m.id = g.student_member_id
WHERE g.id = ANY(sqlc.arg(ids)::uuid[]) AND m.course_id = sqlc.arg(course_id)
ORDER BY g.id
FOR UPDATE OF g;

-- name: ListDraftGradeIDsForAssignment :many
-- The live drafts waiting to be posted for one assignment.
SELECT g.id
FROM grade g
JOIN submission s ON s.id = g.submission_id
WHERE s.assignment_id = $1 AND s.course_id = $2
  AND g.origin = 'entered' AND g.posted_at IS NULL AND g.superseded_by IS NULL
ORDER BY g.id;

-- name: LiveSubmissionGradeExists :one
-- The student's live posted grade on the work: one per student, on a
-- group's work one per member.
SELECT EXISTS (
    SELECT 1 FROM grade
    WHERE submission_id = $1 AND student_member_id = $2 AND posted_at IS NOT NULL AND superseded_by IS NULL
);

-- name: LiveComponentGradeExists :one
SELECT EXISTS (
    SELECT 1 FROM grade
    WHERE component_id = $1 AND student_member_id = $2 AND posted_at IS NOT NULL AND superseded_by IS NULL
);

-- name: PostGrade :execrows
UPDATE grade SET posted_at = $2, posted_by_member_id = $3
WHERE id = $1 AND posted_at IS NULL AND superseded_by IS NULL;

-- name: GetLiveComputedGrade :one
-- A total as it stands, with what a person has given it besides the number
-- worked out: an override and a comment, which a total written again carries
-- on.
SELECT id, score, breakdown, feedback, override_score, override_reason, override_by_member_id, overridden_at
FROM grade
WHERE component_id = $1 AND student_member_id = $2 AND origin = 'computed'
  AND posted_at IS NOT NULL AND superseded_by IS NULL;

-- name: ListStudentsCountedAsZero :many
-- The students of the course with a total written down counting ungraded
-- work as zero.
SELECT DISTINCT g.student_member_id
FROM grade g
JOIN grade_component c ON c.id = g.component_id
WHERE c.course_id = $1 AND g.origin = 'computed' AND g.posted_at IS NOT NULL AND g.superseded_by IS NULL
  AND (g.breakdown->>'ungraded_as_zero')::boolean IS TRUE
ORDER BY 1;

-- name: StudentCountedAsZero :one
SELECT EXISTS (
    SELECT 1 FROM grade
    WHERE student_member_id = $1 AND origin = 'computed' AND posted_at IS NOT NULL AND superseded_by IS NULL
      AND (breakdown->>'ungraded_as_zero')::boolean IS TRUE
);

-- name: ListLiveTotalComponents :many
-- Where the student has a total written down.
SELECT component_id
FROM grade
WHERE student_member_id = $1 AND origin = 'computed' AND posted_at IS NOT NULL AND superseded_by IS NULL
ORDER BY component_id;

-- name: ListStudentsWithLiveTotals :many
-- Every student of the course who has a total written down.
SELECT DISTINCT g.student_member_id
FROM grade g
JOIN grade_component c ON c.id = g.component_id
WHERE c.course_id = $1 AND g.origin = 'computed' AND g.posted_at IS NOT NULL AND g.superseded_by IS NULL
ORDER BY 1;

-- name: ListStudentsGradedOnAssignment :many
-- The students with a live grade entered on the assignment, a draft or posted.
SELECT DISTINCT g.student_member_id
FROM grade g
JOIN submission s ON s.id = g.submission_id
WHERE s.assignment_id = $1 AND g.origin = 'entered' AND g.superseded_by IS NULL
ORDER BY 1;

-- name: ListStudentsGradedBeneath :many
-- The students with a live grade entered on the component, on one beneath it,
-- or on a submission to an assignment beneath it.
WITH RECURSIVE sub(component_id) AS (
    SELECT gc.id FROM grade_component gc WHERE gc.id = sqlc.arg(component_id)
    UNION ALL
    SELECT c.id FROM grade_component c JOIN sub ON c.parent_id = sub.component_id
)
SELECT DISTINCT g.student_member_id
FROM grade g
WHERE g.origin = 'entered' AND g.superseded_by IS NULL
  AND (g.component_id IN (SELECT sub.component_id FROM sub)
       OR g.submission_id IN (SELECT s.id FROM submission s JOIN assignment a ON a.id = s.assignment_id
                              WHERE a.component_id IN (SELECT sub.component_id FROM sub)))
ORDER BY 1;

-- name: LockLiveEnteredGradesOfAssignment :many
-- Every live grade entered on the assignment's submissions, draft or posted,
-- held in id order, as grade.post holds the drafts it posts.
SELECT g.id, g.student_member_id, g.submission_id, g.component_id, g.score, g.feedback, g.breakdown,
       g.rubric_version_id, g.posted_at, g.group_grade_id, g.adjust_kind, g.adjust_points, g.adjust_reason,
       g.adjust_by_member_id, g.adjust_detail
FROM grade g
JOIN submission s ON s.id = g.submission_id
WHERE s.assignment_id = $1 AND g.origin = 'entered' AND g.superseded_by IS NULL
ORDER BY g.id
FOR UPDATE OF g;

-- name: ListLiveEnteredGradeScoresOfAssignment :many
-- LockLiveEnteredGradesOfAssignment's grades, by their scores, without the
-- lock: what a change of the assignment's points is held to before it is
-- carried out, proposed or approved.
SELECT g.id, g.score
FROM grade g
JOIN submission s ON s.id = g.submission_id
WHERE s.assignment_id = $1 AND g.origin = 'entered' AND g.superseded_by IS NULL
ORDER BY g.id;

-- name: LockLiveEnteredGradesOfComponent :many
-- Every live grade entered directly on the component, draft or posted.
SELECT g.id, g.student_member_id, g.submission_id, g.component_id, g.score, g.feedback, g.breakdown,
       g.rubric_version_id, g.posted_at, g.group_grade_id, g.adjust_kind, g.adjust_points, g.adjust_reason,
       g.adjust_by_member_id, g.adjust_detail
FROM grade g
WHERE g.component_id = $1 AND g.origin = 'entered' AND g.superseded_by IS NULL
ORDER BY g.id
FOR UPDATE;

-- name: ListLiveEnteredGradeScoresOfComponent :many
-- LockLiveEnteredGradesOfComponent's grades, by their scores, without the
-- lock, as ListLiveEnteredGradeScoresOfAssignment.
SELECT g.id, g.score
FROM grade g
WHERE g.component_id = $1 AND g.origin = 'entered' AND g.superseded_by IS NULL
ORDER BY g.id;

-- name: MoveFeedbackFiles :exec
-- A grade's feedback files go with it when it is written again without
-- being graded again: a total worked out anew, a score rescaled.
UPDATE document SET grade_id = sqlc.arg(new_grade_id) WHERE grade_id = sqlc.arg(old_grade_id) AND kind = 'feedback';

-- Serialising what races -------------------------------------------------------

-- name: ShareAssignmentForGrading :one
-- The assignment a submission's grade is out of, read again and held still
-- until the grade is in. FOR SHARE waits for an assignment.update under way,
-- and holds the next one off until the grade is there for its check to find.
-- Graders of the same assignment do not wait for one another.
SELECT id, course_id, component_id, title, instructions_document_id, rubric_document_id,
       points_possible, due_at, published_at, group_set_id
FROM assignment
WHERE id = $1 AND course_id = $2
FOR SHARE;

-- name: LockSubmissionForGrading :one
-- A row lock, not an UPDATE: the freeze trigger does not fire. Two drafts for
-- one submission entered at once would otherwise both be live, and late work
-- taking a 'missing' placeholder over takes the same lock. The state is read
-- under it, so it is the state the grade is written against.
SELECT state FROM submission WHERE id = $1 FOR UPDATE;

-- name: LockComponentGradeTarget :exec
SELECT pg_advisory_xact_lock(hashtextextended(
    'grade-target:' || (sqlc.arg(component_id)::uuid)::text || ':' || (sqlc.arg(student_member_id)::uuid)::text, 0));

-- name: LockStudentTotals :exec
-- One writer of a student's rolled-up totals at a time.
SELECT pg_advisory_xact_lock(hashtextextended(
    'totals:' || (sqlc.arg(course_id)::uuid)::text || ':' || (sqlc.arg(student_member_id)::uuid)::text, 0));

-- name: NewestSubmissionDraftAt :one
SELECT created_at FROM grade
WHERE submission_id = $1 AND origin = 'entered' AND posted_at IS NULL AND superseded_by IS NULL
ORDER BY created_at DESC LIMIT 1;

-- name: NewestComponentDraftAt :one
SELECT created_at FROM grade
WHERE component_id = $1 AND student_member_id = $2 AND origin = 'entered' AND posted_at IS NULL AND superseded_by IS NULL
ORDER BY created_at DESC LIMIT 1;

-- What gradecalc needs -------------------------------------------------------

-- name: ListComponents :many
SELECT id, parent_id, name, weight, drop_lowest, points_possible, sort_order
FROM grade_component
WHERE course_id = $1
ORDER BY sort_order, id;

-- name: ListGradedAssignments :many
-- Assignments that count toward the grade. An unpublished one cannot have a
-- submission, so it cannot have a grade; it is left out rather than shown to
-- every student as something they scored nothing on.
SELECT id, component_id, points_possible
FROM assignment
WHERE course_id = $1 AND component_id IS NOT NULL AND published_at IS NOT NULL
ORDER BY id;

-- name: ListLiveAssignmentScores :many
-- Per assignment, the student's live posted grade on the highest attempt that
-- has one.
SELECT DISTINCT ON (s.assignment_id) s.assignment_id, g.score
FROM grade g
JOIN submission s ON s.id = g.submission_id
WHERE s.course_id = $1 AND g.student_member_id = $2
  AND g.origin = 'entered' AND g.posted_at IS NOT NULL AND g.superseded_by IS NULL
ORDER BY s.assignment_id, s.attempt DESC;

-- name: ListLiveTotalOverrides :many
-- The student's totals a person has overridden, and with what, out of 100.
SELECT component_id, override_score
FROM grade
WHERE student_member_id = $1 AND origin = 'computed' AND override_score IS NOT NULL
  AND posted_at IS NOT NULL AND superseded_by IS NULL;

-- name: ListLiveComponentScores :many
SELECT component_id, score
FROM grade
WHERE student_member_id = $1 AND component_id IS NOT NULL
  AND origin = 'entered' AND posted_at IS NOT NULL AND superseded_by IS NULL;

-- name: SubmissionHasGrades :one
-- A grade entered, or proposed and not yet decided: either way, one is on its
-- way for exactly this work. A group grade is entered with its members'.
SELECT EXISTS (
    SELECT 1 FROM grade g WHERE g.submission_id = sqlc.narg(submission_id)::uuid
    UNION ALL
    SELECT 1 FROM group_grade gg WHERE gg.submission_id = sqlc.narg(submission_id)::uuid
    UNION ALL
    SELECT 1 FROM action x
    WHERE x.target_type = 'submission' AND x.target_id = sqlc.narg(submission_id)::uuid
      AND x.action_type = 'grade.submit' AND x.status = 'proposed'
);

-- name: ComponentHasLivePostedGrades :one
-- Any origin: an entered grade, or a total written down when it was a parent.
SELECT EXISTS (SELECT 1 FROM grade WHERE component_id = $1 AND posted_at IS NOT NULL AND superseded_by IS NULL);

-- Group grades ---------------------------------------------------------------

-- name: InsertGroupGrade :exec
-- What a group's work was given, as a group: kept as written
-- (group_grade_kept), its course the submission's.
INSERT INTO group_grade (id, course_id, submission_id, score, out_of, allow_extra, feedback, breakdown, rubric_version_id,
                         grader_member_id, created_by_action_id, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12);

-- name: GetGroupGrade :one
SELECT * FROM group_grade WHERE id = $1 AND course_id = $2;

-- name: ListLiveMemberGrades :many
-- Every live grade on the work, a draft or posted, the newest first: what a
-- member's next grade from a group grade carries its adjustment on from.
SELECT g.id, g.student_member_id, g.score, g.posted_at, g.group_grade_id, g.adjust_kind, g.adjust_points,
       g.adjust_reason, g.adjust_by_member_id, g.created_at
FROM grade g
WHERE g.submission_id = $1 AND g.origin = 'entered' AND g.superseded_by IS NULL
ORDER BY g.student_member_id, g.posted_at NULLS FIRST, g.created_at DESC, g.id DESC;

-- name: ListLiveGradesFromGroupGrade :many
-- Every live grade given from a group grade, a draft or posted, held in id
-- order: what regrading the group's grade writes again.
SELECT g.id, g.student_member_id, g.score, g.posted_at, g.adjust_kind, g.adjust_points, g.adjust_reason,
       g.adjust_by_member_id
FROM grade g
WHERE g.group_grade_id = $1 AND g.superseded_by IS NULL
ORDER BY g.id
FOR UPDATE;

-- name: ListGroupGradeDocuments :many
-- A group grade's feedback files, in their order.
SELECT id, title FROM document WHERE group_grade_id = $1 AND kind = 'feedback' AND status = 'active'
ORDER BY sort_order, id;

-- name: MoveGroupGradeFeedbackFiles :exec
-- A group grade's feedback files go with it when it is written again without
-- being graded again: a score rescaled.
UPDATE document SET group_grade_id = sqlc.arg(new_group_grade_id)
WHERE group_grade_id = sqlc.arg(old_group_grade_id) AND kind = 'feedback';

-- name: LivePostedGradeFromGroupGradeFor :one
-- Whether one of the students has a live posted grade given from the group
-- grade: what lets a member read its feedback files.
SELECT EXISTS (
    SELECT 1 FROM grade
    WHERE group_grade_id = sqlc.arg(group_grade_id) AND student_member_id = ANY(sqlc.arg(student_ids)::uuid[])
      AND posted_at IS NOT NULL AND superseded_by IS NULL
);

-- name: StudentsPostedFromGroupGrade :many
-- The students with a live posted grade given from the group grade.
SELECT student_member_id FROM grade
WHERE group_grade_id = $1 AND posted_at IS NOT NULL AND superseded_by IS NULL
ORDER BY 1;

-- name: GroupGradeHasPostedGrade :one
-- Whether any grade given from the group grade has been posted: its
-- feedback is then a release, and changing it needs grade_post.
SELECT EXISTS (SELECT 1 FROM grade WHERE group_grade_id = $1 AND posted_at IS NOT NULL);

-- name: GroupGradeIsLive :one
-- Whether a grade given from the group grade is live, a draft or posted:
-- one regraded since is the history of the one that replaced it.
SELECT EXISTS (SELECT 1 FROM grade WHERE group_grade_id = $1 AND superseded_by IS NULL);

-- name: ListLiveGroupGradesOfAssignment :many
-- The group grades of the assignment's work that a live grade is given
-- from, a draft or posted: what a change of its points carries with the
-- members' grades.
SELECT gg.*
FROM group_grade gg
JOIN submission s ON s.id = gg.submission_id
WHERE s.assignment_id = $1
  AND EXISTS (SELECT 1 FROM grade g WHERE g.group_grade_id = gg.id AND g.superseded_by IS NULL)
ORDER BY gg.id;

-- Lookups are always "in this course": an id from another course is not found.

-- name: GetSubmissionInCourse :one
SELECT id, assignment_id, course_id, student_member_id, attempt, state, submitted_at
FROM submission
WHERE id = $1 AND course_id = $2;

-- name: GetAssignmentInCourse :one
SELECT id, course_id, component_id, title, instructions_document_id, rubric_document_id,
       points_possible, due_at, published_at
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
                   rubric_version_id, grader_member_id, created_by_action_id, posted_at, posted_by_member_id, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14);

-- name: SupersedeSubmissionDrafts :exec
-- A new draft replaces earlier drafts for the same submission.
UPDATE grade SET superseded_by = sqlc.arg(new_id)
WHERE submission_id = sqlc.arg(submission_id) AND posted_at IS NULL AND superseded_by IS NULL;

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
       g.posted_at, g.superseded_by, s.assignment_id
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
SELECT EXISTS (
    SELECT 1 FROM grade
    WHERE submission_id = $1 AND posted_at IS NOT NULL AND superseded_by IS NULL
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
SELECT id, score
FROM grade
WHERE component_id = $1 AND student_member_id = $2 AND origin = 'computed'
  AND posted_at IS NOT NULL AND superseded_by IS NULL;

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

-- name: ListLiveComponentScores :many
SELECT component_id, score
FROM grade
WHERE student_member_id = $1 AND component_id IS NOT NULL
  AND origin = 'entered' AND posted_at IS NOT NULL AND superseded_by IS NULL;

-- name: SubmissionHasGrades :one
SELECT EXISTS (SELECT 1 FROM grade WHERE submission_id = $1);

-- name: ComponentHasLivePostedGrades :one
-- Any origin: an entered grade, or a total written down when it was a parent.
SELECT EXISTS (SELECT 1 FROM grade WHERE component_id = $1 AND posted_at IS NOT NULL AND superseded_by IS NULL);

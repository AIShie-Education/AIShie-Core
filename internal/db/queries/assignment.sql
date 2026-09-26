-- name: InsertAssignment :exec
INSERT INTO assignment (id, course_id, component_id, title, instructions_document_id, rubric_document_id,
                        points_possible, due_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: GetAssignmentInCourseForUpdate :one
-- GetAssignmentInCourse, locked for the rest of the transaction:
-- assignment.update and assignment.publish read the row, check it and write
-- it back, and two of them at once must take turns. NO KEY UPDATE is the lock
-- the UPDATE takes anyway, taken before the read instead of after it; it does
-- not hold up a submission being created for the assignment. It is taken
-- first: assignment.update holds it and then waits for the component-tree
-- lock, and nothing takes those two the other way round. Graders of the
-- assignment lock the row FOR SHARE (ShareAssignmentForGrading), so they queue
-- behind an update, including while the update waits for the tree lock.
SELECT id, course_id, component_id, title, instructions_document_id, rubric_document_id,
       points_possible, due_at, published_at
FROM assignment
WHERE id = $1 AND course_id = $2
FOR NO KEY UPDATE;

-- name: UpdateAssignment :exec
UPDATE assignment
SET component_id = $2, title = $3, instructions_document_id = $4, rubric_document_id = $5,
    points_possible = $6, due_at = $7
WHERE id = $1;

-- name: PublishAssignment :execrows
UPDATE assignment SET published_at = $2 WHERE id = $1 AND published_at IS NULL;

-- name: LockAssignmentForUnpublish :one
-- FOR UPDATE, not the NO KEY UPDATE of an ordinary update: it conflicts with
-- the KEY SHARE that inserting a submission takes, and that
-- GetAssignmentForSubmission takes before it checks that the assignment is
-- published. So a submission being made waits for an unpublish and then sees
-- the assignment unpublished, or is made first and is seen by it.
SELECT id, published_at
FROM assignment
WHERE id = $1 AND course_id = $2
FOR UPDATE;

-- name: AssignmentHasSubmissions :one
-- Any row at all: a draft, a hand-in, a 'missing' placeholder.
SELECT EXISTS (SELECT 1 FROM submission WHERE assignment_id = $1);

-- name: UnpublishAssignment :execrows
UPDATE assignment SET published_at = NULL WHERE id = $1 AND published_at IS NOT NULL;

-- name: GetAssignmentForSubmission :one
-- GetAssignmentInCourse for a tool about to add a submission to it. KEY SHARE
-- holds up nothing but LockAssignmentForUnpublish, which it waits for; then
-- the assignment is read as that left it.
SELECT id, course_id, component_id, title, instructions_document_id, rubric_document_id,
       points_possible, due_at, published_at
FROM assignment
WHERE id = $1 AND course_id = $2
FOR KEY SHARE;

-- name: GetDocumentInCourse :one
SELECT id, course_id, kind, title, status, published_version_id
FROM document WHERE id = $1 AND course_id = $2;

-- name: AssignmentHasLiveGrades :one
-- Entered and not replaced: a draft waiting to be posted counts, since what it
-- was entered against would change under it just the same.
SELECT EXISTS (
    SELECT 1 FROM grade g JOIN submission s ON s.id = g.submission_id
    WHERE s.assignment_id = $1 AND g.origin = 'entered' AND g.superseded_by IS NULL
);

-- name: ListAssignments :many
-- Scope is applied here, not afterwards. A member who may not write
-- assignments sees only published ones.
SELECT id, course_id, component_id, title, instructions_document_id, rubric_document_id,
       points_possible, due_at, published_at, created_at
FROM assignment a
WHERE a.course_id = $1 AND a.id > sqlc.arg(after)
  AND (sqlc.arg(include_unpublished)::bool OR a.published_at IS NOT NULL)
  AND (sqlc.arg(assignment_all)::bool OR EXISTS (
        SELECT 1 FROM member_assignment_scope y WHERE y.member_id = sqlc.arg(member_id) AND y.assignment_id = a.id))
ORDER BY a.id
LIMIT sqlc.arg(max_rows);

-- name: InsertSubmission :exec
INSERT INTO submission (id, assignment_id, course_id, student_member_id, attempt, body, state, created_at)
VALUES ($1, $2, $3, $4, $5, $6, 'draft', $7);

-- name: GetSubmissionFull :one
SELECT id, assignment_id, course_id, student_member_id, attempt, body, instructions_version_id,
       state, submitted_at, created_at
FROM submission WHERE id = $1 AND course_id = $2;

-- name: LockSubmissionsOf :many
-- All of one student's attempts at one assignment, locked, newest first.
SELECT id, attempt, state FROM submission
WHERE assignment_id = $1 AND student_member_id = $2
ORDER BY attempt DESC
FOR UPDATE;

-- name: ReopenMissingSubmission :exec
-- A 'missing' row is a placeholder written when the due date passed with
-- nothing handed in. Late work takes it over rather than sitting beside it.
UPDATE submission SET state = 'draft', body = $2 WHERE id = $1 AND state = 'missing';

-- name: UpdateSubmissionDraft :execrows
UPDATE submission SET body = $2 WHERE id = $1 AND state = 'draft';

-- name: SubmitSubmission :execrows
UPDATE submission
SET state = $2, submitted_at = $3, instructions_version_id = $4
WHERE id = $1 AND state = 'draft';

-- name: SetSubmissionLateness :execrows
UPDATE submission SET state = $2 WHERE id = $1 AND state IN ('submitted', 'late') AND state <> $2;

-- name: CountSubmissionDocuments :one
SELECT count(*) FROM document WHERE submission_id = $1 AND status = 'active';

-- name: ListSubmissions :many
SELECT s.id, s.assignment_id, s.course_id, s.student_member_id, s.attempt, s.state, s.submitted_at, s.created_at
FROM submission s
WHERE s.course_id = $1 AND s.id > sqlc.arg(after)
  AND (sqlc.narg(assignment_id)::uuid IS NULL OR s.assignment_id = sqlc.narg(assignment_id))
  AND (sqlc.narg(student_member_id)::uuid IS NULL OR s.student_member_id = sqlc.narg(student_member_id))
  AND (sqlc.arg(student_all)::bool OR EXISTS (
        SELECT 1 FROM member_student_scope x WHERE x.member_id = sqlc.arg(member_id) AND x.student_member_id = s.student_member_id))
  AND (sqlc.arg(assignment_all)::bool OR EXISTS (
        SELECT 1 FROM member_assignment_scope y WHERE y.member_id = sqlc.arg(member_id) AND y.assignment_id = s.assignment_id))
ORDER BY s.id
LIMIT sqlc.arg(max_rows);

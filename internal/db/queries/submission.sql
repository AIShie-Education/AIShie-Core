-- name: InsertSubmission :exec
INSERT INTO submission (id, assignment_id, course_id, student_member_id, attempt, body, state, created_at)
VALUES ($1, $2, $3, $4, $5, $6, 'draft', $7);

-- name: GetSubmissionFull :one
SELECT id, assignment_id, course_id, student_member_id, attempt, body, instructions_version_id,
       state, submitted_at, created_at
FROM submission WHERE id = $1 AND course_id = $2;

-- name: GetSubmissionFullForUpdate :one
-- GetSubmissionFull, locked until the transaction ends, so that what
-- submission.submit checks is what it hands in: an edit to the draft, or a
-- file added to it or archived from it, meanwhile waits, and then finds it
-- handed in.
SELECT id, assignment_id, course_id, student_member_id, attempt, body, instructions_version_id,
       state, submitted_at, created_at
FROM submission WHERE id = $1 AND course_id = $2
FOR UPDATE;

-- name: LockSubmissionsOf :many
-- All of one student's attempts at one assignment, locked, newest first.
SELECT id, attempt, state FROM submission
WHERE assignment_id = $1 AND student_member_id = $2
ORDER BY attempt DESC
FOR UPDATE;

-- name: ListSubmissionsOf :many
-- LockSubmissionsOf, not locked: what a tool's Validate reads of a
-- student's attempts before a proposal is queued, which the tool reads again
-- under the lock when it is carried out.
SELECT id, attempt, state FROM submission
WHERE assignment_id = $1 AND student_member_id = $2
ORDER BY attempt DESC;

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
  -- A delegate's principal's scope, the same way; "all" for any other seat.
  AND (sqlc.arg(principal_student_all)::bool OR EXISTS (
        SELECT 1 FROM member_student_scope px WHERE px.member_id = sqlc.arg(principal_id) AND px.student_member_id = s.student_member_id))
  AND (sqlc.arg(principal_assignment_all)::bool OR EXISTS (
        SELECT 1 FROM member_assignment_scope py WHERE py.member_id = sqlc.arg(principal_id) AND py.assignment_id = s.assignment_id))
ORDER BY s.id
LIMIT sqlc.arg(max_rows);

-- name: ListAssignmentRoster :many
-- Every current student of the course whom the caller's student scope
-- reaches, with their latest attempt at one assignment, if any: the students
-- who have not started are rows too, with no submission. The caller's
-- assignment scope is checked on the target, before this runs. A delegate
-- reaches only the students its principal reaches too.
SELECT m.id AS student_member_id, a.display_name, m.status AS member_status,
       s.id AS submission_id, s.attempt, s.state, s.submitted_at
FROM course_member m
JOIN actor a ON a.id = m.actor_id
LEFT JOIN submission s ON s.assignment_id = sqlc.arg(assignment_id) AND s.student_member_id = m.id
    AND s.attempt = (SELECT max(z.attempt) FROM submission z
                     WHERE z.assignment_id = sqlc.arg(assignment_id) AND z.student_member_id = m.id)
WHERE m.course_id = sqlc.arg(course_id) AND m.role = 'student' AND m.status <> 'removed'
  AND m.id > sqlc.arg(after)
  AND (sqlc.arg(student_all)::bool OR EXISTS (
        SELECT 1 FROM member_student_scope y WHERE y.member_id = sqlc.arg(member_id) AND y.student_member_id = m.id))
  AND (sqlc.arg(principal_student_all)::bool OR EXISTS (
        SELECT 1 FROM member_student_scope py WHERE py.member_id = sqlc.arg(principal_id) AND py.student_member_id = m.id))
ORDER BY m.id
LIMIT sqlc.arg(max_rows);

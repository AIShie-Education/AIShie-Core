-- What the background sweeps look for. Each returns a small batch; the sweep
-- runs again on the next tick. None of these is what makes the system
-- correct — authorize() ignores an expired member on every call and approval
-- re-checks a proposal's age inline — they make the state visible and keep
-- the queues clean.

-- name: ListStaleProposals :many
SELECT id, course_id, created_at
FROM action
WHERE status = 'proposed' AND created_at < sqlc.arg(created_before)
ORDER BY created_at
LIMIT sqlc.arg(max_rows);

-- name: ListExpiredMembers :many
SELECT id, course_id, expires_at
FROM course_member
WHERE status <> 'removed' AND expires_at IS NOT NULL AND expires_at <= sqlc.arg(now)
ORDER BY expires_at
LIMIT sqlc.arg(max_rows);

-- name: ListAssignmentsNewlyPastDue :many
-- Published assignments of open courses whose due date has passed and which
-- have not been swept for that due date yet. The sweep's own action row is
-- the marker: its idempotency key names the assignment and the due date, so
-- moving a due date later makes the assignment due for a sweep again.
SELECT a.id, a.course_id, a.due_at
FROM assignment a
JOIN course c ON c.id = a.course_id
WHERE a.published_at IS NOT NULL AND a.due_at IS NOT NULL AND a.due_at <= sqlc.arg(now)
  AND c.status = 'active'
  AND NOT EXISTS (
        SELECT 1 FROM action x
        WHERE x.actor_id = sqlc.arg(system_actor_id)
          -- floor, as Go's time.Unix() does; a plain ::bigint cast rounds, and
          -- a due date with a fractional second would then be swept every tick.
          AND x.idempotency_key = 'job:submission.mark_missing:' || a.id::text || ':' || floor(extract(epoch FROM a.due_at))::bigint::text)
ORDER BY a.due_at
LIMIT sqlc.arg(max_rows);

-- name: ListStudentsWithoutSubmission :many
-- Current students of the course with no submission row at all for the
-- assignment: not a draft, not a hand-in, not an earlier 'missing'.
SELECT m.id
FROM course_member m
WHERE m.course_id = $1 AND m.role = 'student' AND m.status = 'active'
  AND NOT EXISTS (SELECT 1 FROM submission s WHERE s.assignment_id = $2 AND s.student_member_id = m.id)
ORDER BY m.id;

-- name: InsertMissingSubmission :execrows
INSERT INTO submission (id, assignment_id, course_id, student_member_id, attempt, state, created_at)
VALUES ($1, $2, $3, $4, 1, 'missing', $5)
ON CONFLICT (assignment_id, student_member_id, attempt) DO NOTHING;

-- name: GetActionForUpdate :one
SELECT * FROM action WHERE id = $1 FOR UPDATE;

-- name: GetMemberForSweep :one
SELECT id, course_id, status, expires_at FROM course_member WHERE id = $1 FOR UPDATE;

-- name: DeleteStaleSessions :execrows
-- A session is a credential with a short life. Long after it has expired it
-- says nothing the action log does not, so it is the one kind of row that is
-- actually deleted.
DELETE FROM credential WHERE kind = 'session' AND expires_at < sqlc.arg(expired_before);

-- name: TryJobLock :one
SELECT pg_try_advisory_lock(sqlc.arg(key)::bigint);

-- name: ReleaseJobLock :one
SELECT pg_advisory_unlock(sqlc.arg(key)::bigint);

-- name: GetActionCourse :one
SELECT course_id FROM action WHERE id = $1;

-- name: ListOrphanUploads :many
-- Which of these uploads, each given with the course its key names, are
-- this deployment's and attached to nothing? The course must be one this
-- database has. document.upload_url issues keys only under courses that
-- exist, and a course is never deleted, so a key under any other course was
-- written by another deployment keeping its files in the same place: it is
-- not ours to remove, however old it is and whatever points at it there.
-- What is left comes back in the order it was given. The orphan sweep puts
-- a page of listed files at a time to it, and asks again about each one it
-- removes, under the lock attaching takes.
SELECT k.storage_key::text AS storage_key, o.course_id::uuid AS course_id
FROM unnest(sqlc.arg(storage_keys)::text[]) WITH ORDINALITY AS k(storage_key, n)
JOIN unnest(sqlc.arg(course_ids)::uuid[]) WITH ORDINALITY AS o(course_id, n) ON o.n = k.n
WHERE EXISTS (SELECT 1 FROM course c WHERE c.id = o.course_id)
  AND NOT EXISTS (SELECT 1 FROM document_version v WHERE v.storage_key = k.storage_key)
ORDER BY k.n;

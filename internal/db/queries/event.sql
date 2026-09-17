-- name: LockEventStream :exec
-- Held until the transaction ends. See events.Flush for why.
SELECT pg_advisory_xact_lock(sqlc.arg(namespace)::int4, sqlc.arg(stream)::int4);

-- name: InsertEvent :exec
INSERT INTO event (type, course_id, action_id, subject_type, subject_id, student_member_id, assignment_id, payload)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: ListEventsForAction :many
SELECT seq, type, course_id, action_id, subject_type, subject_id, student_member_id, assignment_id, payload, occurred_at
FROM event
WHERE action_id = $1
ORDER BY seq;

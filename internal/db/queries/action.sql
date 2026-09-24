-- name: GetActionByKey :one
SELECT * FROM action WHERE actor_id = $1 AND idempotency_key = $2;

-- name: LockIdempotencyKey :exec
-- Calls with one key take turns from the start, before anything else is
-- locked. A retry of a call still in flight waits here holding nothing, and
-- then finds the first call's row; without this it would wait for that row
-- at InsertAction, holding its caller's seat, which the first call may yet
-- need to lock FOR UPDATE.
SELECT pg_advisory_xact_lock(hashtextextended('action-key:' || sqlc.arg(actor_id)::uuid::text || ':' || sqlc.arg(idempotency_key)::text, 0));

-- name: InsertAction :execrows
-- Zero rows means another call with the same key got there first; the caller
-- then reads that row and replays it. ON CONFLICT waits for an in-flight
-- transaction holding the key, so two simultaneous calls cannot both act.
INSERT INTO action (id, actor_id, course_id, member_id, action_type, target_type, target_id,
                    payload, payload_hash, idempotency_key, authz_result, status, result, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT (actor_id, idempotency_key) DO NOTHING;

-- name: MarkActionExecuted :exec
UPDATE action
SET status = 'executed', executed_at = $2, review_state = $3, result = $4
WHERE id = $1;

-- name: MarkActionFailed :exec
UPDATE action
SET status = 'failed', result = $2
WHERE id = $1;

-- name: GetActionInCourse :one
SELECT * FROM action WHERE id = $1 AND course_id = $2;

-- name: GetActionInCourseForUpdate :one
SELECT * FROM action WHERE id = $1 AND course_id = $2 FOR UPDATE;

-- name: FinishProposal :execrows
-- Moves a proposal to its end state. The status guard makes a lost race
-- between two deciders, or a decider and the expiry sweep, a no-op.
UPDATE action
SET status = $2, decided_by_member_id = $3, decided_at = $4, executed_at = $5, result = $6
WHERE id = $1 AND status = 'proposed';

-- name: SetActionReview :execrows
UPDATE action
SET review_state = $2, reviewed_by_member_id = $3, reviewed_at = $4
WHERE id = $1 AND review_state IN ('pending', 'escalated');

-- name: ListProposedActions :many
SELECT * FROM action
WHERE course_id = $1 AND status = 'proposed' AND id > sqlc.arg(after)
ORDER BY id
LIMIT sqlc.arg(max_rows);

-- name: ListPendingReviewActions :many
SELECT * FROM action
WHERE course_id = $1 AND review_state IN ('pending', 'escalated') AND id > sqlc.arg(after)
ORDER BY id
LIMIT sqlc.arg(max_rows);

-- name: ListActionsByMember :many
SELECT * FROM action
WHERE course_id = $1 AND member_id = $2 AND id > sqlc.arg(after)
ORDER BY id
LIMIT sqlc.arg(max_rows);

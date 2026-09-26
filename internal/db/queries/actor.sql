-- name: GetActor :one
-- The whole row, for showing an actor. Authorization uses GetActorForAuthz,
-- which leaves kind out on purpose.
SELECT id, kind, display_name, email, status, platform_role, created_by_actor_id, created_at,
       owner_actor_id, suspended_by_actor_id
FROM actor
WHERE id = $1;

-- name: GetActorByEmail :one
SELECT id, status FROM actor WHERE lower(email) = lower($1);

-- name: InsertActor :exec
INSERT INTO actor (id, kind, display_name, email, status, platform_role, created_by_actor_id, created_at, owner_actor_id)
VALUES ($1, $2, $3, $4, 'active', $5, $6, $7, sqlc.narg(owner_actor_id));

-- name: CountRootActors :one
SELECT count(*) FROM actor WHERE platform_role = 'root';

-- name: GetSystemActor :one
SELECT id FROM actor WHERE kind = 'system' ORDER BY created_at LIMIT 1;

-- name: ListMembershipsForActor :many
SELECT m.id AS member_id, m.course_id, c.code, c.section, c.title, c.status AS course_status,
       m.role, m.status, m.expires_at, m.student_scope, m.assignment_scope, m.principal_member_id
FROM course_member m
JOIN course c ON c.id = m.course_id
WHERE m.actor_id = $1 AND m.status <> 'removed'
ORDER BY c.code, c.section, m.id;

-- name: LockOwnerForAgents :exec
-- An owner's agents are counted against the limit one creation, or one
-- reactivation, at a time. NO KEY UPDATE, not UPDATE: the call's own action
-- row, and every other action row naming the owner, holds KEY SHARE on this
-- row through its foreign key, and FOR UPDATE would wait for all of them,
-- and deadlock with another call of the owner's doing the same.
SELECT 1 FROM actor WHERE id = $1 FOR NO KEY UPDATE;

-- name: CountActiveAgentsOf :one
-- The agents a person owns that are not suspended: what the limit counts.
SELECT count(*) FROM actor WHERE owner_actor_id = $1 AND status <> 'suspended';

-- name: ListAgentsOf :many
-- A person's agents, oldest first, with what their owner needs to see at a
-- glance: when one last used a token that still works, how many seats it
-- holds that count now, and how many requests to seat it wait for a
-- decision.
SELECT a.id, a.display_name, a.status, a.suspended_by_actor_id, a.created_at, seen.last_used_at AS last_seen_at,
       (SELECT count(*) FROM course_member m
         WHERE m.actor_id = a.id AND m.status = 'active'
           AND (m.expires_at IS NULL OR m.expires_at > sqlc.arg(now))) AS live_seats,
       (SELECT count(*) FROM action x
         WHERE x.target_type = 'actor' AND x.target_id = a.id AND x.action_type = 'member.add_delegate'
           AND x.status = 'proposed') AS pending_requests
FROM actor a
LEFT JOIN LATERAL (
    SELECT c.last_used_at FROM credential c
    WHERE c.actor_id = a.id AND c.kind = 'api_token' AND c.revoked_at IS NULL AND c.last_used_at IS NOT NULL
      AND (c.expires_at IS NULL OR c.expires_at > sqlc.arg(now))
    ORDER BY c.last_used_at DESC LIMIT 1) seen ON true
WHERE a.owner_actor_id = sqlc.arg(owner_actor_id)
ORDER BY a.id;

-- name: AgentLastSeen :many
-- When an agent last used a token that still works: no row if never.
SELECT c.last_used_at FROM credential c
WHERE c.actor_id = sqlc.arg(actor_id) AND c.kind = 'api_token' AND c.revoked_at IS NULL AND c.last_used_at IS NOT NULL
  AND (c.expires_at IS NULL OR c.expires_at > sqlc.arg(now))
ORDER BY c.last_used_at DESC LIMIT 1;

-- name: ListSeatsOfActor :many
-- Every seat an actor holds that is not removed, with its course and the
-- name of the preset it was copied from.
SELECT m.id AS member_id, m.course_id, c.code, c.section, c.title, c.status AS course_status,
       m.status, m.expires_at, m.student_scope, m.assignment_scope, m.principal_member_id, p.name AS preset_name
FROM course_member m
JOIN course c ON c.id = m.course_id
LEFT JOIN permission_preset p ON p.id = m.preset_id
WHERE m.actor_id = $1 AND m.status <> 'removed'
ORDER BY c.code, c.section, m.id;

-- name: ListDelegateRequestsFor :many
-- The proposals to seat an agent as someone's delegate that wait for a
-- decision.
SELECT x.id, x.course_id, c.code, c.section, c.title, x.created_at
FROM action x
JOIN course c ON c.id = x.course_id
WHERE x.target_type = 'actor' AND x.target_id = $1 AND x.action_type = 'member.add_delegate' AND x.status = 'proposed'
ORDER BY x.id;

-- name: SuspendAgentByOwner :execrows
UPDATE actor SET status = 'suspended', suspended_by_actor_id = sqlc.arg(owner_actor_id)
WHERE id = sqlc.arg(id) AND owner_actor_id = sqlc.arg(owner_actor_id) AND status = 'active';

-- name: ReactivateAgentByOwner :execrows
-- Only a suspension the owner made: one an administrator made, or one made
-- before this was recorded, is an administrator's to lift.
UPDATE actor SET status = 'active', suspended_by_actor_id = NULL
WHERE id = sqlc.arg(id) AND owner_actor_id = sqlc.arg(owner_actor_id) AND status = 'suspended'
  AND suspended_by_actor_id = sqlc.arg(owner_actor_id);

-- name: SetActorOwner :exec
UPDATE actor SET owner_actor_id = sqlc.narg(owner_actor_id) WHERE id = sqlc.arg(id);

-- name: CountSeatsInOpenCourses :one
-- Seats an actor holds, not removed, in courses that are not archived: while
-- there is one, its owner does not change.
SELECT count(*) FROM course_member m JOIN course c ON c.id = m.course_id
WHERE m.actor_id = $1 AND m.status <> 'removed' AND c.status <> 'archived';

-- name: RevokeBearerCredentials :exec
-- Every token and session an actor has: they may be in the hands of
-- whoever owned it before.
UPDATE credential SET revoked_at = $2
WHERE actor_id = $1 AND kind IN ('api_token', 'session') AND revoked_at IS NULL;

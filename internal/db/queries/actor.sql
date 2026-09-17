-- name: GetActor :one
-- The whole row, for showing an actor. Authorization uses GetActorForAuthz,
-- which leaves kind out on purpose.
SELECT id, kind, display_name, email, status, platform_role, created_by_actor_id, created_at
FROM actor
WHERE id = $1;

-- name: GetActorByEmail :one
SELECT id, status FROM actor WHERE lower(email) = lower($1);

-- name: InsertActor :exec
INSERT INTO actor (id, kind, display_name, email, status, platform_role, created_by_actor_id, created_at)
VALUES ($1, $2, $3, $4, 'active', $5, $6, $7);

-- name: CountRootActors :one
SELECT count(*) FROM actor WHERE platform_role = 'root';

-- name: GetSystemActor :one
SELECT id FROM actor WHERE kind = 'system' ORDER BY created_at LIMIT 1;

-- name: ListMembershipsForActor :many
SELECT m.id AS member_id, m.course_id, c.code, c.section, c.title, c.status AS course_status,
       m.role, m.status, m.expires_at, m.student_scope, m.assignment_scope
FROM course_member m
JOIN course c ON c.id = m.course_id
WHERE m.actor_id = $1 AND m.status <> 'removed'
ORDER BY c.code, c.section, m.id;

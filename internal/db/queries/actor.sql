-- name: GetActor :one
-- The whole row, for showing an actor. Authorization uses GetActorForAuthz,
-- which leaves kind out on purpose.
SELECT id, kind, display_name, email, status, platform_role, created_by_actor_id, created_at
FROM actor
WHERE id = $1;

-- name: ListActors :many
-- pattern is an ILIKE pattern the caller has made from a search term, with
-- the term's own %, _ and \ escaped. No index serves it, since the term may
-- start anywhere in a name or an address: the walk is the primary key's, in
-- id order, and stops at max_rows, which for an installation's actors
-- (thousands, not millions) is cheap enough. platform_role 'none' asks for
-- the actors who hold neither role; the column's CHECK keeps it from naming
-- a real one.
SELECT id, kind, display_name, email, status, platform_role, created_by_actor_id, created_at
FROM actor
WHERE id > sqlc.arg(after)
  AND (sqlc.narg(pattern)::text IS NULL
       OR display_name ILIKE sqlc.narg(pattern) OR email ILIKE sqlc.narg(pattern))
  AND (sqlc.narg(kind)::text IS NULL OR kind = sqlc.narg(kind))
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
  AND (sqlc.narg(platform_role)::text IS NULL OR coalesce(platform_role, 'none') = sqlc.narg(platform_role))
ORDER BY id
LIMIT sqlc.arg(max_rows);

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

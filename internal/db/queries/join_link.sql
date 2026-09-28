-- name: InsertJoinLink :exec
INSERT INTO course_join_link (id, course_id, token_prefix, secret_hash, role, preset_id, created_by_member_id, expires_at,
                              max_uses, allowed_email_domains, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, sqlc.narg(max_uses), sqlc.narg(allowed_email_domains), $9);

-- name: GetJoinLinkByPrefix :one
-- A link found by the prefix of a token someone presents, with what the page
-- that opens it may show of its course. The token is checked against
-- secret_hash before anything here is believed.
SELECT l.*, c.code, c.section, c.title, c.status AS course_status
FROM course_join_link l
JOIN course c ON c.id = l.course_id
WHERE l.token_prefix = $1;

-- name: GetJoinLinkInCourse :one
SELECT * FROM course_join_link WHERE id = $1 AND course_id = $2;

-- name: LockJoinLink :one
-- The link, held for the rest of the call: every join through it and its
-- revocation take it in turn, so that a use is counted against what the one
-- before left, and a revocation waits for the joins in flight or is waited
-- for. NO KEY UPDATE: a seat that names the link takes KEY SHARE on it
-- through its foreign key, which this leaves alone.
SELECT * FROM course_join_link WHERE id = $1 FOR NO KEY UPDATE;

-- name: CountJoinLinkUse :execrows
-- One use more, never past the limit: none when the limit is reached, and
-- the CHECK course_join_link_uses_counted refuses it whatever asks.
UPDATE course_join_link SET uses = uses + 1
WHERE id = $1 AND (max_uses IS NULL OR uses < max_uses);

-- name: RevokeJoinLink :execrows
UPDATE course_join_link SET revoked_at = sqlc.arg(revoked_at), revoked_by_member_id = sqlc.arg(revoked_by_member_id)
WHERE id = sqlc.arg(id) AND revoked_at IS NULL;

-- name: ListJoinLinks :many
-- A course's links, newest first — the one on the screen now at the top —
-- from before the id given, with the names of whoever made and revoked each.
-- Never the token: only its hash is kept, and that is not selected.
SELECT l.id, l.course_id, l.role, l.preset_id, l.created_by_member_id, l.expires_at, l.max_uses, l.uses,
       l.allowed_email_domains, l.revoked_at, l.revoked_by_member_id, l.created_at,
       ca.display_name AS created_by_name, ra.display_name AS revoked_by_name
FROM course_join_link l
JOIN course_member cm ON cm.id = l.created_by_member_id
JOIN actor ca ON ca.id = cm.actor_id
LEFT JOIN course_member rm ON rm.id = l.revoked_by_member_id
LEFT JOIN actor ra ON ra.id = rm.actor_id
WHERE l.course_id = $1 AND (sqlc.narg(before)::uuid IS NULL OR l.id < sqlc.narg(before))
ORDER BY l.id DESC
LIMIT sqlc.arg(max_rows);

-- name: InsertRegisteredPerson :exec
-- A person who registers through a join link: their email is theirs to vouch
-- for alone (email_verified false), and whoever made the link, on whose
-- authority they are let in, is who created them.
INSERT INTO actor (id, kind, display_name, email, email_verified, status, created_by_actor_id, created_at)
VALUES ($1, 'human', $2, $3, false, 'active', $4, $5);

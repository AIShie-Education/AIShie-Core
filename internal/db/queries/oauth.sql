-- Agent-actor OAuth Client Credentials (migration 0034). Lists deliberately
-- select no secret or token hash; those are needed only by authentication.

-- name: InsertOAuthClient :exec
INSERT INTO oauth_client (id, agent_actor_id, label, secret_hash, scopes, created_by_actor_id, created_at)
VALUES (sqlc.arg(id), sqlc.arg(agent_actor_id), sqlc.arg(label), sqlc.arg(secret_hash), sqlc.arg(scopes),
        sqlc.narg(created_by_actor_id), sqlc.arg(created_at));

-- name: ListOAuthClientsForAgent :many
SELECT id, agent_actor_id, label, scopes, created_by_actor_id, created_at, rotated_at, revoked_at
FROM oauth_client
WHERE agent_actor_id = sqlc.arg(agent_actor_id)
ORDER BY created_at DESC, id;

-- name: GetOAuthClientForAgent :one
SELECT id, agent_actor_id, label, scopes, created_by_actor_id, created_at, rotated_at, revoked_at
FROM oauth_client
WHERE id = sqlc.arg(id) AND agent_actor_id = sqlc.arg(agent_actor_id);

-- name: LockOAuthClientForToken :one
-- Secret verification, token issue, rotation and revocation serialize on the
-- client row, so a rotation cannot leave a newly issued token valid under the
-- old secret. Agent kind and standing are checked on every token request.
SELECT c.id, c.agent_actor_id, c.label, c.secret_hash, c.scopes, c.created_by_actor_id,
       c.created_at, c.rotated_at, c.revoked_at,
       a.kind AS agent_kind, a.status AS agent_status,
       a.owner_actor_id AS agent_owner_actor_id, o.status AS agent_owner_status
FROM oauth_client c
JOIN actor a ON a.id = c.agent_actor_id
LEFT JOIN actor o ON o.id = a.owner_actor_id
WHERE c.id = sqlc.arg(id)
FOR UPDATE OF c;

-- name: RotateOAuthClientSecret :execrows
UPDATE oauth_client SET secret_hash = sqlc.arg(secret_hash), rotated_at = sqlc.arg(rotated_at)
WHERE id = sqlc.arg(id) AND agent_actor_id = sqlc.arg(agent_actor_id) AND revoked_at IS NULL;

-- name: RevokeOAuthClient :execrows
UPDATE oauth_client SET revoked_at = sqlc.arg(revoked_at)
WHERE id = sqlc.arg(id) AND agent_actor_id = sqlc.arg(agent_actor_id) AND revoked_at IS NULL;

-- name: RevokeOAuthAccessTokensForClient :exec
UPDATE oauth_access_token SET revoked_at = sqlc.arg(revoked_at)
WHERE client_id = sqlc.arg(client_id) AND revoked_at IS NULL;

-- name: InsertOAuthAccessToken :exec
INSERT INTO oauth_access_token (id, client_id, agent_actor_id, token_hash, scopes, resource, created_at, expires_at)
VALUES (sqlc.arg(id), sqlc.arg(client_id), sqlc.arg(agent_actor_id), sqlc.arg(token_hash), sqlc.arg(scopes),
        sqlc.arg(resource), sqlc.arg(created_at), sqlc.arg(expires_at));

-- name: GetOAuthAccessTokenByHash :one
-- Every MCP request checks the token, its client and its agent again. Rotation
-- and revocation mark every outstanding token revoked in the same transaction.
SELECT t.id, t.client_id, t.agent_actor_id, t.scopes, t.resource, t.created_at, t.expires_at,
       t.revoked_at, t.last_used_at, c.revoked_at AS client_revoked_at,
       a.kind AS agent_kind, a.status AS agent_status,
       a.owner_actor_id AS agent_owner_actor_id, o.status AS agent_owner_status
FROM oauth_access_token t
JOIN oauth_client c ON c.id = t.client_id
JOIN actor a ON a.id = t.agent_actor_id
LEFT JOIN actor o ON o.id = a.owner_actor_id
WHERE t.token_hash = sqlc.arg(token_hash);

-- name: TouchOAuthAccessToken :exec
-- Best-effort usage accounting, at most one write a minute per token.
UPDATE oauth_access_token SET last_used_at = sqlc.arg(last_used_at)
WHERE id = sqlc.arg(id)
  AND (last_used_at IS NULL OR last_used_at < sqlc.arg(last_used_at)::timestamptz - interval '1 minute');

-- name: GetCredentialByPrefix :one
-- A token or a session, found by its public prefix before its hash is
-- checked. Revoked and expired rows are returned too, so that the caller can
-- tell them from an unknown prefix in its logs; it rejects all three alike.
SELECT id, actor_id, kind, secret_hash, expires_at, revoked_at
FROM credential
WHERE token_prefix = $1 AND kind IN ('api_token', 'session');

-- name: TouchCredential :exec
-- At most one write a minute per credential, however busy it is. The cast is
-- needed: left to itself, PostgreSQL reads $2 - interval as interval - interval,
-- and every call fails.
UPDATE credential SET last_used_at = $2
WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < $2::timestamptz - interval '1 minute');

-- name: InsertCredential :exec
INSERT INTO credential (id, actor_id, kind, secret_hash, provider, subject, token_prefix, label, expires_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: GetPasswordCredential :one
SELECT id, secret_hash
FROM credential
WHERE actor_id = $1 AND kind = 'password' AND revoked_at IS NULL
ORDER BY created_at DESC
LIMIT 1;

-- name: RevokePasswordCredentials :exec
UPDATE credential SET revoked_at = $2
WHERE actor_id = $1 AND kind = 'password' AND revoked_at IS NULL;

-- name: RevokeCredential :execrows
-- Only the owner's own credential; someone else's id changes nothing.
UPDATE credential SET revoked_at = $3
WHERE id = $1 AND actor_id = $2 AND revoked_at IS NULL;

-- name: RevokeCredentialByID :exec
UPDATE credential SET revoked_at = $2
WHERE id = $1 AND revoked_at IS NULL;

-- name: GetCredentialForActor :one
SELECT id, kind FROM credential WHERE id = $1 AND actor_id = $2;

-- name: ListCredentialsForActor :many
-- Never the hash.
SELECT id, kind, provider, subject, token_prefix, label, last_used_at, expires_at, revoked_at, created_at
FROM credential
WHERE actor_id = $1
ORDER BY created_at DESC, id;

-- name: GetSSOCredential :one
-- The account an identity provider's subject is linked to, if any.
SELECT c.id, c.actor_id, c.revoked_at, a.status AS actor_status
FROM credential c
JOIN actor a ON a.id = c.actor_id
WHERE c.kind = 'sso' AND c.provider = $1 AND c.subject = $2;

-- name: ReviveSSOCredential :exec
-- Linking again an identity that was unlinked from the same actor.
UPDATE credential SET revoked_at = NULL WHERE id = $1 AND kind = 'sso';

-- name: GetCredentialByPrefix :one
-- A token, a session or a service's credential, found by its public prefix
-- before its hash is checked. Revoked and expired rows are returned too, so
-- that the caller can tell them from an unknown prefix in its logs; it
-- rejects all three alike. The actor's kind comes with it: a credential of
-- the system actor's is rejected the same way.
SELECT c.id, c.actor_id, c.kind, c.secret_hash, c.expires_at, c.revoked_at, a.kind AS actor_kind
FROM credential c
JOIN actor a ON a.id = c.actor_id
WHERE c.token_prefix = $1 AND c.kind IN ('api_token', 'session', 'service');

-- name: TouchCredential :exec
-- At most one write a minute per credential, however busy it is. The cast is
-- needed: left to itself, PostgreSQL reads $2 - interval as interval - interval,
-- and every call fails.
UPDATE credential SET last_used_at = $2
WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < $2::timestamptz - interval '1 minute');

-- name: InsertCredential :exec
INSERT INTO credential (id, actor_id, kind, secret_hash, provider, subject, token_prefix, label, expires_at, created_at,
                        issued_by_actor_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);

-- name: GetPasswordCredential :one
-- The actor's live password, and whether someone else set it for them to
-- change (must_change).
SELECT id, secret_hash, must_change
FROM credential
WHERE actor_id = $1 AND kind = 'password' AND revoked_at IS NULL
ORDER BY created_at DESC
LIMIT 1;

-- name: RevokePasswordCredentials :exec
UPDATE credential SET revoked_at = $2
WHERE actor_id = $1 AND kind = 'password' AND revoked_at IS NULL;

-- name: InsertTemporaryPassword :exec
-- A password someone else set for its person, who must change it before
-- anything else (member.reset_password): marked must_change, saying who set
-- it, as the CHECK credential_must_change_is_an_issued_password holds.
INSERT INTO credential (id, actor_id, kind, secret_hash, label, created_at, issued_by_actor_id, must_change)
VALUES (sqlc.arg(id), sqlc.arg(actor_id), 'password', sqlc.arg(secret_hash), sqlc.arg(label), sqlc.arg(created_at),
        sqlc.arg(issued_by_actor_id), true);

-- name: RevokeSessions :execrows
-- Signs a person out everywhere: every browser session they have.
UPDATE credential SET revoked_at = $2
WHERE actor_id = $1 AND kind = 'session' AND revoked_at IS NULL;

-- name: PasswordChangeRequired :one
-- Whether an actor's live password is one someone else set, which they must
-- change before anything else.
SELECT EXISTS (SELECT 1 FROM credential
               WHERE actor_id = $1 AND kind = 'password' AND revoked_at IS NULL AND must_change)::bool;

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
-- Never the hash. The issuer's name comes with the row, for an administrator
-- telling one token from another.
SELECT c.id, c.kind, c.provider, c.subject, c.token_prefix, c.label, c.last_used_at, c.expires_at, c.revoked_at,
       c.created_at, c.issued_by_actor_id, i.display_name AS issued_by_name, c.must_change, c.issued_to_service
FROM credential c
LEFT JOIN actor i ON i.id = c.issued_by_actor_id
WHERE c.actor_id = $1
ORDER BY c.created_at DESC, c.id;

-- name: GetSSOCredential :one
-- The account an identity provider's subject is linked to, if any, and its
-- kind, read to refuse: an agent does not sign in so.
SELECT c.id, c.actor_id, c.revoked_at, a.status AS actor_status, a.kind AS actor_kind
FROM credential c
JOIN actor a ON a.id = c.actor_id
WHERE c.kind = 'sso' AND c.provider = $1 AND c.subject = $2;

-- name: ReviveSSOCredential :exec
-- Linking again an identity that was unlinked from the same actor.
UPDATE credential SET revoked_at = NULL WHERE id = $1 AND kind = 'sso';

-- name: RevokeInvites :exec
-- An actor's live invitation: when another replaces it, when a password is
-- set (by it or otherwise), and when the email it was sent to changes.
UPDATE credential SET revoked_at = $2
WHERE actor_id = $1 AND kind = 'invite' AND revoked_at IS NULL;

-- name: GetInviteByPrefix :one
-- An invitation, found by its prefix before its hash is checked, and locked:
-- it is used once, and two tries at it take turns. Revoked and expired rows
-- are returned too, as GetCredentialByPrefix returns them. The actor comes
-- with it, and who issued it: null for one made before that was recorded.
SELECT c.id, c.actor_id, c.secret_hash, c.expires_at, c.revoked_at, c.issued_by_actor_id,
       a.kind AS actor_kind, a.status AS actor_status, a.email AS actor_email, a.login_id AS actor_login_id
FROM credential c
JOIN actor a ON a.id = c.actor_id
WHERE c.token_prefix = $1 AND c.kind = 'invite'
FOR UPDATE OF c;

-- name: InsertRuntimeToken :exec
-- A runtime agent's token, issued to the site's agent runtime by the
-- agent_runtime service (issued_by_actor_id): it never expires, and is the
-- agent's one token that is not revoked (credential_one_runtime_token,
-- credential_fits_hosting).
INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, label, created_at, issued_by_actor_id, issued_to_service)
VALUES (sqlc.arg(id), sqlc.arg(actor_id), 'api_token', sqlc.arg(secret_hash), sqlc.arg(token_prefix), sqlc.arg(label),
        sqlc.arg(created_at), sqlc.arg(issued_by_actor_id), 'agent_runtime');

-- name: RevokeRuntimeTokens :many
-- Every runtime token of an agent's that is not revoked: one at most.
UPDATE credential SET revoked_at = sqlc.arg(now)
WHERE actor_id = sqlc.arg(actor_id) AND issued_to_service = 'agent_runtime' AND revoked_at IS NULL
RETURNING id;


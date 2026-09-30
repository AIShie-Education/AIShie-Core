-- name: ListSSOProviders :many
-- Every identity provider the site's administrators set up, in the order the
-- sign-in page shows them, with who made and last changed each, and how many
-- accounts are linked at it now (live identities, whatever their actors'
-- status). The sealed secret comes with the row, for the caller to see
-- whether it opens; it is never shown.
SELECT p.id, p.display_name, p.issuer, p.client_id, p.client_secret_sealed, p.client_secret_hint, p.scopes,
       p.subject_claim, p.email_claim, p.allowed_email_domains, p.link_by_email, p.enabled, p.position, p.version,
       p.created_by_actor_id, cb.display_name AS created_by_name, p.created_at,
       p.updated_by_actor_id, ub.display_name AS updated_by_name, p.updated_at,
       (SELECT count(*) FROM credential c WHERE c.kind = 'sso' AND c.provider = p.id AND c.revoked_at IS NULL)::int AS linked_accounts
FROM sso_provider p
JOIN actor cb ON cb.id = p.created_by_actor_id
JOIN actor ub ON ub.id = p.updated_by_actor_id
ORDER BY p.position, p.id;

-- name: GetSSOProvider :one
SELECT p.id, p.display_name, p.issuer, p.client_id, p.client_secret_sealed, p.client_secret_hint, p.scopes,
       p.subject_claim, p.email_claim, p.allowed_email_domains, p.link_by_email, p.enabled, p.position, p.version,
       p.created_by_actor_id, cb.display_name AS created_by_name, p.created_at,
       p.updated_by_actor_id, ub.display_name AS updated_by_name, p.updated_at,
       (SELECT count(*) FROM credential c WHERE c.kind = 'sso' AND c.provider = p.id AND c.revoked_at IS NULL)::int AS linked_accounts
FROM sso_provider p
JOIN actor cb ON cb.id = p.created_by_actor_id
JOIN actor ub ON ub.id = p.updated_by_actor_id
WHERE p.id = $1;

-- name: LockSSOProvider :one
-- A provider, held for the change a write makes to it: two writes made over
-- the same version meet here, and the second finds the version moved on.
SELECT id, client_secret_sealed, version, enabled
FROM sso_provider
WHERE id = $1
FOR UPDATE;

-- name: SSOProviderExists :one
SELECT EXISTS (SELECT 1 FROM sso_provider WHERE id = $1)::bool;

-- name: NextSSOPosition :one
-- Where a provider added without a position goes: after every other.
SELECT (coalesce(max(position), 0) + 1)::int FROM sso_provider;

-- name: InsertSSOProvider :exec
INSERT INTO sso_provider (id, display_name, issuer, client_id, client_secret_sealed, client_secret_hint, scopes,
                          subject_claim, email_claim, allowed_email_domains, link_by_email, enabled, position,
                          created_by_actor_id, created_at, updated_by_actor_id, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(display_name), sqlc.arg(issuer), sqlc.arg(client_id), sqlc.arg(client_secret_sealed),
        sqlc.arg(client_secret_hint), sqlc.arg(scopes), sqlc.arg(subject_claim), sqlc.narg(email_claim),
        sqlc.arg(allowed_email_domains), sqlc.arg(link_by_email), sqlc.arg(enabled), sqlc.arg(position),
        sqlc.arg(actor_id), sqlc.arg(now), sqlc.arg(actor_id), sqlc.arg(now));

-- name: UpdateSSOProvider :one
-- Writes every field over the version its writer read, and moves the version
-- on; no row comes back when the version moved on first.
UPDATE sso_provider
SET display_name = sqlc.arg(display_name), issuer = sqlc.arg(issuer), client_id = sqlc.arg(client_id),
    client_secret_sealed = sqlc.arg(client_secret_sealed), client_secret_hint = sqlc.arg(client_secret_hint),
    scopes = sqlc.arg(scopes), subject_claim = sqlc.arg(subject_claim), email_claim = sqlc.narg(email_claim),
    allowed_email_domains = sqlc.arg(allowed_email_domains), link_by_email = sqlc.arg(link_by_email),
    position = sqlc.arg(position), version = version + 1,
    updated_by_actor_id = sqlc.arg(actor_id), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version)
RETURNING version;

-- name: SetSSOProviderEnabled :one
UPDATE sso_provider
SET enabled = sqlc.arg(enabled), version = version + 1, updated_by_actor_id = sqlc.arg(actor_id), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version)
RETURNING version;

-- name: DeleteSSOProvider :execrows
DELETE FROM sso_provider WHERE id = $1 AND version = $2;

-- name: CountSSOLinks :one
-- The live identities linked at a provider: the accounts that sign in
-- through it, whatever their actors' status.
SELECT count(*)::int FROM credential WHERE kind = 'sso' AND provider = $1 AND revoked_at IS NULL;

-- name: RevokeSSOLinks :execrows
-- Every live identity linked at a provider that is going. Revoked, not
-- deleted: an identity once linked to an account is never linked to another
-- (unique (provider, subject)), and linking it again to the same one revives
-- it.
UPDATE credential SET revoked_at = $2 WHERE kind = 'sso' AND provider = $1 AND revoked_at IS NULL;

-- name: ListEnabledSSOProviders :many
-- What a sign-in reads: the providers switched on, in the sign-in page's
-- order, with what signing in through each takes, the sealed secret among it.
SELECT id, display_name, issuer, client_id, client_secret_sealed, scopes, subject_claim, email_claim,
       allowed_email_domains, link_by_email, version
FROM sso_provider
WHERE enabled
ORDER BY position, id;

-- name: ListSealedSSOSecrets :many
-- Every sealed client secret, for `aishie-core secrets rewrap`.
SELECT id, client_secret_sealed FROM sso_provider ORDER BY id;

-- name: RewrapSSOSecret :execrows
-- A secret sealed again under the current key, written only over what was
-- read: a change made meanwhile is left as it is. The secret is the same, so
-- the version does not move.
UPDATE sso_provider SET client_secret_sealed = sqlc.arg(sealed)
WHERE id = sqlc.arg(id) AND client_secret_sealed = sqlc.arg(was);

-- name: GetSSOLinkByEmailCandidate :one
-- The person an email names, as linking by a verified email reads them: only
-- an active person whose email someone vouches for here, with no platform
-- role, is ever linked so.
SELECT id, kind, status, email, email_verified, platform_role
FROM actor
WHERE lower(email) = lower(sqlc.arg(email));

-- name: SSOIdentityKnown :one
-- Whether an identity has ever been linked at a provider, to anyone, revoked
-- or not.
SELECT EXISTS (SELECT 1 FROM credential WHERE kind = 'sso' AND provider = $1 AND subject = $2)::bool;

-- name: HasLiveSSOLinkAt :one
-- Whether an actor already has a live identity at a provider.
SELECT EXISTS (SELECT 1 FROM credential
               WHERE kind = 'sso' AND provider = $1 AND actor_id = $2 AND revoked_at IS NULL)::bool;

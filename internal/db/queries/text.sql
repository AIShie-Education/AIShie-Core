-- Text versions (docs/schema.md §2.4, Text versions): the Markdown a file of
-- a version is transcribed into, by the site's service (source ai) or
-- written by staff (source staff). One to a file, keyed by its version and
-- the file.

-- name: GetTextView :one
-- A file's text version as its readers are shown it, without the text.
-- Who edited it comes with their name.
SELECT t.version_id, t.file_id, t.status, t.source, t.pages, t.model, t.reason, t.revision, t.produced_at,
       t.edited_by_member_id, t.edited_at, t.updated_at,
       coalesce(octet_length(t.body), 0)::int AS bytes,
       e.display_name AS edited_by_name
FROM document_version_text t
LEFT JOIN course_member m ON m.id = t.edited_by_member_id
LEFT JOIN actor e ON e.id = m.actor_id
WHERE t.version_id = sqlc.arg(version_id) AND t.file_id = sqlc.arg(file_id);

-- name: ListTextViews :many
-- The text versions of the files of a document's versions, without their
-- text.
SELECT t.version_id, t.file_id, t.status, t.source, t.pages, t.model, t.reason, t.revision, t.produced_at,
       t.edited_by_member_id, t.edited_at, t.updated_at,
       coalesce(octet_length(t.body), 0)::int AS bytes,
       e.display_name AS edited_by_name
FROM document_version_text t
LEFT JOIN course_member m ON m.id = t.edited_by_member_id
LEFT JOIN actor e ON e.id = m.actor_id
WHERE t.document_id = $1;

-- name: ListVersionTextViews :many
-- The text versions of a version's files, without their text.
SELECT t.version_id, t.file_id, t.status, t.source, t.pages, t.model, t.reason, t.revision, t.produced_at,
       t.edited_by_member_id, t.edited_at, t.updated_at,
       coalesce(octet_length(t.body), 0)::int AS bytes,
       e.display_name AS edited_by_name
FROM document_version_text t
LEFT JOIN course_member m ON m.id = t.edited_by_member_id
LEFT JOIN actor e ON e.id = m.actor_id
WHERE t.version_id = $1;

-- name: GetTextBody :one
-- A file's text version with its whole text, for reading it in parts.
SELECT t.version_id, t.file_id, t.status, t.source, t.pages, t.model, t.reason, t.revision, t.produced_at,
       t.edited_by_member_id, t.edited_at, t.updated_at,
       coalesce(octet_length(t.body), 0)::int AS bytes, t.body,
       e.display_name AS edited_by_name
FROM document_version_text t
LEFT JOIN course_member m ON m.id = t.edited_by_member_id
LEFT JOIN actor e ON e.id = m.actor_id
WHERE t.version_id = sqlc.arg(version_id) AND t.file_id = sqlc.arg(file_id);

-- name: LockText :one
-- A file's text version, held for a change to it. Whoever changes it holds
-- the document first, as adding and purging a version do.
SELECT * FROM document_version_text
WHERE version_id = sqlc.arg(version_id) AND file_id = sqlc.arg(file_id) AND document_id = sqlc.arg(document_id)
FOR UPDATE;

-- name: QueueNewText :exec
-- A text version for a file of a version from before there were any, which
-- nobody queued (the backfill queued the published and the latest): asked
-- for by staff, it is queued as an upload is.
INSERT INTO document_version_text (version_id, file_id, document_id, course_id, queued_at, created_at, updated_at)
VALUES (sqlc.arg(version_id), sqlc.arg(file_id), sqlc.arg(document_id), sqlc.arg(course_id), sqlc.arg(now), sqlc.arg(now),
        sqlc.arg(now));

-- name: EditText :one
-- Staff's text in place of whatever there was: done, theirs, never written
-- over by the service. A claim of it ends here; the service's completion of
-- it is refused.
UPDATE document_version_text
SET status = 'done', source = 'staff', body = sqlc.arg(body), reason = NULL,
    lease_id = NULL, claimed_until = NULL,
    edited_by_member_id = sqlc.arg(edited_by_member_id), edited_at = sqlc.arg(now),
    revision = revision + 1, updated_at = sqlc.arg(now)
WHERE version_id = sqlc.arg(version_id) AND file_id = sqlc.arg(file_id)
RETURNING revision;

-- name: RequeueText :one
-- Back to the queue, as an upload is queued: whatever it said goes, a claim
-- of it ends, and its attempts start again.
UPDATE document_version_text
SET status = 'pending', body = NULL, source = NULL, pages = NULL, model = NULL, reason = NULL, produced_at = NULL,
    edited_by_member_id = NULL, edited_at = NULL, lease_id = NULL, claimed_until = NULL,
    attempts = 0, backfill = false, queued_at = sqlc.arg(now),
    revision = revision + CASE WHEN body IS NULL THEN 0 ELSE 1 END, updated_at = sqlc.arg(now)
WHERE version_id = sqlc.arg(version_id) AND file_id = sqlc.arg(file_id)
RETURNING revision;

-- ---------------------------------------------------------------------------
-- The service's queue
-- ---------------------------------------------------------------------------

-- name: ExhaustTexts :exec
-- What has been claimed max_attempts times and not finished fails, rather
-- than be claimed for ever: a file the service cannot get through. SKIP
-- LOCKED, as a claim: what another call holds is its to change, and two
-- claims at once never wait for each other here.
UPDATE document_version_text
SET status = 'failed', reason = 'attempts_exhausted', lease_id = NULL, claimed_until = NULL, updated_at = sqlc.arg(now)
WHERE (version_id, file_id) IN (
    SELECT x.version_id, x.file_id FROM document_version_text x
    WHERE x.attempts >= sqlc.arg(max_attempts)::int
      AND (x.status = 'pending' OR (x.status = 'working' AND x.claimed_until <= sqlc.arg(now)))
    FOR UPDATE SKIP LOCKED);

-- name: ClaimTexts :many
-- Up to max_rows text versions waiting, or whose claim has lapsed, claimed for
-- the caller until claimed_until: uploads first, oldest first, then the
-- backfill, newest first; a version's files in order. SKIP LOCKED: two
-- claims at once never take the same one. What is in an archived course, or
-- of an archived document, waits until it is open again, since nothing is
-- written there meanwhile.
WITH picked AS (
    SELECT t.version_id, t.file_id
    FROM document_version_text t
    JOIN document_version_file f ON f.id = t.file_id
    JOIN document d ON d.id = t.document_id
    JOIN course c ON c.id = t.course_id
    WHERE (t.status = 'pending' OR (t.status = 'working' AND t.claimed_until <= sqlc.arg(now)))
      AND t.attempts < sqlc.arg(max_attempts)::int
      AND d.status = 'active' AND c.status <> 'archived'
    ORDER BY t.backfill,
             CASE WHEN NOT t.backfill THEN t.queued_at END,
             CASE WHEN t.backfill THEN t.queued_at END DESC,
             t.version_id, f.position
    LIMIT sqlc.arg(max_rows)
    FOR UPDATE OF t SKIP LOCKED
)
UPDATE document_version_text t
SET status = 'working', lease_id = gen_random_uuid(), claimed_until = sqlc.arg(claimed_until),
    claimed_by_credential_id = sqlc.arg(credential_id), claimed_at = sqlc.arg(now),
    attempts = t.attempts + 1, updated_at = sqlc.arg(now)
FROM picked, document_version_file f
WHERE t.version_id = picked.version_id AND t.file_id = picked.file_id AND f.id = t.file_id
RETURNING t.version_id, t.file_id, t.document_id, t.course_id, t.lease_id, t.claimed_until, t.attempts, t.backfill, t.queued_at,
          f.position, f.filename, f.storage_key, f.content_type, f.byte_size, f.checksum;

-- name: GetTextForService :one
-- What a call of the service's is about: the version's text versions' course.
SELECT version_id, document_id, course_id FROM document_version_text WHERE version_id = $1 LIMIT 1;

-- name: GetClaimedFile :one
-- The file of a text version the caller's claim holds, by its id.
SELECT t.file_id, t.claimed_until, f.position, f.filename, f.storage_key, f.content_type, f.byte_size, f.checksum
FROM document_version_text t
JOIN document_version_file f ON f.id = t.file_id
WHERE t.version_id = sqlc.arg(version_id) AND t.file_id = sqlc.arg(file_id) AND t.status = 'working'
  AND t.lease_id = sqlc.arg(lease_id);

-- name: RenewTextLease :one
-- A claim held longer, from now; its own and nobody else's.
UPDATE document_version_text
SET claimed_until = sqlc.arg(claimed_until), updated_at = sqlc.arg(now)
WHERE version_id = sqlc.arg(version_id) AND file_id = sqlc.arg(file_id) AND status = 'working' AND lease_id = sqlc.arg(lease_id)
RETURNING claimed_until;

-- name: ShareDocument :one
-- The document's status, held FOR SHARE for a write about one of its
-- versions: archiving it waits for the write, or the write sees it.
SELECT status FROM document WHERE id = $1 FOR SHARE;

-- name: LockTextForService :one
-- The text version a call of the service's is about, by its file, held.
SELECT * FROM document_version_text
WHERE version_id = sqlc.arg(version_id) AND file_id = sqlc.arg(file_id)
FOR UPDATE;

-- name: FinishTextDone :one
-- The service's text: done, the model's, made now.
UPDATE document_version_text
SET status = 'done', source = 'ai', body = sqlc.arg(body), pages = sqlc.arg(pages), model = sqlc.arg(model),
    produced_at = sqlc.arg(now), reason = NULL, lease_id = NULL, claimed_until = NULL,
    revision = revision + 1, updated_at = sqlc.arg(now)
WHERE version_id = sqlc.arg(version_id) AND file_id = sqlc.arg(file_id)
RETURNING revision;

-- name: FinishTextUndone :exec
-- The service could not, or would not: failed or skipped, saying why.
UPDATE document_version_text
SET status = sqlc.arg(status), reason = sqlc.arg(reason), lease_id = NULL, claimed_until = NULL, updated_at = sqlc.arg(now)
WHERE version_id = sqlc.arg(version_id) AND file_id = sqlc.arg(file_id);

-- name: ReleaseTexts :many
-- What a revoked credential had claimed, back in the queue for another,
-- that claim not counted. The courses come back, to wake whoever waits.
UPDATE document_version_text
SET status = 'pending', lease_id = NULL, claimed_until = NULL, attempts = greatest(attempts - 1, 0), updated_at = sqlc.arg(now)
WHERE status = 'working' AND claimed_by_credential_id = sqlc.arg(credential_id)
RETURNING course_id;

-- ---------------------------------------------------------------------------
-- Services and their credentials
-- ---------------------------------------------------------------------------

-- name: InsertServiceActor :exec
-- The service for a scope, made the first time a credential is issued for
-- it, by whoever issues it; there is one for each scope.
INSERT INTO actor (id, kind, display_name, service_scope, created_by_actor_id, created_at)
VALUES (sqlc.arg(id), 'service', sqlc.arg(display_name), sqlc.arg(scope), sqlc.arg(created_by_actor_id), sqlc.arg(created_at))
ON CONFLICT (service_scope) WHERE service_scope IS NOT NULL DO NOTHING;

-- name: GetServiceActor :one
SELECT id, display_name, status, created_at FROM actor WHERE service_scope = $1;

-- name: LockServiceActor :exec
-- Credentials for one service are issued one at a time, and counted so.
SELECT 1 FROM actor WHERE id = $1 FOR NO KEY UPDATE;

-- name: ListLiveServiceCredentials :many
SELECT id FROM credential
WHERE actor_id = sqlc.arg(actor_id) AND kind = 'service' AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > sqlc.arg(now))
ORDER BY id;

-- name: ListServiceCredentials :many
-- A service's credentials, newest first, revoked ones included, and how many
-- claims each holds now: text versions the transcriber's, renditions the
-- agent runtime's. Never the hash.
SELECT c.id, c.token_prefix, c.label, c.last_used_at, c.expires_at, c.revoked_at, c.created_at,
       c.issued_by_actor_id, i.display_name AS issued_by_name,
       ((SELECT count(*) FROM document_version_text t WHERE t.claimed_by_credential_id = c.id AND t.status = 'working')
        + (SELECT count(*) FROM file_rendition r WHERE r.claimed_by_credential_id = c.id AND r.status = 'claimed'))::int AS claims_held
FROM credential c
LEFT JOIN actor i ON i.id = c.issued_by_actor_id
WHERE c.actor_id = $1 AND c.kind = 'service'
ORDER BY c.created_at DESC, c.id;

-- name: RevokeServiceCredential :execrows
UPDATE credential SET revoked_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND actor_id = sqlc.arg(actor_id) AND kind = 'service' AND revoked_at IS NULL;

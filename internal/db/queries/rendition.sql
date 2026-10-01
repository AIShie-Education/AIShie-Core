-- Renditions (docs/schema.md §2.4, Renditions): the PDF an Office or
-- OpenDocument file is converted into, once, by the site's agent runtime.
-- One to a file, of a version (file_id) or of a message (attachment_id),
-- queued by the database as the file is recorded. Who may read one is who
-- may read its file, which the application decides.

-- name: ListRenditionsOfFiles :many
-- The renditions of the given files of versions; a file that has none is
-- not converted.
SELECT * FROM file_rendition WHERE file_id = ANY(sqlc.arg(file_ids)::uuid[]);

-- name: ListRenditionsOfAttachments :many
-- The renditions of the given files of messages.
SELECT * FROM file_rendition WHERE attachment_id = ANY(sqlc.arg(attachment_ids)::uuid[]);

-- name: ListDocumentRenditionStates :many
-- Where the renditions of the files of every version of a document stand.
SELECT r.file_id::uuid AS file_id, r.status
FROM file_rendition r
JOIN document_version_file f ON f.id = r.file_id
WHERE f.document_id = $1;

-- name: LockRenditionOfFile :one
-- A file's rendition, held for a change to it.
SELECT * FROM file_rendition WHERE file_id = $1 FOR UPDATE;

-- name: LockRenditionOfAttachment :one
-- A message's file's rendition, held for a change to it.
SELECT * FROM file_rendition WHERE attachment_id = $1 FOR UPDATE;

-- name: RequeueRendition :exec
-- Back to the queue, as an upload is queued: why it failed goes, and its
-- attempts start again.
UPDATE file_rendition
SET status = 'queued', reason = NULL, attempts = 0, backfill = false, queued_at = sqlc.arg(now), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND status IN ('failed', 'skipped');

-- name: RenditionKeysOfVersions :many
-- The PDFs of the files of these versions, which go from the file store
-- when the versions are purged.
SELECT r.storage_key::text AS storage_key
FROM file_rendition r
JOIN document_version_file f ON f.id = r.file_id
WHERE f.version_id = ANY(sqlc.arg(version_ids)::uuid[]) AND r.storage_key IS NOT NULL
ORDER BY r.storage_key;

-- ---------------------------------------------------------------------------
-- The runtime's queue
-- ---------------------------------------------------------------------------

-- name: ExhaustRenditions :exec
-- What has been claimed max_attempts times and not finished fails, rather
-- than be claimed for ever: a file the runtime cannot get through. SKIP
-- LOCKED, as a claim: what another call holds is its to change.
UPDATE file_rendition
SET status = 'failed', reason = 'attempts_exhausted', lease_id = NULL, claimed_until = NULL, updated_at = sqlc.arg(now)
WHERE id IN (
    SELECT x.id FROM file_rendition x
    WHERE x.attempts >= sqlc.arg(max_attempts)::int
      AND (x.status = 'queued' OR (x.status = 'claimed' AND x.claimed_until <= sqlc.arg(now)))
    FOR UPDATE SKIP LOCKED);

-- name: ClaimRenditions :many
-- Up to max_rows renditions waiting, or whose claim has lapsed, claimed for
-- the caller until claimed_until: what was queued as its file was recorded
-- first, the oldest first, then the backfill, the newest first. SKIP
-- LOCKED: two claims at once never take the same one. Each comes with its
-- file, a version's or a message's.
WITH picked AS (
    SELECT r.id
    FROM file_rendition r
    WHERE (r.status = 'queued' OR (r.status = 'claimed' AND r.claimed_until <= sqlc.arg(now)))
      AND r.attempts < sqlc.arg(max_attempts)::int
    ORDER BY r.backfill,
             CASE WHEN NOT r.backfill THEN r.queued_at END,
             CASE WHEN r.backfill THEN r.queued_at END DESC,
             r.id
    LIMIT sqlc.arg(max_rows)
    FOR UPDATE SKIP LOCKED
), claimed AS (
    UPDATE file_rendition r
    SET status = 'claimed', lease_id = gen_random_uuid(), claimed_until = sqlc.arg(claimed_until),
        claimed_by_credential_id = sqlc.arg(credential_id), claimed_at = sqlc.arg(now),
        attempts = r.attempts + 1, updated_at = sqlc.arg(now)
    FROM picked
    WHERE r.id = picked.id
    RETURNING r.id, r.course_id, r.file_id, r.attachment_id, r.lease_id, r.claimed_until, r.attempts, r.backfill, r.queued_at
)
SELECT c.id, c.course_id, c.file_id, c.attachment_id, c.lease_id, c.claimed_until, c.attempts, c.backfill, c.queued_at,
       coalesce(f.filename, a.filename)::text AS filename, coalesce(f.storage_key, a.storage_key)::text AS storage_key,
       coalesce(f.content_type, a.content_type)::text AS content_type, coalesce(f.byte_size, a.byte_size)::bigint AS byte_size,
       coalesce(f.checksum, a.checksum) AS checksum
FROM claimed c
LEFT JOIN document_version_file f ON f.id = c.file_id
LEFT JOIN conversation_attachment a ON a.id = c.attachment_id;

-- name: GetRenditionForService :one
-- What a call of the runtime's is about: the rendition's course.
SELECT id, course_id FROM file_rendition WHERE id = $1;

-- name: GetClaimedRendition :one
-- A rendition the caller's claim holds, with its file.
SELECT r.id, r.course_id, r.file_id, r.attachment_id, r.claimed_until,
       coalesce(f.filename, a.filename)::text AS filename, coalesce(f.storage_key, a.storage_key)::text AS storage_key,
       coalesce(f.content_type, a.content_type)::text AS content_type, coalesce(f.byte_size, a.byte_size)::bigint AS byte_size,
       coalesce(f.checksum, a.checksum) AS checksum
FROM file_rendition r
LEFT JOIN document_version_file f ON f.id = r.file_id
LEFT JOIN conversation_attachment a ON a.id = r.attachment_id
WHERE r.id = sqlc.arg(id) AND r.status = 'claimed' AND r.lease_id = sqlc.arg(lease_id);

-- name: LockRendition :one
-- A rendition, held for a call of the runtime's about it.
SELECT * FROM file_rendition WHERE id = $1 FOR UPDATE;

-- name: RenewRenditionLease :one
-- A claim held longer, from now; its own and nobody else's.
UPDATE file_rendition
SET claimed_until = sqlc.arg(claimed_until), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND status = 'claimed' AND lease_id = sqlc.arg(lease_id)
RETURNING claimed_until;

-- name: FinishRenditionDone :exec
-- The PDF: done, kept as it is from now on.
UPDATE file_rendition
SET status = 'done', storage_key = sqlc.arg(storage_key), byte_size = sqlc.arg(byte_size), checksum = sqlc.narg(checksum),
    page_count = sqlc.arg(page_count), produced_at = sqlc.arg(now), reason = NULL, lease_id = NULL, claimed_until = NULL,
    updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND status = 'claimed';

-- name: FinishRenditionUndone :exec
-- The runtime could not, or would not: failed or skipped, saying why.
UPDATE file_rendition
SET status = sqlc.arg(status), reason = sqlc.arg(reason), lease_id = NULL, claimed_until = NULL, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND status = 'claimed';

-- name: ReleaseRenditions :many
-- What a revoked credential had claimed, back in the queue for another,
-- that claim not counted. The courses come back, to wake whoever waits.
UPDATE file_rendition
SET status = 'queued', lease_id = NULL, claimed_until = NULL, attempts = greatest(attempts - 1, 0), updated_at = sqlc.arg(now)
WHERE status = 'claimed' AND claimed_by_credential_id = sqlc.arg(credential_id)
RETURNING course_id;

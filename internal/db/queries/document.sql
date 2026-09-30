-- name: InsertDocument :exec
INSERT INTO document (id, course_id, kind, title, submission_id, grade_id, sort_order, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: InsertDocumentVersion :exec
INSERT INTO document_version (id, document_id, seq, body_md, storage_key, content_type, byte_size, checksum, author_member_id, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: InsertDocumentVersionFile :exec
-- One file of a version, written with it (document_version_file_with_its_version).
INSERT INTO document_version_file (id, version_id, document_id, position, filename, storage_key, content_type, byte_size,
                                   checksum, created_at)
VALUES (sqlc.arg(id), sqlc.arg(version_id), sqlc.arg(document_id), sqlc.arg(position), sqlc.arg(filename),
        sqlc.arg(storage_key), sqlc.arg(content_type), sqlc.arg(byte_size), sqlc.narg(checksum), sqlc.arg(created_at));

-- name: ListVersionFiles :many
-- A version's files, in order.
SELECT * FROM document_version_file WHERE version_id = $1 ORDER BY position;

-- name: ListDocumentFiles :many
-- The files of every version of a document, each version's in order.
SELECT * FROM document_version_file WHERE document_id = $1 ORDER BY version_id, position;

-- name: GetDocumentFile :one
-- A file of one of a document's versions.
SELECT * FROM document_version_file WHERE id = $1 AND document_id = $2;

-- name: GetDocumentWithOwner :one
-- A document and, when it is owned, whose it is: a submitted file belongs to
-- its submission's student and assignment; a feedback file to its grade's.
SELECT d.id, d.course_id, d.kind, d.title, d.submission_id, d.grade_id, d.published_version_id,
       d.sort_order, d.status, d.created_at, d.purged_at, d.purged_by_actor_id, d.purge_reason,
       s.student_member_id  AS submission_student,
       s.assignment_id      AS submission_assignment,
       g.student_member_id  AS grade_student,
       gs.assignment_id     AS grade_assignment,
       g.posted_at          AS grade_posted_at,
       g.superseded_by      AS grade_superseded_by
FROM document d
LEFT JOIN submission s  ON s.id = d.submission_id
LEFT JOIN grade g       ON g.id = d.grade_id
LEFT JOIN submission gs ON gs.id = g.submission_id
WHERE d.id = $1 AND d.course_id = $2;

-- name: LockDocument :exec
-- Serialises version numbering: two writers must not both take seq n+1.
SELECT 1 FROM document WHERE id = $1 FOR UPDATE;

-- name: MaxVersionSeq :one
SELECT COALESCE(max(seq), 0)::int FROM document_version WHERE document_id = $1;

-- name: GetVersionOfDocument :one
SELECT * FROM document_version WHERE id = $1 AND document_id = $2;

-- name: GetLatestVersion :one
SELECT * FROM document_version WHERE document_id = $1 ORDER BY seq DESC LIMIT 1;

-- name: ListVersions :many
SELECT id, seq, (storage_key IS NOT NULL)::bool AS has_file, content_type, byte_size, author_member_id, created_at, purged_at
FROM document_version WHERE document_id = $1 ORDER BY seq;

-- name: SetPublishedVersion :exec
UPDATE document SET published_version_id = $2 WHERE id = $1;

-- name: SetDocumentStatus :execrows
UPDATE document SET status = $2 WHERE id = $1 AND status <> $2;

-- name: UpdateDocumentDetails :exec
UPDATE document SET title = $2, sort_order = $3 WHERE id = $1;

-- name: ListVersionsToPurge :many
-- The versions of a document not purged yet, and the files each holds, in
-- order.
SELECT v.id,
       coalesce(array_agg(f.storage_key ORDER BY f.position) FILTER (WHERE f.id IS NOT NULL), '{}')::text[] AS storage_keys
FROM document_version v
LEFT JOIN document_version_file f ON f.version_id = v.id
WHERE v.document_id = $1 AND v.purged_at IS NULL
GROUP BY v.id, v.seq
ORDER BY v.seq;

-- name: PurgeVersion :execrows
-- Its text, its file and the file's checksum go; the rest stays, with who,
-- when and why. The one change a version takes (document_version_guarded).
UPDATE document_version
SET body_md = NULL, storage_key = NULL, checksum = NULL,
    purged_at = sqlc.arg(purged_at), purged_by_actor_id = sqlc.arg(purged_by_actor_id), purge_reason = sqlc.arg(purge_reason)
WHERE id = sqlc.arg(id) AND purged_at IS NULL;

-- name: PurgeDocument :execrows
-- Archived for good, saying who purged it, when and why.
UPDATE document
SET status = 'archived', purged_at = sqlc.arg(purged_at), purged_by_actor_id = sqlc.arg(purged_by_actor_id),
    purge_reason = sqlc.arg(purge_reason)
WHERE id = sqlc.arg(id) AND purged_at IS NULL;

-- name: ListCourseDocuments :many
-- Course-level documents: material, instructions, rubrics. Owned documents
-- (submitted files, feedback) are reached through their owners instead.
-- Instructions and rubrics are the assignment's: to anyone who does not
-- write assignments they exist only once a published assignment within their
-- scope refers to them, or a student could read next week's exam by listing.
SELECT id, kind, title, published_version_id, sort_order, status, created_at, purged_at
FROM document d
WHERE d.course_id = $1 AND d.id > sqlc.arg(after)
  AND d.kind = ANY(sqlc.arg(kinds)::text[])
  AND (sqlc.arg(include_unpublished)::bool OR d.published_version_id IS NOT NULL)
  AND (sqlc.arg(include_archived)::bool OR d.status = 'active')
  AND (sqlc.arg(writes_assignments)::bool OR d.kind = 'material' OR EXISTS (
        SELECT 1 FROM assignment a
        WHERE (a.instructions_document_id = d.id OR a.rubric_document_id = d.id) AND a.published_at IS NOT NULL
          AND (sqlc.arg(assignment_all)::bool OR EXISTS (
                SELECT 1 FROM member_assignment_scope y WHERE y.member_id = sqlc.arg(member_id) AND y.assignment_id = a.id))
          AND (sqlc.arg(principal_assignment_all)::bool OR EXISTS (
                SELECT 1 FROM member_assignment_scope py WHERE py.member_id = sqlc.arg(principal_id) AND py.assignment_id = a.id))))
ORDER BY d.id
LIMIT sqlc.arg(max_rows);

-- name: DocumentInUseByPublishedAssignment :one
-- Whether a published assignment within the member's scope refers to the
-- document as its instructions or rubric. A delegate's scope is its
-- principal's too.
SELECT EXISTS (
    SELECT 1 FROM assignment a
    WHERE (a.instructions_document_id = sqlc.arg(document_id) OR a.rubric_document_id = sqlc.arg(document_id))
      AND a.published_at IS NOT NULL
      AND (sqlc.arg(assignment_all)::bool OR EXISTS (
            SELECT 1 FROM member_assignment_scope y WHERE y.member_id = sqlc.arg(member_id) AND y.assignment_id = a.id))
      AND (sqlc.arg(principal_assignment_all)::bool OR EXISTS (
            SELECT 1 FROM member_assignment_scope py WHERE py.member_id = sqlc.arg(principal_id) AND py.assignment_id = a.id))
);

-- name: ListPublishedAssignmentsUsingDocument :many
-- The published assignments that refer to the document as their instructions
-- or rubric: an event about the document is filed under each of them. KEY
-- SHARE waits for an assignment.unpublish under way (LockAssignmentForUnpublish),
-- after which the row is read again as it left it: an assignment unpublished
-- meanwhile is not listed, and the event goes out under its unreleased name.
SELECT a.id FROM assignment a
WHERE (a.instructions_document_id = sqlc.arg(document_id) OR a.rubric_document_id = sqlc.arg(document_id))
  AND a.published_at IS NOT NULL
ORDER BY a.id
FOR KEY SHARE;

-- name: ListSubmissionDocuments :many
SELECT id, title, status, created_at FROM document WHERE submission_id = $1 AND status = 'active' ORDER BY id;

-- name: ListGradeDocuments :many
SELECT id, title, status, created_at FROM document WHERE grade_id = $1 AND status = 'active' ORDER BY id;

-- name: VersionPinnedInScope :one
-- Is this version the one some submission within the member's scope was
-- submitted under? Then that member may read it even after the instructions
-- have moved on: it is what they, or their student, were told.
SELECT EXISTS (
    SELECT 1 FROM submission s
    WHERE s.instructions_version_id = sqlc.arg(version_id)
      AND (sqlc.arg(student_all)::bool OR EXISTS (
            SELECT 1 FROM member_student_scope x WHERE x.member_id = sqlc.arg(member_id) AND x.student_member_id = s.student_member_id))
      AND (sqlc.arg(assignment_all)::bool OR EXISTS (
            SELECT 1 FROM member_assignment_scope y WHERE y.member_id = sqlc.arg(member_id) AND y.assignment_id = s.assignment_id))
      AND (sqlc.arg(principal_student_all)::bool OR EXISTS (
            SELECT 1 FROM member_student_scope px WHERE px.member_id = sqlc.arg(principal_id) AND px.student_member_id = s.student_member_id))
      AND (sqlc.arg(principal_assignment_all)::bool OR EXISTS (
            SELECT 1 FROM member_assignment_scope py WHERE py.member_id = sqlc.arg(principal_id) AND py.assignment_id = s.assignment_id))
);

-- name: StorageKeyInUse :one
-- Whether a file has been attached: to a version of a document, as any of
-- its files or in its own columns (as the release before 0023 writes it),
-- or to a message of a conversation.
SELECT (EXISTS (SELECT 1 FROM document_version WHERE storage_key = sqlc.narg(storage_key)::text)
     OR EXISTS (SELECT 1 FROM document_version_file WHERE storage_key = sqlc.narg(storage_key)::text)
     OR EXISTS (SELECT 1 FROM conversation_attachment WHERE storage_key = sqlc.narg(storage_key)::text))::bool AS in_use;

-- name: LockStorageKey :exec
-- Serialises attaching one upload. Held until the transaction ends.
SELECT pg_advisory_xact_lock(hashtextextended('storage-key:' || sqlc.arg(storage_key)::text, 0));

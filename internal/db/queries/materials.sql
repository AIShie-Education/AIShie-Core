-- Search and the derived passage index of a course's material, instructions
-- and rubrics (migration 0033). Search text is made by memory.SearchText in
-- the application; PostgreSQL needs no extension and no locale-specific lexer.

-- name: DeleteMaterialDocumentPassages :execrows
DELETE FROM document_search_passages WHERE document_id = $1;

-- name: DeleteMaterialBodyPassages :exec
DELETE FROM document_search_passages WHERE version_id = $1 AND file_id IS NULL;

-- name: DeleteMaterialFilePassages :exec
DELETE FROM document_search_passages WHERE version_id = $1 AND file_id = $2;

-- name: InsertMaterialPassage :exec
INSERT INTO document_search_passages (course_id, document_id, version_id, file_id, text_source, text_revision,
                                      section_kind, section_no, part_no, content, search_text, updated_at)
VALUES (sqlc.arg(course_id), sqlc.arg(document_id), sqlc.arg(version_id), sqlc.narg(file_id), sqlc.arg(text_source),
        sqlc.arg(text_revision), sqlc.arg(section_kind), sqlc.arg(section_no), sqlc.arg(part_no), sqlc.arg(content),
        sqlc.arg(search_text), sqlc.arg(updated_at));

-- name: ListMaterialDocumentBodies :many
-- Every version's own text that can be indexed, including the published and
-- the latest; the search itself chooses the one the reader is shown.
SELECT v.id AS version_id, v.body_md
FROM document_version v
WHERE v.document_id = $1 AND v.purged_at IS NULL AND v.body_md IS NOT NULL AND btrim(v.body_md) <> ''
ORDER BY v.seq;

-- name: GetMaterialFileText :one
-- One file's current completed text, its source, revision and filename.
SELECT t.version_id, t.file_id, t.source, t.revision, t.body, f.filename
FROM document_version_text t
JOIN document_version_file f ON f.id = t.file_id AND f.version_id = t.version_id
JOIN document_version v ON v.id = t.version_id
WHERE t.version_id = sqlc.arg(version_id) AND t.file_id = sqlc.arg(file_id)
  AND t.status = 'done' AND t.source IN ('ai', 'staff') AND t.body IS NOT NULL AND v.purged_at IS NULL;

-- name: ListMaterialDocumentTexts :many
-- Every completed file text of every version of a document not purged.
SELECT t.version_id, t.file_id, t.source, t.revision, t.body, f.filename
FROM document_version_text t
JOIN document_version_file f ON f.id = t.file_id AND f.version_id = t.version_id
JOIN document_version v ON v.id = t.version_id
WHERE t.document_id = $1 AND t.status = 'done' AND t.source IN ('ai', 'staff') AND t.body IS NOT NULL
  AND v.purged_at IS NULL
ORDER BY v.seq, f.position;

-- name: ListMaterialBackfill :many
-- Units of indexing that are absent or still at an older text revision. One
-- unit is a version's own body, or one file's text. Whitespace-only bodies are
-- deliberately left out, so they are not rediscovered on every sweep. The
-- caller locks the document and reads the unit again before replacing it.
SELECT * FROM (
    SELECT d.course_id, d.id AS document_id, v.id AS version_id, NULL::uuid AS file_id,
           'body_md'::text AS text_source, 1::integer AS text_revision
    FROM document d
    JOIN document_version v ON v.document_id = d.id
    WHERE d.kind IN ('material', 'instructions', 'rubric') AND d.status = 'active' AND d.purged_at IS NULL
      AND v.purged_at IS NULL AND v.body_md IS NOT NULL AND btrim(v.body_md) <> ''
      AND NOT EXISTS (SELECT 1 FROM document_search_passages p
                      WHERE p.version_id = v.id AND p.file_id IS NULL AND p.text_source = 'body_md' AND p.text_revision = 1)
  UNION ALL
    SELECT d.course_id, d.id AS document_id, t.version_id, t.file_id, t.source, t.revision
    FROM document d
    JOIN document_version_text t ON t.document_id = d.id
    JOIN document_version v ON v.id = t.version_id
    WHERE d.kind IN ('material', 'instructions', 'rubric') AND d.status = 'active' AND d.purged_at IS NULL
      AND v.purged_at IS NULL AND t.status = 'done' AND t.source IN ('ai', 'staff') AND t.body IS NOT NULL
      AND NOT EXISTS (SELECT 1 FROM document_search_passages p
                      WHERE p.version_id = t.version_id AND p.file_id = t.file_id
                        AND p.text_source = t.source AND p.text_revision = t.revision)
) needed
ORDER BY document_id, version_id, file_id NULLS FIRST
LIMIT sqlc.arg(max_rows);

-- name: SearchMaterialPassages :many
-- The passages of exactly the versions document.get would show this seat.
-- The document and assignment filters mirror ListCourseDocuments; selecting
-- the latest version even when it is a purged tombstone mirrors
-- docReader.versionOf, rather than falling back to an older readable version.
WITH readable AS (
    SELECT d.id, d.kind, d.title, d.published_version_id, d.sort_order, d.created_at
    FROM document d
    WHERE d.course_id = sqlc.arg(course_id) AND d.status = 'active' AND d.purged_at IS NULL
      AND d.kind = ANY(sqlc.arg(kinds)::text[])
      AND (sqlc.arg(read_drafts)::bool OR d.published_version_id IS NOT NULL)
      AND (sqlc.arg(writes_assignments)::bool OR d.kind = 'material' OR EXISTS (
            SELECT 1 FROM assignment a
            WHERE (a.instructions_document_id = d.id OR a.rubric_document_id = d.id) AND a.published_at IS NOT NULL
              AND (sqlc.arg(assignment_all)::bool OR EXISTS (
                    SELECT 1 FROM member_assignment_scope y
                    WHERE y.member_id = sqlc.arg(member_id) AND y.assignment_id = a.id))
              AND (sqlc.arg(principal_assignment_all)::bool OR EXISTS (
                    SELECT 1 FROM member_assignment_scope py
                    WHERE py.member_id = sqlc.arg(principal_id) AND py.assignment_id = a.id))))
), selected AS (
    SELECT r.id, r.kind, r.title, r.sort_order, r.created_at, v.id AS version_id, v.purged_at
    FROM readable r
    JOIN LATERAL (
        SELECT v.id, v.purged_at, v.seq
        FROM document_version v
        WHERE v.document_id = r.id AND (sqlc.arg(read_drafts)::bool OR v.id = r.published_version_id)
        ORDER BY v.seq DESC
        LIMIT 1
    ) v ON true
)
SELECT s.id AS document_id, s.kind, s.title, s.version_id, p.file_id, f.filename,
       p.text_source, p.section_kind, p.section_no, p.part_no, p.content
FROM selected s
JOIN document_search_passages p ON p.course_id = sqlc.arg(course_id) AND p.document_id = s.id AND p.version_id = s.version_id
LEFT JOIN document_version_file f ON f.id = p.file_id AND f.version_id = p.version_id
CROSS JOIN (SELECT to_tsquery('simple', sqlc.arg(query)::text) AS value) q
WHERE s.purged_at IS NULL AND p.search @@ q.value
ORDER BY ts_rank_cd(p.search, q.value) DESC, s.sort_order, s.created_at, s.id,
         f.position NULLS FIRST, p.part_no, p.id
LIMIT sqlc.arg(max_rows) OFFSET sqlc.arg(offset_rows);

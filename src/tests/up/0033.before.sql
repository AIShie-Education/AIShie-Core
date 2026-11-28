-- AIshie Core — before 0033_document_search_passages.up.sql, in make db-test-sql
-- Existing body and file text in NUR101: migration 0033 creates their derived
-- index's schema; the jobs runner, not a locking migration, backfills content.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;
INSERT INTO document (id, course_id, kind, title)
VALUES ('00000000-0000-0000-0033-0000000000e1', '00000000-0000-0000-0018-000000000041',
        'material', 'Migration search');
INSERT INTO document_version (id, document_id, seq, body_md, author_member_id)
VALUES ('00000000-0000-0000-0033-0000000000f1', '00000000-0000-0000-0033-0000000000e1', 1,
        'Migration body', '00000000-0000-0000-0018-000000000051');
INSERT INTO document_version (id, document_id, seq, body_md, author_member_id, created_at)
VALUES ('00000000-0000-0000-0033-0000000000f2', '00000000-0000-0000-0033-0000000000e1', 2,
        NULL, '00000000-0000-0000-0018-000000000051', '2026-01-01T00:00:00Z');
INSERT INTO document_version_file (id, version_id, document_id, position, filename, storage_key, content_type, byte_size, created_at)
VALUES ('00000000-0000-0000-0033-0000000000d1', '00000000-0000-0000-0033-0000000000f2',
        '00000000-0000-0000-0033-0000000000e1', 1, 'slides.pdf', 'tests/0033-slides.pdf', 'application/pdf', 100,
        '2026-01-01T00:00:00Z');
-- Recording the file queued its text; complete that existing queue row.
UPDATE document_version_text
SET status = 'done', body = 'Migration slide', source = 'ai', revision = 4, model = 'A model', produced_at = now()
WHERE version_id = '00000000-0000-0000-0033-0000000000f2'
  AND file_id = '00000000-0000-0000-0033-0000000000d1';
UPDATE document SET published_version_id = '00000000-0000-0000-0033-0000000000f1'
WHERE id = '00000000-0000-0000-0033-0000000000e1';
COMMIT;

-- AIshie Core — before 0023_document_version_files.up.sql, in `make db-test-sql`
--
-- What the migration finds, beside the documents of tests/up/0020: a
-- handout whose title is no file's name, a slash and a tab in it, uploaded
-- as a Word file whose type says more than its type; and the rubric's text,
-- written by Lin. Committed, so that the migration runs over it; it stays,
-- for the redo and the down after it (tests/down/0023.*), and the downs
-- after that drop it with everything else.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

-- e1 the handout, and f1 its one version
INSERT INTO document (id, course_id, kind, title, status)
VALUES ('00000000-0000-0000-0022-0000000000e1', '00000000-0000-0000-0018-000000000041', 'material', E'Week 2/3\thandout', 'active');
INSERT INTO document_version (id, document_id, seq, storage_key, content_type, byte_size, checksum, author_member_id, created_at)
VALUES ('00000000-0000-0000-0022-0000000000f1', '00000000-0000-0000-0022-0000000000e1', 1, 'up22/f1',
        'application/vnd.openxmlformats-officedocument.wordprocessingml.document; charset=binary', 20, 'sha256:22',
        '00000000-0000-0000-0018-000000000051', '2026-09-05 09:00:00+00');
UPDATE document_version_text
SET status = 'done', body = '## Criteria', source = 'staff', edited_by_member_id = '00000000-0000-0000-0018-000000000051',
    edited_at = now(), revision = 2
WHERE version_id = '00000000-0000-0000-0020-0000000000f4';

COMMIT;

-- AIshie Core — before 0023_document_version_files.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with: a version of Week 1 of
-- tests/up/0020 holding three files, the second transcribed; and a version
-- written as the release before writes one, its file in its own columns
-- alone, which the database records as its one file. Committed, so that the
-- down migration runs over it.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

-- v1 Week 1's fourth version, and d1..d3 its files · v2 its fifth, as the release before writes it
INSERT INTO document_version (id, document_id, seq, body_md, storage_key, content_type, byte_size, checksum, author_member_id,
                              created_at)
VALUES ('00000000-0000-0000-0022-0000000000a1', '00000000-0000-0000-0020-0000000000e1', 4, 'Read the handout first.',
        'documents/down22/d1', 'application/pdf', 100, 'sha256:d1', '00000000-0000-0000-0018-000000000051',
        '2026-09-20 09:00:00+00');
INSERT INTO document_version_file (id, version_id, document_id, position, filename, storage_key, content_type, byte_size,
                                   checksum, created_at) VALUES
    ('00000000-0000-0000-0022-0000000000d1', '00000000-0000-0000-0022-0000000000a1', '00000000-0000-0000-0020-0000000000e1', 1,
     'slides.pdf', 'documents/down22/d1', 'application/pdf', 100, 'sha256:d1', '2026-09-20 09:00:00+00'),
    ('00000000-0000-0000-0022-0000000000d2', '00000000-0000-0000-0022-0000000000a1', '00000000-0000-0000-0020-0000000000e1', 2,
     'handout.docx', 'documents/down22/d2', 'application/msword', 200, 'sha256:d2', '2026-09-20 09:00:00+00'),
    ('00000000-0000-0000-0022-0000000000d3', '00000000-0000-0000-0022-0000000000a1', '00000000-0000-0000-0020-0000000000e1', 3,
     'loops.py', 'documents/down22/d3', 'text/x-python', 300, NULL, '2026-09-20 09:00:00+00');
UPDATE document_version_text
SET status = 'done', body = '## The handout', source = 'ai', model = 'A model', produced_at = now(), pages = 1, revision = 2
WHERE version_id = '00000000-0000-0000-0022-0000000000a1' AND file_id = '00000000-0000-0000-0022-0000000000d2';
INSERT INTO document_version (id, document_id, seq, storage_key, content_type, byte_size, author_member_id, created_at)
VALUES ('00000000-0000-0000-0022-0000000000a2', '00000000-0000-0000-0020-0000000000e1', 5, 'courses/down22/a2',
        'application/pdf', 10, '00000000-0000-0000-0018-000000000051', '2026-09-21 09:00:00+00');

COMMIT;

DO $chk$
BEGIN
    IF (SELECT count(*) FROM document_version_text WHERE version_id = '00000000-0000-0000-0022-0000000000a1') <> 3
       OR NOT EXISTS (SELECT 1 FROM document_version_file f JOIN document_version_text t ON t.file_id = f.id
                      WHERE f.version_id = '00000000-0000-0000-0022-0000000000a2' AND f.position = 1
                        AND f.filename = 'Week 1.pdf' AND f.storage_key = 'courses/down22/a2') THEN
        RAISE EXCEPTION 'FAIL  0023 down: the files were not queued for their texts, each';
    END IF;
END $chk$;

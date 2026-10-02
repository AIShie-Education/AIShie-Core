-- AIshie Core — before 0027_drop_deprecated.up.sql, in `make db-test-sql`
--
-- What the migration finds, beside the agents of tests/up/0025, whose
-- runtime agents' site chat credentials name their runtime tokens: Week 4,
-- written as the release before writes a version, its own columns naming
-- its first file, of two, the second's text done by its file; and a
-- version of it the release before purged, which still says what type and
-- size its file was. Committed, so that the migration runs over it; it
-- stays, for the redo and the down after it (tests/down/0027.*), and the
-- downs after that drop it with everything else.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

-- e1 Week 4 · f1 its version of slides and a program, d1 and d2 · f2 its second, d3 its file, purged below
INSERT INTO document (id, course_id, kind, title, status)
VALUES ('00000000-0000-0000-0027-0000000000e1', '00000000-0000-0000-0018-000000000041', 'material', 'Week 4', 'active');
INSERT INTO document_version (id, document_id, seq, body_md, storage_key, content_type, byte_size, checksum, author_member_id,
                              created_at) VALUES
    ('00000000-0000-0000-0027-0000000000f1', '00000000-0000-0000-0027-0000000000e1', 1, 'Slides, and the program to run.',
     'documents/up27/d1', 'application/pdf', 100, 'sha256:27d1', '00000000-0000-0000-0018-000000000051', '2026-09-28 09:00:00+00'),
    ('00000000-0000-0000-0027-0000000000f2', '00000000-0000-0000-0027-0000000000e1', 2, NULL,
     'documents/up27/d3', 'application/pdf', 50, 'sha256:27d3', '00000000-0000-0000-0018-000000000051', '2026-09-29 09:00:00+00');
INSERT INTO document_version_file (id, version_id, document_id, position, filename, storage_key, content_type, byte_size,
                                   checksum, created_at) VALUES
    ('00000000-0000-0000-0027-0000000000d1', '00000000-0000-0000-0027-0000000000f1', '00000000-0000-0000-0027-0000000000e1', 1,
     'slides.pdf', 'documents/up27/d1', 'application/pdf', 100, 'sha256:27d1', '2026-09-28 09:00:00+00'),
    ('00000000-0000-0000-0027-0000000000d2', '00000000-0000-0000-0027-0000000000f1', '00000000-0000-0000-0027-0000000000e1', 2,
     'loops.py', 'documents/up27/d2', 'text/x-python', 30, NULL, '2026-09-28 09:00:00+00'),
    ('00000000-0000-0000-0027-0000000000d3', '00000000-0000-0000-0027-0000000000f2', '00000000-0000-0000-0027-0000000000e1', 1,
     'class list.pdf', 'documents/up27/d3', 'application/pdf', 50, 'sha256:27d3', '2026-09-29 09:00:00+00');

COMMIT;
BEGIN;

UPDATE document_version_text
SET status = 'done', body = '## loops.py', source = 'ai', model = 'A model', produced_at = now(), pages = 1, revision = 2
WHERE version_id = '00000000-0000-0000-0027-0000000000f1' AND file_id = '00000000-0000-0000-0027-0000000000d2';
UPDATE document_version
SET storage_key = NULL, checksum = NULL, purged_at = now(), purged_by_actor_id = '00000000-0000-0000-0018-000000000031',
    purge_reason = 'The class list was attached by mistake.'
WHERE id = '00000000-0000-0000-0027-0000000000f2';

COMMIT;

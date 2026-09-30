-- AIshie Core — before 0020_document_text.up.sql, in `make db-test-sql`
--
-- What the migration finds, in the course of tests/up/0018 and another,
-- archived: slides with three versions, each a file, the second published
-- and the third a draft; a published rubric; archived slides; a document
-- whose file was purged and replaced by text; a submitted file; notes of
-- text alone; and, in the archived course, published slides. Committed, so
-- that the migration runs over it; it stays, for the redo and the down after
-- it (tests/down/0020.*), and the downs after that drop it with everything
-- else.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

-- 42 NUR102, archived, and 55 Lin's seat there
INSERT INTO course (id, dept_id, term_id, code, section, title, status, created_by_actor_id)
VALUES ('00000000-0000-0000-0020-000000000042', '00000000-0000-0000-0018-000000000021', '00000000-0000-0000-0018-000000000011',
        'NUR102', 'A', 'Last year', 'archived', '00000000-0000-0000-0018-000000000031');
INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope)
VALUES ('00000000-0000-0000-0020-000000000055', '00000000-0000-0000-0020-000000000042', '00000000-0000-0000-0018-000000000031',
        'instructor', '00000000-0000-0000-0018-000000000031', 'all', 'all');
-- 71 an assignment of NUR101's, and a1 Wei's draft of it
INSERT INTO assignment (id, course_id, title, points_possible, published_at)
VALUES ('00000000-0000-0000-0020-000000000071', '00000000-0000-0000-0018-000000000041', 'Care plan', 10, now());
INSERT INTO submission (id, assignment_id, course_id, student_member_id, body)
VALUES ('00000000-0000-0000-0020-0000000000a1', '00000000-0000-0000-0020-000000000071', '00000000-0000-0000-0018-000000000041',
        '00000000-0000-0000-0018-000000000053', 'my plan');

-- e1 Week 1 · e2 the rubric · e3 archived slides · e4 a mistake · e5 Wei's file · e6 notes · e7 NUR102's slides
INSERT INTO document (id, course_id, kind, title, status, submission_id) VALUES
    ('00000000-0000-0000-0020-0000000000e1', '00000000-0000-0000-0018-000000000041', 'material', 'Week 1', 'active', NULL),
    ('00000000-0000-0000-0020-0000000000e2', '00000000-0000-0000-0018-000000000041', 'rubric', 'Rubric', 'active', NULL),
    ('00000000-0000-0000-0020-0000000000e3', '00000000-0000-0000-0018-000000000041', 'material', 'Old slides', 'archived', NULL),
    ('00000000-0000-0000-0020-0000000000e4', '00000000-0000-0000-0018-000000000041', 'material', 'Mistake', 'active', NULL),
    ('00000000-0000-0000-0020-0000000000e5', '00000000-0000-0000-0018-000000000041', 'submission', 'plan.pdf', 'active',
     '00000000-0000-0000-0020-0000000000a1'),
    ('00000000-0000-0000-0020-0000000000e6', '00000000-0000-0000-0018-000000000041', 'material', 'Notes', 'active', NULL),
    ('00000000-0000-0000-0020-0000000000e7', '00000000-0000-0000-0020-000000000042', 'material', 'Week 1', 'active', NULL);
-- f1..f3 Week 1's, a day apart · f4 the rubric's · f5 the archived slides' · f6 the mistake, purged, and f7
-- the text in its place · f8 Wei's file · f9 the notes · fa NUR102's slides
INSERT INTO document_version (id, document_id, seq, body_md, storage_key, content_type, byte_size, author_member_id, created_at) VALUES
    ('00000000-0000-0000-0020-0000000000f1', '00000000-0000-0000-0020-0000000000e1', 1, NULL, 'up20/f1', 'application/pdf', 10,
     '00000000-0000-0000-0018-000000000051', '2026-09-01 09:00:00+00'),
    ('00000000-0000-0000-0020-0000000000f2', '00000000-0000-0000-0020-0000000000e1', 2, NULL, 'up20/f2', 'application/pdf', 10,
     '00000000-0000-0000-0018-000000000051', '2026-09-02 09:00:00+00'),
    ('00000000-0000-0000-0020-0000000000f3', '00000000-0000-0000-0020-0000000000e1', 3, NULL, 'up20/f3', 'application/pdf', 10,
     '00000000-0000-0000-0018-000000000051', '2026-09-03 09:00:00+00'),
    ('00000000-0000-0000-0020-0000000000f4', '00000000-0000-0000-0020-0000000000e2', 1, NULL, 'up20/f4', 'application/pdf', 10,
     '00000000-0000-0000-0018-000000000051', '2026-08-01 09:00:00+00'),
    ('00000000-0000-0000-0020-0000000000f5', '00000000-0000-0000-0020-0000000000e3', 1, NULL, 'up20/f5', 'application/pdf', 10,
     '00000000-0000-0000-0018-000000000051', '2026-08-01 09:00:00+00'),
    ('00000000-0000-0000-0020-0000000000f6', '00000000-0000-0000-0020-0000000000e4', 1, NULL, 'up20/f6', 'application/pdf', 10,
     '00000000-0000-0000-0018-000000000051', '2026-08-01 09:00:00+00'),
    ('00000000-0000-0000-0020-0000000000f7', '00000000-0000-0000-0020-0000000000e4', 2, 'What it should have said', NULL, NULL, NULL,
     '00000000-0000-0000-0018-000000000051', '2026-08-02 09:00:00+00'),
    ('00000000-0000-0000-0020-0000000000f8', '00000000-0000-0000-0020-0000000000e5', 1, NULL, 'up20/f8', 'application/pdf', 10,
     '00000000-0000-0000-0018-000000000053', '2026-09-04 09:00:00+00'),
    ('00000000-0000-0000-0020-0000000000f9', '00000000-0000-0000-0020-0000000000e6', 1, 'Notes', NULL, NULL, NULL,
     '00000000-0000-0000-0018-000000000051', '2026-09-04 09:00:00+00'),
    ('00000000-0000-0000-0020-0000000000fa', '00000000-0000-0000-0020-0000000000e7', 1, NULL, 'up20/fa', 'application/pdf', 10,
     '00000000-0000-0000-0020-000000000055', '2025-09-01 09:00:00+00');
UPDATE document_version SET storage_key = NULL, purged_at = now(), purged_by_actor_id = '00000000-0000-0000-0018-000000000031',
                            purge_reason = 'the wrong file'
WHERE id = '00000000-0000-0000-0020-0000000000f6';
UPDATE document SET published_version_id = '00000000-0000-0000-0020-0000000000f2' WHERE id = '00000000-0000-0000-0020-0000000000e1';
UPDATE document SET published_version_id = '00000000-0000-0000-0020-0000000000f4' WHERE id = '00000000-0000-0000-0020-0000000000e2';
UPDATE document SET published_version_id = '00000000-0000-0000-0020-0000000000f5' WHERE id = '00000000-0000-0000-0020-0000000000e3';
UPDATE document SET published_version_id = '00000000-0000-0000-0020-0000000000f7' WHERE id = '00000000-0000-0000-0020-0000000000e4';
UPDATE document SET published_version_id = '00000000-0000-0000-0020-0000000000f8' WHERE id = '00000000-0000-0000-0020-0000000000e5';
UPDATE document SET published_version_id = '00000000-0000-0000-0020-0000000000f9' WHERE id = '00000000-0000-0000-0020-0000000000e6';
UPDATE document SET published_version_id = '00000000-0000-0000-0020-0000000000fa' WHERE id = '00000000-0000-0000-0020-0000000000e7';

COMMIT;

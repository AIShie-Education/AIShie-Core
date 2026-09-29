-- AIshie Core — before 0015_flexible_records.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with: a course total overridden, and
-- lecture notes with one version purged and one left as it was. Committed,
-- so that the down migration runs over it; the downs after it drop the rest
-- with everything else.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

INSERT INTO term (id, name, starts_on, ends_on)
VALUES ('00000000-0000-0000-0015-000000000011', '2027 Summer', '2027-06-01', '2027-08-20');
INSERT INTO department (id, name) VALUES ('00000000-0000-0000-0015-000000000021', 'Physics');
INSERT INTO actor (id, kind, display_name, platform_role, created_by_actor_id) VALUES
    ('00000000-0000-0000-0015-000000000032', 'human', 'Admin', 'admin', NULL),
    ('00000000-0000-0000-0015-000000000034', 'human', 'Sato',  NULL,    NULL),
    ('00000000-0000-0000-0015-000000000035', 'human', 'Yuki',  NULL,    NULL);
INSERT INTO course (id, dept_id, term_id, code, section, title, status, created_by_actor_id)
VALUES ('00000000-0000-0000-0015-000000000041', '00000000-0000-0000-0015-000000000021', '00000000-0000-0000-0015-000000000011',
        'PHYS101', 'A', 'Mechanics', 'active', '00000000-0000-0000-0015-000000000032');
-- 51 Sato · 52 Yuki
INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope) VALUES
    ('00000000-0000-0000-0015-000000000051', '00000000-0000-0000-0015-000000000041', '00000000-0000-0000-0015-000000000034',
     'instructor', '00000000-0000-0000-0015-000000000032', 'all', 'all'),
    ('00000000-0000-0000-0015-000000000052', '00000000-0000-0000-0015-000000000041', '00000000-0000-0000-0015-000000000035',
     'student', '00000000-0000-0000-0015-000000000034', 'listed', 'all');
INSERT INTO grade_component (id, course_id, name) VALUES
    ('00000000-0000-0000-0015-000000000061', '00000000-0000-0000-0015-000000000041', 'Total');
-- b1 the post that wrote the total · b2 the override
INSERT INTO action (id, actor_id, course_id, member_id, action_type, target_type, payload_hash, idempotency_key,
                    authz_result, status, executed_at) VALUES
    ('00000000-0000-0000-0015-0000000000b1', '00000000-0000-0000-0015-000000000034', '00000000-0000-0000-0015-000000000041',
     '00000000-0000-0000-0015-000000000051', 'grade.post', 'grade', repeat('0', 64), 'k-post', 'autonomous', 'executed', now()),
    ('00000000-0000-0000-0015-0000000000b2', '00000000-0000-0000-0015-000000000034', '00000000-0000-0000-0015-000000000041',
     '00000000-0000-0000-0015-000000000051', 'grade.override_total', 'grade', repeat('0', 64), 'k-override', 'autonomous', 'executed', now());
-- d1 the total as posted, superseded by d2, the same with an override
INSERT INTO grade (id, student_member_id, component_id, origin, score, grader_member_id, created_by_action_id,
                   posted_at, posted_by_member_id, superseded_by) VALUES
    ('00000000-0000-0000-0015-0000000000d1', '00000000-0000-0000-0015-000000000052', '00000000-0000-0000-0015-000000000061',
     'computed', 89.5, '00000000-0000-0000-0015-000000000051', '00000000-0000-0000-0015-0000000000b1', now(),
     '00000000-0000-0000-0015-000000000051', '00000000-0000-0000-0015-0000000000d2');
INSERT INTO grade (id, student_member_id, component_id, origin, score, grader_member_id, created_by_action_id,
                   posted_at, posted_by_member_id, override_score, override_reason, override_by_member_id, overridden_at) VALUES
    ('00000000-0000-0000-0015-0000000000d2', '00000000-0000-0000-0015-000000000052', '00000000-0000-0000-0015-000000000061',
     'computed', 89.5, '00000000-0000-0000-0015-000000000051', '00000000-0000-0000-0015-0000000000b2', now(),
     '00000000-0000-0000-0015-000000000051', 90, 'Borderline; the lab work was strong.',
     '00000000-0000-0000-0015-000000000051', now());
-- e1 lecture notes · f1 a version purged · f2 one kept
INSERT INTO document (id, course_id, kind, title) VALUES
    ('00000000-0000-0000-0015-0000000000e1', '00000000-0000-0000-0015-000000000041', 'material', 'Lecture 1');
INSERT INTO document_version (id, document_id, seq, storage_key, content_type, byte_size, checksum, author_member_id) VALUES
    ('00000000-0000-0000-0015-0000000000f1', '00000000-0000-0000-0015-0000000000e1', 1, 'courses/down0015/f1',
     'application/pdf', 2048, 'sha256:0015', '00000000-0000-0000-0015-000000000051');
INSERT INTO document_version (id, document_id, seq, body_md, author_member_id) VALUES
    ('00000000-0000-0000-0015-0000000000f2', '00000000-0000-0000-0015-0000000000e1', 2, 'The notes, without the class list.',
     '00000000-0000-0000-0015-000000000051');
UPDATE document SET published_version_id = '00000000-0000-0000-0015-0000000000f2' WHERE id = '00000000-0000-0000-0015-0000000000e1';
UPDATE document_version
   SET storage_key = NULL, checksum = NULL, purged_at = now(),
       purged_by_actor_id = '00000000-0000-0000-0015-000000000032', purge_reason = 'The class list was attached by mistake.'
 WHERE id = '00000000-0000-0000-0015-0000000000f1';

COMMIT;

-- AIshie Core — before 0012_join_links.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with: a course with two join links,
-- one live and one revoked; a person who registered through the live one,
-- with an email nobody has checked and a password, seated through it; and an
-- existing student who joined through it. Committed, so that the down
-- migration runs over it; the downs after it drop it with everything else.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

INSERT INTO term (id, name, starts_on, ends_on)
VALUES ('00000000-0000-0000-0012-000000000011', '2027 Spring', '2027-01-10', '2027-05-20');
INSERT INTO department (id, name) VALUES ('00000000-0000-0000-0012-000000000021', 'Physics');
-- 31 Sato, the instructor · 35 Yuki, registered through the link · 37 Ken, a student already
INSERT INTO actor (id, kind, display_name, email, created_by_actor_id) VALUES
    ('00000000-0000-0000-0012-000000000031', 'human', 'Sato', 'sato@example.edu', NULL),
    ('00000000-0000-0000-0012-000000000037', 'human', 'Ken',  'ken@example.edu',  NULL);
INSERT INTO actor (id, kind, display_name, email, email_verified, created_by_actor_id) VALUES
    ('00000000-0000-0000-0012-000000000035', 'human', 'Yuki', 'yuki@example.edu', false, '00000000-0000-0000-0012-000000000031');
INSERT INTO credential (id, actor_id, kind, secret_hash) VALUES
    ('00000000-0000-0000-0012-0000000000c1', '00000000-0000-0000-0012-000000000035', 'password', '$argon2id$stand-in');
INSERT INTO course (id, dept_id, term_id, code, section, title, status, created_by_actor_id)
VALUES ('00000000-0000-0000-0012-000000000041', '00000000-0000-0000-0012-000000000021', '00000000-0000-0000-0012-000000000011',
        'PHYS101', 'A', 'Mechanics', 'active', '00000000-0000-0000-0012-000000000031');
INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope, perm_member_manage)
VALUES ('00000000-0000-0000-0012-000000000051', '00000000-0000-0000-0012-000000000041', '00000000-0000-0000-0012-000000000031',
        'instructor', '00000000-0000-0000-0012-000000000031', 'all', 'all', 'autonomous');
-- l1 live · l2 revoked by Sato
INSERT INTO course_join_link (id, course_id, token_prefix, secret_hash, preset_id, created_by_member_id, expires_at, max_uses, uses)
SELECT '00000000-0000-0000-0012-0000000001a1', '00000000-0000-0000-0012-000000000041', 'down0012live', 'sha256:' || repeat('a', 64),
       p.id, '00000000-0000-0000-0012-000000000051', now() + interval '10 minutes', 30, 2
FROM permission_preset p WHERE p.name = 'student' AND p.dept_id IS NULL;
INSERT INTO course_join_link (id, course_id, token_prefix, secret_hash, preset_id, created_by_member_id, expires_at,
                              allowed_email_domains, revoked_at, revoked_by_member_id)
SELECT '00000000-0000-0000-0012-0000000001a2', '00000000-0000-0000-0012-000000000041', 'down0012gone', 'sha256:' || repeat('b', 64),
       p.id, '00000000-0000-0000-0012-000000000051', now() + interval '10 minutes', '{example.edu}', now(),
       '00000000-0000-0000-0012-000000000051'
FROM permission_preset p WHERE p.name = 'student' AND p.dept_id IS NULL;
-- 52 Yuki's seat · 53 Ken's, both through l1, added by the link's maker
INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope, join_link_id) VALUES
    ('00000000-0000-0000-0012-000000000052', '00000000-0000-0000-0012-000000000041', '00000000-0000-0000-0012-000000000035',
     'student', '00000000-0000-0000-0012-000000000031', 'listed', 'all', '00000000-0000-0000-0012-0000000001a1'),
    ('00000000-0000-0000-0012-000000000053', '00000000-0000-0000-0012-000000000041', '00000000-0000-0000-0012-000000000037',
     'student', '00000000-0000-0000-0012-000000000031', 'listed', 'all', '00000000-0000-0000-0012-0000000001a1');
INSERT INTO member_student_scope (member_id, student_member_id) VALUES
    ('00000000-0000-0000-0012-000000000052', '00000000-0000-0000-0012-000000000052'),
    ('00000000-0000-0000-0012-000000000053', '00000000-0000-0000-0012-000000000053');

COMMIT;

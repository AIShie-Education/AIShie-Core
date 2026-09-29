-- AIshie Core — before 0013_member_invite.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with: a course whose instructor hands
-- out join links (member_invite), with a link made, and a department's own
-- preset that carries it. Committed, so that the down migration runs over
-- it; the downs after it drop it with everything else.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

INSERT INTO term (id, name, starts_on, ends_on)
VALUES ('00000000-0000-0000-0013-000000000011', '2027 Spring', '2027-01-10', '2027-05-20');
INSERT INTO department (id, name) VALUES ('00000000-0000-0000-0013-000000000021', 'Chemistry');
-- 31 Mori, the instructor
INSERT INTO actor (id, kind, display_name, email) VALUES
    ('00000000-0000-0000-0013-000000000031', 'human', 'Mori', 'mori@example.edu');
INSERT INTO course (id, dept_id, term_id, code, section, title, status, created_by_actor_id)
VALUES ('00000000-0000-0000-0013-000000000041', '00000000-0000-0000-0013-000000000021', '00000000-0000-0000-0013-000000000011',
        'CHEM101', 'A', 'Chemistry', 'active', '00000000-0000-0000-0013-000000000031');
-- 91 the department's head TA, who hands out links
INSERT INTO permission_preset (id, dept_id, name, role, student_scope, assignment_scope, perm_member_manage, perm_member_invite)
VALUES ('00000000-0000-0000-0013-000000000091', '00000000-0000-0000-0013-000000000021', 'head_ta', 'ta', 'all', 'all',
        'autonomous', 'autonomous');
INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope,
                           perm_member_manage, perm_member_invite)
VALUES ('00000000-0000-0000-0013-000000000051', '00000000-0000-0000-0013-000000000041', '00000000-0000-0000-0013-000000000031',
        'instructor', '00000000-0000-0000-0013-000000000031', 'all', 'all', 'autonomous', 'autonomous');
INSERT INTO course_join_link (id, course_id, token_prefix, secret_hash, preset_id, created_by_member_id, expires_at)
SELECT '00000000-0000-0000-0013-0000000001a1', '00000000-0000-0000-0013-000000000041', 'down0013live', 'sha256:' || repeat('c', 64),
       p.id, '00000000-0000-0000-0013-000000000051', now() + interval '10 minutes'
FROM permission_preset p WHERE p.name = 'student' AND p.dept_id IS NULL;

COMMIT;

-- AIshie Core — before 0016_login_ids.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with: a course whose instructor has a
-- login ID and an email; a student who registered through a join link with
-- a login ID of their own and no email, whose password the instructor has
-- reset to a temporary one, which the student has signed in with; and a
-- student with both, given by an administrator. Committed, so that the down
-- migration runs over it; the downs after it drop it with everything else.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

INSERT INTO term (id, name, starts_on, ends_on)
VALUES ('00000000-0000-0000-0016-000000000011', '2027 Spring', '2027-01-10', '2027-05-20');
INSERT INTO department (id, name) VALUES ('00000000-0000-0000-0016-000000000021', 'Tropical Agriculture');
-- 31 Lin, the instructor · 35 Wei, a login ID and no email · 37 Fang, both
INSERT INTO actor (id, kind, display_name, email, login_id, created_by_actor_id) VALUES
    ('00000000-0000-0000-0016-000000000031', 'human', 'Lin',  'lin@campus.example.edu',  'T19880042', NULL),
    ('00000000-0000-0000-0016-000000000037', 'human', 'Fang', 'fang@campus.example.edu', '20230002',  NULL);
INSERT INTO actor (id, kind, display_name, login_id, login_id_verified, created_by_actor_id) VALUES
    ('00000000-0000-0000-0016-000000000035', 'human', 'Wei', '20230001', false, '00000000-0000-0000-0016-000000000031');
-- c1 Wei's own password, revoked by the reset · c2 the temporary one Lin set · c3 the session Wei signed in with
INSERT INTO credential (id, actor_id, kind, secret_hash, revoked_at) VALUES
    ('00000000-0000-0000-0016-0000000000c1', '00000000-0000-0000-0016-000000000035', 'password', '$argon2id$stand-in', now());
INSERT INTO credential (id, actor_id, kind, secret_hash, label, issued_by_actor_id, must_change) VALUES
    ('00000000-0000-0000-0016-0000000000c2', '00000000-0000-0000-0016-000000000035', 'password', '$argon2id$stand-in-too',
     'temporary password set by Lin', '00000000-0000-0000-0016-000000000031', true);
INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, expires_at) VALUES
    ('00000000-0000-0000-0016-0000000000c3', '00000000-0000-0000-0016-000000000035', 'session', 'h', 'down0016sess',
     now() + interval '12 hours');
INSERT INTO course (id, dept_id, term_id, code, section, title, status, created_by_actor_id)
VALUES ('00000000-0000-0000-0016-000000000041', '00000000-0000-0000-0016-000000000021', '00000000-0000-0000-0016-000000000011',
        'AGR101', 'A', 'Tropical Crops', 'active', '00000000-0000-0000-0016-000000000031');
INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope, perm_member_manage)
VALUES ('00000000-0000-0000-0016-000000000051', '00000000-0000-0000-0016-000000000041', '00000000-0000-0000-0016-000000000031',
        'instructor', '00000000-0000-0000-0016-000000000031', 'all', 'all', 'autonomous');
INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope) VALUES
    ('00000000-0000-0000-0016-000000000052', '00000000-0000-0000-0016-000000000041', '00000000-0000-0000-0016-000000000035',
     'student', '00000000-0000-0000-0016-000000000031', 'listed', 'all'),
    ('00000000-0000-0000-0016-000000000053', '00000000-0000-0000-0016-000000000041', '00000000-0000-0000-0016-000000000037',
     'student', '00000000-0000-0000-0016-000000000031', 'listed', 'all');
INSERT INTO member_student_scope (member_id, student_member_id) VALUES
    ('00000000-0000-0000-0016-000000000052', '00000000-0000-0000-0016-000000000052'),
    ('00000000-0000-0000-0016-000000000053', '00000000-0000-0000-0016-000000000053');

COMMIT;

-- AIshie Core — before 0014_agent_owner_and_decisions.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with: an agent a person owns, seated
-- as their delegate, and one registered with no owner, seated on its own,
-- neither of which may change hands, nor decide but by proposal, while 0014
-- is in; and a person beside them who decides as they please. Committed, so
-- that the down migration runs over it; the downs after it drop it with
-- everything else.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

INSERT INTO term (id, name, starts_on, ends_on)
VALUES ('00000000-0000-0000-0014-000000000011', '2027 Spring', '2027-01-10', '2027-05-20');
INSERT INTO department (id, name) VALUES ('00000000-0000-0000-0014-000000000021', 'Physics');
INSERT INTO actor (id, kind, display_name, created_by_actor_id) VALUES
    ('00000000-0000-0000-0014-000000000031', 'human', 'Sato', NULL),
    ('00000000-0000-0000-0014-000000000032', 'human', 'Ken',  NULL),
    ('00000000-0000-0000-0014-000000000035', 'agent', 'Lab bot', NULL);
INSERT INTO actor (id, kind, display_name, owner_actor_id, created_by_actor_id) VALUES
    ('00000000-0000-0000-0014-000000000036', 'agent', 'Sato''s helper', '00000000-0000-0000-0014-000000000031',
     '00000000-0000-0000-0014-000000000031');
INSERT INTO course (id, dept_id, term_id, code, section, title, status, created_by_actor_id)
VALUES ('00000000-0000-0000-0014-000000000041', '00000000-0000-0000-0014-000000000021', '00000000-0000-0000-0014-000000000011',
        'PHYS101', 'A', 'Physics', 'active', '00000000-0000-0000-0014-000000000031');
-- 51 Sato, instructor · 55 the lab bot, on its own · 56 Sato's helper, his delegate;
-- all three asked to decide as they please, the two agents written deciding by proposal
INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope, perm_action_decide) VALUES
    ('00000000-0000-0000-0014-000000000051', '00000000-0000-0000-0014-000000000041', '00000000-0000-0000-0014-000000000031',
     'instructor', '00000000-0000-0000-0014-000000000031', 'all', 'all', 'autonomous'),
    ('00000000-0000-0000-0014-000000000055', '00000000-0000-0000-0014-000000000041', '00000000-0000-0000-0014-000000000035',
     'assistant', '00000000-0000-0000-0014-000000000031', 'all', 'all', 'autonomous');
INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope, perm_action_decide,
                           principal_member_id) VALUES
    ('00000000-0000-0000-0014-000000000056', '00000000-0000-0000-0014-000000000041', '00000000-0000-0000-0014-000000000036',
     'assistant', '00000000-0000-0000-0014-000000000031', 'all', 'all', 'pending_review', '00000000-0000-0000-0014-000000000051');

DO $chk$
BEGIN
    BEGIN
        UPDATE actor SET owner_actor_id = '00000000-0000-0000-0014-000000000032'
         WHERE id = '00000000-0000-0000-0014-000000000036';
        RAISE EXCEPTION 'FAIL  0014: an agent changed hands while 0014 was in';
    EXCEPTION WHEN restrict_violation THEN
        NULL;
    END;
    IF (SELECT array_agg(perm_action_decide::text ORDER BY id) FROM course_member
         WHERE course_id = '00000000-0000-0000-0014-000000000041')
       <> ARRAY['autonomous', 'confirm_required', 'confirm_required'] THEN
        RAISE EXCEPTION 'FAIL  0014: an agent was seated deciding but by proposal while 0014 was in';
    END IF;
END $chk$;

COMMIT;

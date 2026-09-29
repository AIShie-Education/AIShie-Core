-- AIshie Core — before 0007_agent_ownership.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with: an agent a person owns, seated
-- as their delegate with a proposal waiting, beside one of its seats already
-- removed, and holding a token its owner issued. Committed, so that the down migration runs over it; the downs
-- after it drop it with everything else.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

INSERT INTO term (id, name, starts_on, ends_on)
VALUES ('00000000-0000-0000-0007-000000000011', '2026 Autumn', '2026-09-01', '2026-12-20');
INSERT INTO department (id, name) VALUES ('00000000-0000-0000-0007-000000000021', 'Computing');
INSERT INTO actor (id, kind, display_name, platform_role, created_by_actor_id) VALUES
    ('00000000-0000-0000-0007-000000000031', 'human', 'root', 'root', NULL),
    ('00000000-0000-0000-0007-000000000035', 'human', 'Yuki', NULL, '00000000-0000-0000-0007-000000000031');
INSERT INTO actor (id, kind, display_name, owner_actor_id, created_by_actor_id) VALUES
    ('00000000-0000-0000-0007-000000000038', 'agent', 'Yuki''s agent', '00000000-0000-0000-0007-000000000035',
     '00000000-0000-0000-0007-000000000035');
INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, issued_by_actor_id) VALUES
    ('00000000-0000-0000-0007-0000000000c1', '00000000-0000-0000-0007-000000000038', 'api_token', 'h', 'down0007',
     '00000000-0000-0000-0007-000000000035');
INSERT INTO course (id, dept_id, term_id, code, section, title, status, created_by_actor_id)
VALUES ('00000000-0000-0000-0007-000000000041', '00000000-0000-0000-0007-000000000021', '00000000-0000-0000-0007-000000000011',
        'CS101', 'A', 'Intro', 'active', '00000000-0000-0000-0007-000000000031');
-- 52 Yuki · 5a her agent, her delegate · 5b an earlier seat of it, removed
INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope)
VALUES ('00000000-0000-0000-0007-000000000052', '00000000-0000-0000-0007-000000000041', '00000000-0000-0000-0007-000000000035',
        'student', '00000000-0000-0000-0007-000000000031', 'listed', 'all');
INSERT INTO course_member (id, course_id, actor_id, role, status, added_by_actor_id, student_scope, assignment_scope,
                           principal_member_id, perm_document_read) VALUES
    ('00000000-0000-0000-0007-00000000005b', '00000000-0000-0000-0007-000000000041', '00000000-0000-0000-0007-000000000038',
     'assistant', 'removed', '00000000-0000-0000-0007-000000000035', 'listed', 'all', '00000000-0000-0000-0007-000000000052', 'autonomous'),
    ('00000000-0000-0000-0007-00000000005a', '00000000-0000-0000-0007-000000000041', '00000000-0000-0000-0007-000000000038',
     'assistant', 'active', '00000000-0000-0000-0007-000000000035', 'listed', 'all', '00000000-0000-0000-0007-000000000052', 'autonomous');
INSERT INTO action (id, actor_id, course_id, member_id, action_type, target_type, payload_hash, idempotency_key,
                    authz_result, status) VALUES
    ('00000000-0000-0000-0007-0000000000b1', '00000000-0000-0000-0007-000000000038', '00000000-0000-0000-0007-000000000041',
     '00000000-0000-0000-0007-00000000005a', 'document.create', 'document', repeat('0', 64), 'k-waiting',
     'confirm_required', 'proposed');

COMMIT;

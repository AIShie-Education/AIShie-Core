-- AIshie Core — before 0010_department_admins.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with: a tree three levels deep, an
-- appointment in force beside one that has ended, and an action recorded as
-- made by a department's administrator. Committed, so that the down migration
-- runs over it; the downs after it drop it with everything else.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

-- 21 Engineering > 22 Computing > 23 AI
INSERT INTO department (id, name) VALUES ('00000000-0000-0000-0010-000000000021', 'Engineering');
INSERT INTO department (id, name, parent_id) VALUES
    ('00000000-0000-0000-0010-000000000022', 'Computing', '00000000-0000-0000-0010-000000000021');
INSERT INTO department (id, name, parent_id) VALUES
    ('00000000-0000-0000-0010-000000000023', 'AI', '00000000-0000-0000-0010-000000000022');
INSERT INTO actor (id, kind, display_name, platform_role, created_by_actor_id) VALUES
    ('00000000-0000-0000-0010-000000000031', 'human', 'root', 'root', NULL),
    ('00000000-0000-0000-0010-000000000032', 'human', 'Ada', NULL, '00000000-0000-0000-0010-000000000031'),
    ('00000000-0000-0000-0010-000000000033', 'human', 'Bob', NULL, '00000000-0000-0000-0010-000000000031');
-- a1 Ada at Engineering · a2 Bob at Computing, ended by Ada
INSERT INTO department_admin (id, dept_id, actor_id, appointed_by_actor_id) VALUES
    ('00000000-0000-0000-0010-0000000000a1', '00000000-0000-0000-0010-000000000021',
     '00000000-0000-0000-0010-000000000032', '00000000-0000-0000-0010-000000000031');
INSERT INTO department_admin (id, dept_id, actor_id, appointed_by_actor_id, removed_at, removed_by_actor_id) VALUES
    ('00000000-0000-0000-0010-0000000000a2', '00000000-0000-0000-0010-000000000022',
     '00000000-0000-0000-0010-000000000033', '00000000-0000-0000-0010-000000000032', now(),
     '00000000-0000-0000-0010-000000000032');
-- b1 Ada made AI, by her appointment at Engineering
INSERT INTO action (id, actor_id, action_type, target_type, payload_hash, idempotency_key,
                    authz_result, status, executed_at, authority, authority_dept_id) VALUES
    ('00000000-0000-0000-0010-0000000000b1', '00000000-0000-0000-0010-000000000032', 'department.create', 'department',
     repeat('0', 64), 'k-ai', 'autonomous', 'executed', now(), 'department', '00000000-0000-0000-0010-000000000021');

COMMIT;

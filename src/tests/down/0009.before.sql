-- AIshie Core — before 0009_memory.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with: a tutor's memory of a student
-- who asked it, a proposal to the course's shared memory, the owner's switch
-- and a count of writes. Committed, so that the down migration runs over
-- it; the downs after it drop the rest with everything else.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

INSERT INTO term (id, name, starts_on, ends_on)
VALUES ('00000000-0000-0000-0009-000000000011', '2027 Spring', '2027-01-10', '2027-05-20');
INSERT INTO department (id, name) VALUES ('00000000-0000-0000-0009-000000000021', 'Mathematics');
INSERT INTO actor (id, kind, display_name, created_by_actor_id) VALUES
    ('00000000-0000-0000-0009-000000000035', 'human', 'Mei',   NULL),
    ('00000000-0000-0000-0009-000000000036', 'agent', 'tutor', NULL);
INSERT INTO course (id, dept_id, term_id, code, section, title, status, created_by_actor_id)
VALUES ('00000000-0000-0000-0009-000000000041', '00000000-0000-0000-0009-000000000021', '00000000-0000-0000-0009-000000000011',
        'MATH201', 'A', 'Calculus', 'active', '00000000-0000-0000-0009-000000000035');
-- 52 Mei · 53 the tutor
INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope) VALUES
    ('00000000-0000-0000-0009-000000000052', '00000000-0000-0000-0009-000000000041', '00000000-0000-0000-0009-000000000035',
     'student', '00000000-0000-0000-0009-000000000035', 'listed', 'all'),
    ('00000000-0000-0000-0009-000000000053', '00000000-0000-0000-0009-000000000041', '00000000-0000-0000-0009-000000000036',
     'assistant', '00000000-0000-0000-0009-000000000035', 'listed', 'all');
-- b1 the tutor's memory.write
INSERT INTO action (id, actor_id, action_type, target_type, payload_hash, idempotency_key, authz_result, status, executed_at)
VALUES ('00000000-0000-0000-0009-0000000000b1', '00000000-0000-0000-0009-000000000036', 'memory.write', 'memory',
        repeat('0', 64), 'k-remember', 'autonomous', 'executed', now());
INSERT INTO memory_entry (id, holder_actor_id, scope, course_id, holder_member_id, subject_actor_id, subject_member_id, status,
                          body, search_text, text_hash, source, created_by_actor_id, created_by_action_id,
                          updated_by_actor_id, updated_by_action_id, created_at, updated_at) VALUES
    ('00000000-0000-0000-0009-0000000000e1', '00000000-0000-0000-0009-000000000036', 'asker', '00000000-0000-0000-0009-000000000041',
     '00000000-0000-0000-0009-000000000053', '00000000-0000-0000-0009-000000000035', '00000000-0000-0000-0009-000000000052', 'active',
     'Prefers graphs to formulas.', 'prefers graphs to formulas', '\x01', 'agent', '00000000-0000-0000-0009-000000000036',
     '00000000-0000-0000-0009-0000000000b1', '00000000-0000-0000-0009-000000000036', '00000000-0000-0000-0009-0000000000b1', now(), now()),
    ('00000000-0000-0000-0009-0000000000e2', '00000000-0000-0000-0009-000000000036', 'course', '00000000-0000-0000-0009-000000000041',
     '00000000-0000-0000-0009-000000000053', NULL, NULL, 'proposed',
     'The midterm covers chapters 1 to 4.', 'the midterm covers chapters 1 to 4', '\x02', 'agent', '00000000-0000-0000-0009-000000000036',
     '00000000-0000-0000-0009-0000000000b1', '00000000-0000-0000-0009-000000000036', '00000000-0000-0000-0009-0000000000b1', now(), now());
INSERT INTO memory_setting (holder_actor_id, enabled, updated_by_actor_id, updated_at)
VALUES ('00000000-0000-0000-0009-000000000036', true, '00000000-0000-0000-0009-000000000035', now());
INSERT INTO memory_write_count (holder_actor_id, hour, n)
VALUES ('00000000-0000-0000-0009-000000000036', date_trunc('hour', now()), 2);

COMMIT;

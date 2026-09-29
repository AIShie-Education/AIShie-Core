-- AIshie Core — before 0008_conversations.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with: a conversation with a message in
-- it, and an answer waiting for a decision. Committed, so that the down
-- migration runs over it; the downs after it drop the rest with everything
-- else.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

INSERT INTO term (id, name, starts_on, ends_on)
VALUES ('00000000-0000-0000-0008-000000000011', '2026 Autumn', '2026-09-01', '2026-12-20');
INSERT INTO department (id, name) VALUES ('00000000-0000-0000-0008-000000000021', 'Computing');
INSERT INTO actor (id, kind, display_name, created_by_actor_id) VALUES
    ('00000000-0000-0000-0008-000000000035', 'human', 'Yuki',  NULL),
    ('00000000-0000-0000-0008-000000000036', 'agent', 'tutor', NULL);
INSERT INTO course (id, dept_id, term_id, code, section, title, status, created_by_actor_id)
VALUES ('00000000-0000-0000-0008-000000000041', '00000000-0000-0000-0008-000000000021', '00000000-0000-0000-0008-000000000011',
        'CS101', 'A', 'Intro', 'active', '00000000-0000-0000-0008-000000000035');
-- 52 Yuki · 53 the tutor
INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope) VALUES
    ('00000000-0000-0000-0008-000000000052', '00000000-0000-0000-0008-000000000041', '00000000-0000-0000-0008-000000000035',
     'student', '00000000-0000-0000-0008-000000000035', 'listed', 'all'),
    ('00000000-0000-0000-0008-000000000053', '00000000-0000-0000-0008-000000000041', '00000000-0000-0000-0008-000000000036',
     'assistant', '00000000-0000-0000-0008-000000000035', 'listed', 'all');
-- b1 Yuki's question · b2 the tutor's answer, waiting
INSERT INTO action (id, actor_id, course_id, member_id, action_type, target_type, payload_hash, idempotency_key,
                    authz_result, status, executed_at) VALUES
    ('00000000-0000-0000-0008-0000000000b1', '00000000-0000-0000-0008-000000000035', '00000000-0000-0000-0008-000000000041',
     '00000000-0000-0000-0008-000000000052', 'conversation.open', 'conversation', repeat('0', 64), 'k-open',
     'autonomous', 'executed', now());
INSERT INTO action (id, actor_id, course_id, member_id, action_type, target_type, payload_hash, idempotency_key,
                    authz_result, status) VALUES
    ('00000000-0000-0000-0008-0000000000b2', '00000000-0000-0000-0008-000000000036', '00000000-0000-0000-0008-000000000041',
     '00000000-0000-0000-0008-000000000053', 'conversation.answer', 'conversation', repeat('0', 64), 'k-answer',
     'confirm_required', 'proposed');
INSERT INTO conversation (id, course_id, opener_member_id, respondent_member_id, last_message_at, last_author_member_id)
VALUES ('00000000-0000-0000-0008-0000000000c1', '00000000-0000-0000-0008-000000000041', '00000000-0000-0000-0008-000000000052',
        '00000000-0000-0000-0008-000000000053', now(), '00000000-0000-0000-0008-000000000052');
INSERT INTO conversation_message (conversation_id, course_id, seq, author_member_id, body, created_by_action_id)
VALUES ('00000000-0000-0000-0008-0000000000c1', '00000000-0000-0000-0008-000000000041', 1,
        '00000000-0000-0000-0008-000000000052', 'What is a thesis?', '00000000-0000-0000-0008-0000000000b1');

COMMIT;

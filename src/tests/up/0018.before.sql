-- AIshiteru Core — before 0018_conversations_with_agents.up.sql, in `make db-test-sql`
--
-- What the migration finds: a course in which people answer conversations
-- beside an agent. An instructor who answers, a TA who answers by proposal,
-- a student asking each of them and the course's tutor agent, twice, once
-- with nothing asked yet, and a seat removed while it answered; answers and
-- a question waiting for approval in the conversations with people, one
-- waiting in the tutor's, and conversations waiting to be opened with a
-- person and with the tutor; and presets for people that give
-- conversation_answer, beside one for agents. Nobody's read state yet.
-- Committed, so that the migration runs over it; it stays, for the redo and
-- the downs after it (tests/down/0018.*), and the downs after that drop it
-- with everything else.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

INSERT INTO term (id, name, starts_on, ends_on)
VALUES ('00000000-0000-0000-0018-000000000011', '2027 Spring', '2027-01-10', '2027-05-20');
INSERT INTO department (id, name) VALUES ('00000000-0000-0000-0018-000000000021', 'Nursing');
-- 31 Lin, the instructor · 32 Ho, a TA · 33 Wei, a student · 34 Old, an instructor who left · 36 the tutor, an agent
INSERT INTO actor (id, kind, display_name, created_by_actor_id) VALUES
    ('00000000-0000-0000-0018-000000000031', 'human', 'Lin',   NULL),
    ('00000000-0000-0000-0018-000000000032', 'human', 'Ho',    NULL),
    ('00000000-0000-0000-0018-000000000033', 'human', 'Wei',   NULL),
    ('00000000-0000-0000-0018-000000000034', 'human', 'Old',   NULL),
    ('00000000-0000-0000-0018-000000000036', 'agent', 'tutor', NULL);
INSERT INTO course (id, dept_id, term_id, code, section, title, status, created_by_actor_id)
VALUES ('00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0018-000000000021', '00000000-0000-0000-0018-000000000011',
        'NUR101', 'A', 'Foundations of Care', 'active', '00000000-0000-0000-0018-000000000031');
-- 51 Lin, answering · 52 Ho, answering by proposal · 53 Wei · 54 Old, removed, answering · 56 the tutor, answering
INSERT INTO course_member (id, course_id, actor_id, role, status, added_by_actor_id, student_scope, assignment_scope,
                           perm_conversation_ask, perm_conversation_answer, perm_action_decide) VALUES
    ('00000000-0000-0000-0018-000000000051', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0018-000000000031',
     'instructor', 'active', '00000000-0000-0000-0018-000000000031', 'all', 'all', 'autonomous', 'autonomous', 'autonomous'),
    ('00000000-0000-0000-0018-000000000052', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0018-000000000032',
     'ta', 'active', '00000000-0000-0000-0018-000000000031', 'all', 'all', 'autonomous', 'confirm_required', 'denied'),
    ('00000000-0000-0000-0018-000000000053', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0018-000000000033',
     'student', 'active', '00000000-0000-0000-0018-000000000031', 'listed', 'all', 'autonomous', 'denied', 'denied'),
    ('00000000-0000-0000-0018-000000000054', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0018-000000000034',
     'instructor', 'removed', '00000000-0000-0000-0018-000000000031', 'all', 'all', 'autonomous', 'autonomous', 'autonomous'),
    ('00000000-0000-0000-0018-000000000056', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0018-000000000036',
     'assistant', 'active', '00000000-0000-0000-0018-000000000031', 'listed', 'all', 'denied', 'autonomous', 'denied');
INSERT INTO member_student_scope (member_id, student_member_id) VALUES
    ('00000000-0000-0000-0018-000000000053', '00000000-0000-0000-0018-000000000053');

-- Presets: 91 a department's for its leads · 92 a built-in for tutors that
-- answer, as TAs · 93 a department's for its agents, which stays as it is
INSERT INTO permission_preset (id, dept_id, name, role, student_scope, assignment_scope, perm_document_read, perm_conversation_answer) VALUES
    ('00000000-0000-0000-0018-000000000091', '00000000-0000-0000-0018-000000000021', 'up0018-lead', 'instructor', 'all', 'all',
     'autonomous', 'autonomous'),
    ('00000000-0000-0000-0018-000000000092', NULL, 'up0018-answering-ta', 'ta', 'all', 'all', 'autonomous', 'confirm_required'),
    ('00000000-0000-0000-0018-000000000093', '00000000-0000-0000-0018-000000000021', 'up0018-bot', 'assistant', 'listed', 'all',
     'autonomous', 'autonomous');

-- Actions: b1..b4 Wei's questions that opened c1..c4 · b5 Lin's answer to c1, waiting · b6 Wei's second
-- question in c2, waiting · b7 the tutor's answer in c3, waiting · b8 a conversation with Ho, waiting to
-- be opened · b9 one with the tutor, waiting to be opened · ba Ho's draft of a document, waiting
INSERT INTO action (id, actor_id, course_id, member_id, action_type, target_type, target_id, payload, payload_hash,
                    idempotency_key, authz_result, status, executed_at) VALUES
    ('00000000-0000-0000-0018-0000000000b1', '00000000-0000-0000-0018-000000000033', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000053', 'conversation.open', 'conversation', '00000000-0000-0000-0018-0000000000c1',
     '{}', repeat('0', 64), 'up18-b1', 'autonomous', 'executed', now()),
    ('00000000-0000-0000-0018-0000000000b2', '00000000-0000-0000-0018-000000000033', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000053', 'conversation.open', 'conversation', '00000000-0000-0000-0018-0000000000c2',
     '{}', repeat('0', 64), 'up18-b2', 'autonomous', 'executed', now()),
    ('00000000-0000-0000-0018-0000000000b3', '00000000-0000-0000-0018-000000000033', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000053', 'conversation.open', 'conversation', '00000000-0000-0000-0018-0000000000c3',
     '{}', repeat('0', 64), 'up18-b3', 'autonomous', 'executed', now()),
    ('00000000-0000-0000-0018-0000000000b4', '00000000-0000-0000-0018-000000000033', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000053', 'conversation.open', 'conversation', '00000000-0000-0000-0018-0000000000c4',
     '{}', repeat('0', 64), 'up18-b4', 'autonomous', 'executed', now());
INSERT INTO action (id, actor_id, course_id, member_id, action_type, target_type, target_id, payload, payload_hash,
                    idempotency_key, authz_result, status) VALUES
    ('00000000-0000-0000-0018-0000000000b5', '00000000-0000-0000-0018-000000000031', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000051', 'conversation.answer', 'conversation', '00000000-0000-0000-0018-0000000000c1',
     '{"in_reply_to_message_id": "00000000-0000-0000-0018-000000000c11"}', repeat('0', 64), 'up18-b5', 'confirm_required', 'proposed'),
    ('00000000-0000-0000-0018-0000000000b6', '00000000-0000-0000-0018-000000000033', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000053', 'conversation.ask', 'conversation', '00000000-0000-0000-0018-0000000000c2',
     '{"body": "And the next one?"}', repeat('0', 64), 'up18-b6', 'confirm_required', 'proposed'),
    ('00000000-0000-0000-0018-0000000000b7', '00000000-0000-0000-0018-000000000036', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000056', 'conversation.answer', 'conversation', '00000000-0000-0000-0018-0000000000c3',
     '{"in_reply_to_message_id": "00000000-0000-0000-0018-000000000c31"}', repeat('0', 64), 'up18-b7', 'confirm_required', 'proposed'),
    ('00000000-0000-0000-0018-0000000000b8', '00000000-0000-0000-0018-000000000033', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000053', 'conversation.open', 'conversation', NULL,
     '{"respondent_member_id": "00000000-0000-0000-0018-000000000052"}', repeat('0', 64), 'up18-b8', 'confirm_required', 'proposed'),
    ('00000000-0000-0000-0018-0000000000b9', '00000000-0000-0000-0018-000000000033', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000053', 'conversation.open', 'conversation', NULL,
     '{"respondent_member_id": "00000000-0000-0000-0018-000000000056"}', repeat('0', 64), 'up18-b9', 'confirm_required', 'proposed'),
    ('00000000-0000-0000-0018-0000000000ba', '00000000-0000-0000-0018-000000000032', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000052', 'document.create', 'document', NULL,
     '{}', repeat('0', 64), 'up18-ba', 'confirm_required', 'proposed');

-- Conversations: c1 Wei asks Lin · c2 Wei asks Ho · c3 Wei asks the tutor · c4 Wei asked Lin, closed · c5 Old
-- was asked, and his seat was removed before 0008's removals closed anything · c6 Wei opened one with the
-- tutor and has asked nothing yet
INSERT INTO conversation (id, course_id, opener_member_id, respondent_member_id, last_message_at, last_author_member_id) VALUES
    ('00000000-0000-0000-0018-0000000000c1', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0018-000000000053',
     '00000000-0000-0000-0018-000000000051', now(), '00000000-0000-0000-0018-000000000053'),
    ('00000000-0000-0000-0018-0000000000c2', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0018-000000000053',
     '00000000-0000-0000-0018-000000000052', now(), '00000000-0000-0000-0018-000000000053'),
    ('00000000-0000-0000-0018-0000000000c3', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0018-000000000053',
     '00000000-0000-0000-0018-000000000056', now(), '00000000-0000-0000-0018-000000000053'),
    ('00000000-0000-0000-0018-0000000000c4', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0018-000000000053',
     '00000000-0000-0000-0018-000000000051', now(), '00000000-0000-0000-0018-000000000053'),
    ('00000000-0000-0000-0018-0000000000c5', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0018-000000000053',
     '00000000-0000-0000-0018-000000000054', now(), '00000000-0000-0000-0018-000000000053');
INSERT INTO conversation (id, course_id, opener_member_id, respondent_member_id) VALUES
    ('00000000-0000-0000-0018-0000000000c6', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0018-000000000053',
     '00000000-0000-0000-0018-000000000056');
INSERT INTO conversation_message (id, conversation_id, course_id, seq, author_member_id, body, created_by_action_id) VALUES
    ('00000000-0000-0000-0018-000000000c11', '00000000-0000-0000-0018-0000000000c1', '00000000-0000-0000-0018-000000000041', 1,
     '00000000-0000-0000-0018-000000000053', 'When is the skills lab?', '00000000-0000-0000-0018-0000000000b1'),
    ('00000000-0000-0000-0018-000000000c21', '00000000-0000-0000-0018-0000000000c2', '00000000-0000-0000-0018-000000000041', 1,
     '00000000-0000-0000-0018-000000000053', 'Can you check my care plan?', '00000000-0000-0000-0018-0000000000b2'),
    ('00000000-0000-0000-0018-000000000c31', '00000000-0000-0000-0018-0000000000c3', '00000000-0000-0000-0018-000000000041', 1,
     '00000000-0000-0000-0018-000000000053', 'What is a care plan?', '00000000-0000-0000-0018-0000000000b3'),
    ('00000000-0000-0000-0018-000000000c41', '00000000-0000-0000-0018-0000000000c4', '00000000-0000-0000-0018-000000000041', 1,
     '00000000-0000-0000-0018-000000000053', 'Is attendance taken?', '00000000-0000-0000-0018-0000000000b4'),
    ('00000000-0000-0000-0018-000000000c51', '00000000-0000-0000-0018-0000000000c5', '00000000-0000-0000-0018-000000000041', 1,
     '00000000-0000-0000-0018-000000000053', 'Are you still teaching this?', '00000000-0000-0000-0018-0000000000b4');
UPDATE conversation SET status = 'closed', closed_reason = 'Thanks!' WHERE id = '00000000-0000-0000-0018-0000000000c4';

COMMIT;

-- AIshie Core — before 0029_message_sources.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with, written as this release writes
-- it: the tutor's answer to Wei's question in the conversation of
-- tests/up/0018, relying on page 3 of Week 4's slides (tests/up/0027) and on
-- Week 2 (tests/up/0026). Committed, so that the down migration runs over
-- it.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

-- c32 the tutor's answer to c31
INSERT INTO conversation_message (id, conversation_id, course_id, seq, author_member_id, in_reply_to_message_id, body,
                                  created_by_action_id, sources_stated)
VALUES ('00000000-0000-0000-0029-000000000c32', '00000000-0000-0000-0018-0000000000c3', '00000000-0000-0000-0018-000000000041',
        (SELECT max(seq) + 1 FROM conversation_message WHERE conversation_id = '00000000-0000-0000-0018-0000000000c3'),
        '00000000-0000-0000-0018-000000000056', '00000000-0000-0000-0018-000000000c31', 'A plan of the care a patient needs.',
        '00000000-0000-0000-0018-0000000000b3', true);
INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id, file_id, file_version_id, page)
VALUES ('00000000-0000-0000-0029-000000000c32', '00000000-0000-0000-0018-000000000041', 1,
        '00000000-0000-0000-0027-0000000000e1', '00000000-0000-0000-0027-0000000000f1',
        '00000000-0000-0000-0027-0000000000d1', '00000000-0000-0000-0027-0000000000f1', 3);
INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id)
VALUES ('00000000-0000-0000-0029-000000000c32', '00000000-0000-0000-0018-000000000041', 2,
        '00000000-0000-0000-0026-0000000000e9', '00000000-0000-0000-0026-0000000000f1');

COMMIT;

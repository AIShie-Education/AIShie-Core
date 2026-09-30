-- AIshie Core — before 0021_conversation_attachments.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with: Wei's question to the tutor in
-- the conversations of tests/up/0018, carrying two files, and her question
-- to Lin, carrying one. Committed, so that the down migration runs over it.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

-- a1, a2 the files of c31, Wei's question to the tutor · a3 the file of c11, her question to Lin
INSERT INTO conversation_attachment (id, message_id, conversation_id, course_id, position, filename, storage_key,
                                     content_type, byte_size, checksum, created_at)
SELECT x.id::uuid, m.id, m.conversation_id, m.course_id, x.position, x.filename, x.storage_key, 'application/pdf', 2048,
       'sha256:00', m.created_at
FROM (VALUES ('00000000-0000-0000-0021-0000000000a1', '00000000-0000-0000-0018-000000000c31', 1, 'care plan.pdf',
              'conversations/down21/a1'),
             ('00000000-0000-0000-0021-0000000000a2', '00000000-0000-0000-0018-000000000c31', 2, 'notes.pdf',
              'conversations/down21/a2'),
             ('00000000-0000-0000-0021-0000000000a3', '00000000-0000-0000-0018-000000000c11', 1, 'timetable.pdf',
              'conversations/down21/a3')) AS x(id, message_id, position, filename, storage_key)
JOIN conversation_message m ON m.id = x.message_id::uuid;

COMMIT;

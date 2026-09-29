-- AIshiteru Core — before 0019_conversation_drafts.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with: an answer being written in the
-- tutor's conversation of tests/up/0018, its draft kept. Committed, so that
-- the down migration runs over it.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

INSERT INTO conversation_draft (conversation_id, course_id, attempt, version, body, steps)
VALUES ('00000000-0000-0000-0018-0000000000c3', '00000000-0000-0000-0018-000000000041', 'a1', 3, 'So far',
        '[{"kind":"reading_document","target":"Week 1","state":"done"},{"kind":"writing","state":"running"}]');

COMMIT;

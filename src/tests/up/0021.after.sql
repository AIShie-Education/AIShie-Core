-- AIshie Core — after 0021_conversation_attachments.up.sql, in `make db-test-sql`
--
-- Nothing written before carries a file: the table is new and empty, the
-- conversations of tests/up/0018 and their messages are as they were, and
-- a file is held to its message from the start.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM conversation_attachment) THEN
        RAISE EXCEPTION 'FAIL  0021 up: a message was given a file';
    END IF;
    IF (SELECT count(*) FROM conversation_message WHERE id::text LIKE '00000000-0000-0000-0018-%') = 0 THEN
        RAISE EXCEPTION 'FAIL  0021 up: the messages of before are gone';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'conversation_attachment_with_its_message')
       OR NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'conversation_attachment_append_only') THEN
        RAISE EXCEPTION 'FAIL  0021 up: files are not held to their messages';
    END IF;
END $chk$;
\echo 'PASS  0021 up gives no message a file, and holds each file to its message from the start'

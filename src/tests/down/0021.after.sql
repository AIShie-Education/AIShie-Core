-- AIshie Core — after 0021_conversation_attachments.down.sql, in `make db-test-sql`
--
-- The files' rows are gone, and with them nothing else: the messages that
-- carried them stay as they were written. The release before this one
-- writes no attachment and reads none.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'conversation_attachment') THEN
        RAISE EXCEPTION 'FAIL  0021 down: the attachments are still there';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'conversation_attachment_check_message') THEN
        RAISE EXCEPTION 'FAIL  0021 down: the attachments'' trigger function is still there';
    END IF;
    IF (SELECT count(*) FROM conversation_message
        WHERE id IN ('00000000-0000-0000-0018-000000000c31', '00000000-0000-0000-0018-000000000c11')) <> 2 THEN
        RAISE EXCEPTION 'FAIL  0021 down: a message that carried files is gone';
    END IF;
END $chk$;
\echo 'PASS  0021 down with files on messages, whose rows go, and the messages, which stay'

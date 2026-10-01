-- AIshie Core — after 0024_conversation_export.down.sql, in `make db-test-sql`
--
-- The index is gone, and with it nothing else: every message stays as it
-- was. Exports made meanwhile are actions, which stay.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'conversation_message_created_idx') THEN
        RAISE EXCEPTION 'FAIL  0024 down: the index of the messages by when they were written is still there';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM conversation_message WHERE conversation_id = '00000000-0000-0000-0018-0000000000c3') THEN
        RAISE EXCEPTION 'FAIL  0024 down: what was written in a conversation is gone';
    END IF;
END $chk$;
\echo 'PASS  0024 down drops the index and nothing else'

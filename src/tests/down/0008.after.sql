-- AIshiteru Core — after 0008_conversations.down.sql, in `make db-test-sql`
--
-- The conversation tables are gone; the answer that waited is cancelled as a
-- decision cancels a proposal whose tool is gone; the action that wrote the
-- question stays, as history.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name LIKE 'conversation%') THEN
        RAISE EXCEPTION 'FAIL  0008 down: a conversation table is still there';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM action WHERE id = '00000000-0000-0000-0008-0000000000b2' AND status = 'cancelled'
                   AND result->'error'->>'code' = 'failed_precondition'
                   AND result->'error'->'details'->>'reason' = 'tool_removed') THEN
        RAISE EXCEPTION 'FAIL  0008 down: the waiting answer was not cancelled';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM action WHERE id = '00000000-0000-0000-0008-0000000000b1' AND status = 'executed') THEN
        RAISE EXCEPTION 'FAIL  0008 down: the action that wrote a message did not stay';
    END IF;
END $chk$;
\echo 'PASS  0008 down with a conversation present'

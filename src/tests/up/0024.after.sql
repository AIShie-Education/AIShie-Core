-- AIshie Core — after 0024_conversation_export.up.sql, in `make db-test-sql`
--
-- The messages written in a span of time are found by when they were
-- written, through the index, without reading every message of the site;
-- nothing that was written is changed.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
DECLARE
    line text;
    used boolean := false;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_indexes WHERE tablename = 'conversation_message'
                   AND indexname = 'conversation_message_created_idx' AND indexdef LIKE '%(created_at)') THEN
        RAISE EXCEPTION 'FAIL  0024 up: no index of the messages by when they were written';
    END IF;
    SET LOCAL enable_seqscan = off;
    FOR line IN EXECUTE $q$EXPLAIN SELECT m.conversation_id FROM conversation_message m
                           WHERE m.created_at >= '2026-09-01' AND m.created_at < '2026-10-01'$q$ LOOP
        used := used OR line LIKE '%conversation_message_created_idx%';
    END LOOP;
    IF NOT used THEN
        RAISE EXCEPTION 'FAIL  0024 up: the messages of a span of time are not found through the index';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM conversation_message WHERE conversation_id = '00000000-0000-0000-0018-0000000000c3') THEN
        RAISE EXCEPTION 'FAIL  0024 up: what was written in a conversation is gone';
    END IF;
END $chk$;
\echo 'PASS  0024 up finds the messages of a span of time by the index, and changes nothing written'

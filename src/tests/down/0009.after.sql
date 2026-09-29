-- AIshie Core — after 0009_memory.down.sql, in `make db-test-sql`
--
-- The memory tables are gone, and every entry with them; the action that
-- wrote one stays, as history, and never held its text.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name LIKE 'memory%') THEN
        RAISE EXCEPTION 'FAIL  0009 down: a memory table is still there';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'memory_entry_check') THEN
        RAISE EXCEPTION 'FAIL  0009 down: the memory trigger''s function is still there';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM action WHERE id = '00000000-0000-0000-0009-0000000000b1' AND status = 'executed') THEN
        RAISE EXCEPTION 'FAIL  0009 down: the action that wrote an entry did not stay';
    END IF;
END $chk$;
\echo 'PASS  0009 down with memory present'

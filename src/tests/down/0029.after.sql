-- AIshie Core — after 0029_message_sources.down.sql, in `make db-test-sql`
--
-- The sources are gone, and what held them, and so is whether an answer
-- said what it relied on; the answer that named them stays as it was
-- written, and so do the versions and files it relied on.
-- The release before this one writes no source and reads none.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public'
               AND table_name = 'conversation_message_source') THEN
        RAISE EXCEPTION 'FAIL  0029 down: the sources are still there';
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public'
               AND table_name = 'conversation_message' AND column_name = 'sources_stated') THEN
        RAISE EXCEPTION 'FAIL  0029 down: messages still say whether they named their sources';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_proc WHERE proname IN ('conversation_message_source_check', 'conversation_message_source_guarded')) THEN
        RAISE EXCEPTION 'FAIL  0029 down: a function it added is still there';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM conversation_message WHERE id = '00000000-0000-0000-0029-000000000c32'
                   AND body = 'A plan of the care a patient needs.')
       OR NOT EXISTS (SELECT 1 FROM document_version_file WHERE id = '00000000-0000-0000-0027-0000000000d1')
       OR NOT EXISTS (SELECT 1 FROM document_version WHERE id = '00000000-0000-0000-0026-0000000000f1' AND purged_at IS NULL) THEN
        RAISE EXCEPTION 'FAIL  0029 down: the answer, or what it relied on, is gone';
    END IF;
END $chk$;
\echo 'PASS  0029 down with an answer''s sources, whose rows go, and the answer, which stays'

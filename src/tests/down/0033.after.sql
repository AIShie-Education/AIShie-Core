-- AIshie Core — after 0033_document_search_passages.down.sql, in make db-test-sql
\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public'
               AND table_name = 'document_search_passages') THEN
        RAISE EXCEPTION 'FAIL  0033 down: the passage table remains';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'document_version_purge_search_passages')
       OR EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'document_version_search_passages_purged' AND NOT tgisinternal) THEN
        RAISE EXCEPTION 'FAIL  0033 down: the purge function or trigger remains';
    END IF;
END $chk$;
\echo 'PASS  0033 down removes the derived search schema and its purge hook'

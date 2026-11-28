-- AIshie Core — after 0034_mcp_oauth.down.sql, in make db-test-sql
\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public'
               AND table_name IN ('oauth_client', 'oauth_access_token')) THEN
        RAISE EXCEPTION 'FAIL  0034 down: an OAuth table remains';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM actor WHERE id = '00000000-0000-0000-0034-0000000000a1') THEN
        RAISE EXCEPTION 'FAIL  0034 down: the agent actor was removed with its clients';
    END IF;
END $chk$;
\echo 'PASS  0034 down removes populated OAuth tables and keeps the agent actor'

-- AIshie Core — after 0025_agent_hosting.down.sql, in `make db-test-sql`
--
-- Hosting is gone, and who a token was issued to. The coach's runtime
-- token stays live, an API token of its own, and its site chat credential
-- still names it, so that the release before asks it in the site. The
-- service stays, as its action names it, as an agent nobody owns,
-- suspended, its credentials revoked API tokens: the one revoked before
-- keeps its date.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public'
               AND ((table_name = 'actor' AND column_name = 'hosting')
                    OR (table_name = 'credential' AND column_name = 'issued_to_service'))) THEN
        RAISE EXCEPTION 'FAIL  0025 down: a column it added is still there';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_proc WHERE proname IN ('credential_check_hosting', 'actor_hosting_unchanged', 'actor_hosting_by_default'))
       OR EXISTS (SELECT 1 FROM pg_class WHERE relname = 'credential_one_runtime_token') THEN
        RAISE EXCEPTION 'FAIL  0025 down: a function or an index it added is still there';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM credential WHERE id = '00000000-0000-0000-0025-0000000000d1' AND kind = 'api_token'
                   AND revoked_at IS NULL)
       OR NOT EXISTS (SELECT 1 FROM actor WHERE id = '00000000-0000-0000-0025-00000000003a'
                      AND site_chat_credential_id = '00000000-0000-0000-0025-0000000000d1' AND status = 'active') THEN
        RAISE EXCEPTION 'FAIL  0025 down: the coach''s runtime token is not its live, declared token';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM actor WHERE id = '00000000-0000-0000-0025-00000000004a'
                   AND kind = 'agent' AND status = 'suspended' AND owner_actor_id IS NULL AND service_scope IS NULL)
       OR NOT EXISTS (SELECT 1 FROM action WHERE id = '00000000-0000-0000-0025-0000000000b1') THEN
        RAISE EXCEPTION 'FAIL  0025 down: the service is not an agent, suspended, and still named by its action';
    END IF;
    IF EXISTS (SELECT 1 FROM credential WHERE actor_id = '00000000-0000-0000-0025-00000000004a'
               AND (kind <> 'api_token' OR revoked_at IS NULL))
       OR NOT EXISTS (SELECT 1 FROM credential WHERE id = '00000000-0000-0000-0025-0000000004c2'
                      AND revoked_at = '2026-09-01 00:00:00+00') THEN
        RAISE EXCEPTION 'FAIL  0025 down: the service''s credentials are not revoked API tokens as they were revoked';
    END IF;
END $chk$;
\echo 'PASS  0025 down with runtime and mcp agents and the agent runtime service, which becomes a suspended agent whose credentials are revoked'

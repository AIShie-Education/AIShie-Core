-- AIshie Core — after 0025_agent_hosting.up.sql, in `make db-test-sql`
--
-- The tutor and the enrolment bot, whose runtimes' tokens were live, are
-- runtime agents, each token taken as the runtime's; the tutor's laptop
-- token is revoked, and the one revoked long ago keeps its date. The
-- helper, the bot and the script are mcp agents, their tokens as they
-- were. Every agent has a hosting, the agents of the fixtures before
-- among them, and nobody else has one.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
DECLARE
    got text;
BEGIN
    SELECT string_agg(right(id::text, 2) || '=' || coalesce(hosting, '-'), ' ' ORDER BY id) INTO got
      FROM actor WHERE id::text LIKE '00000000-0000-0000-0025-%';
    IF got IS DISTINCT FROM '31=- 35=runtime 36=mcp 37=mcp 38=mcp 39=runtime' THEN
        RAISE EXCEPTION 'FAIL  0025 up: the hostings are %', got;
    END IF;
    IF EXISTS (SELECT 1 FROM actor WHERE (kind = 'agent') <> (hosting IS NOT NULL)) THEN
        RAISE EXCEPTION 'FAIL  0025 up: an agent has no hosting, or someone else has one';
    END IF;
    SELECT string_agg(right(id::text, 2) || '=' || coalesce(issued_to_service, '-') || ':'
                      || CASE WHEN revoked_at IS NULL THEN 'live' ELSE 'revoked' END, ' ' ORDER BY id) INTO got
      FROM credential WHERE id::text LIKE '00000000-0000-0000-0025-%';
    IF got IS DISTINCT FROM 'c1=agent_runtime:live c2=-:revoked c3=-:revoked c4=-:revoked c5=-:live c6=-:live c7=-:live c8=agent_runtime:live' THEN
        RAISE EXCEPTION 'FAIL  0025 up: the tokens are %', got;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM credential WHERE id = '00000000-0000-0000-0025-0000000000c3' AND revoked_at = '2026-01-01 00:00:00+00')
       OR NOT EXISTS (SELECT 1 FROM credential WHERE id = '00000000-0000-0000-0025-0000000000c4' AND revoked_at = '2026-02-01 00:00:00+00') THEN
        RAISE EXCEPTION 'FAIL  0025 up: a token revoked already was revoked again, and lost its date';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM actor WHERE id = '00000000-0000-0000-0025-000000000035'
                   AND site_chat_credential_id = '00000000-0000-0000-0025-0000000000c1') THEN
        RAISE EXCEPTION 'FAIL  0025 up: the tutor''s site chat credential changed';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'actor_hosting_fixed')
       OR NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'credential_fits_hosting')
       OR NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'actor_hosting_default') THEN
        RAISE EXCEPTION 'FAIL  0025 up: no trigger holds the rules';
    END IF;
END $chk$;
\echo 'PASS  0025 up makes runtime agents of those whose runtime''s token is live, taking it as the runtime''s and revoking their others, and mcp agents of the rest'

-- AIshiteru Core — after 0011_site_chat.down.sql, in `make db-test-sql`
--
-- Which credential declared site chat is gone, and the key it was held by;
-- the agent and its token stay, the token still live.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public'
               AND table_name = 'actor' AND column_name = 'site_chat_credential_id') THEN
        RAISE EXCEPTION 'FAIL  0011 down: actor.site_chat_credential_id is still there';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conname IN ('credential_id_actor_key', 'actor_site_chat_is_agent',
                                                             'actor_site_chat_credential_fk')) THEN
        RAISE EXCEPTION 'FAIL  0011 down: a constraint it added is still there';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM actor WHERE id = '00000000-0000-0000-0011-000000000036' AND kind = 'agent' AND status = 'active') THEN
        RAISE EXCEPTION 'FAIL  0011 down: the agent did not stay';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM credential WHERE id = '00000000-0000-0000-0011-0000000000c1' AND revoked_at IS NULL) THEN
        RAISE EXCEPTION 'FAIL  0011 down: the runtime''s token did not stay as it was';
    END IF;
END $chk$;
\echo 'PASS  0011 down with an agent''s site chat declared'

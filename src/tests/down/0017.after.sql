-- AIshie Core — after 0017_api_tokens_for_agents.down.sql, in `make db-test-sql`
--
-- The refusal is gone, and with it nothing else: what the up revoked stays
-- revoked, and every credential live before the down is live after it. The
-- release before this one may issue a person a token again, and give an
-- agent a password.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'credential_fits_actor_kind')
       OR EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'credential_check_actor_kind') THEN
        RAISE EXCEPTION 'FAIL  0017 down: the trigger or its function is still there';
    END IF;
    IF EXISTS (SELECT 1 FROM credential WHERE revoked_at IS NULL
               AND id IN ('00000000-0000-0000-0017-0000000001a1', '00000000-0000-0000-0017-0000000001b1',
                          '00000000-0000-0000-0017-0000000001d3', '00000000-0000-0000-0017-0000000001e2')) THEN
        RAISE EXCEPTION 'FAIL  0017 down: a credential the up revoked works again';
    END IF;
    IF (SELECT count(*) FROM credential WHERE revoked_at IS NULL
        AND id IN ('00000000-0000-0000-0017-0000000002a1', '00000000-0000-0000-0017-0000000002a2',
                   '00000000-0000-0000-0017-0000000002b1', '00000000-0000-0000-0017-0000000001b4',
                   '00000000-0000-0000-0017-0000000001d1')) <> 5 THEN
        RAISE EXCEPTION 'FAIL  0017 down: a live credential did not stay live';
    END IF;
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix, label)
    VALUES ('00000000-0000-0000-0017-000000000261', 'api_token', 'h', 'dn17ruitok02', 'as the release before issues one');
    INSERT INTO credential (actor_id, kind, secret_hash)
    VALUES ('00000000-0000-0000-0017-000000000265', 'password', '$argon2id$stand-in');
END $chk$;
\echo 'PASS  0017 down with people signed in, an agent''s token, and what the up revoked, which stays revoked'

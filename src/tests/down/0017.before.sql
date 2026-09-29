-- AIshiteru Core — before 0017_api_tokens_for_agents.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with: a person who signs in, with a
-- password and a session; an agent with its token; and, from before the up
-- (tests/up/0017.before.sql), the tokens of people's and the passwords,
-- sessions, identities and invitations of agents' it revoked. None of the
-- wrong kind may be written, moved or brought back while 0017 is in.
-- Committed, so that the down migration runs over it; the downs after it
-- drop it with everything else.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

-- 261 Rui, a person · 265 a grading agent nobody owns
INSERT INTO actor (id, kind, display_name, email, created_by_actor_id) VALUES
    ('00000000-0000-0000-0017-000000000261', 'human', 'Rui', 'rui@down0017.example', NULL),
    ('00000000-0000-0000-0017-000000000265', 'agent', 'grader', NULL, NULL);
-- 2a1 Rui's password · 2a2 his session · 2b1 the agent's token
INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, expires_at) VALUES
    ('00000000-0000-0000-0017-0000000002a1', '00000000-0000-0000-0017-000000000261', 'password', '$argon2id$stand-in', NULL, NULL),
    ('00000000-0000-0000-0017-0000000002a2', '00000000-0000-0000-0017-000000000261', 'session', 'h', 'dn17ruisess1', now() + interval '12 hours'),
    ('00000000-0000-0000-0017-0000000002b1', '00000000-0000-0000-0017-000000000265', 'api_token', 'h', 'dn17grader01', NULL);

DO $chk$
DECLARE
    tries text[] := ARRAY[
        $$INSERT INTO credential (actor_id, kind, secret_hash, token_prefix)
          VALUES ('00000000-0000-0000-0017-000000000261', 'api_token', 'h', 'dn17ruitok01')$$,
        $$INSERT INTO credential (actor_id, kind, secret_hash)
          VALUES ('00000000-0000-0000-0017-000000000265', 'password', '$argon2id$stand-in')$$,
        $$UPDATE credential SET revoked_at = NULL WHERE id = '00000000-0000-0000-0017-0000000001b1'$$,
        $$UPDATE credential SET actor_id = '00000000-0000-0000-0017-000000000261' WHERE id = '00000000-0000-0000-0017-0000000002b1'$$];
    stmt text;
BEGIN
    FOREACH stmt IN ARRAY tries LOOP
        BEGIN
            EXECUTE stmt;
            RAISE EXCEPTION 'FAIL  0017: while it was in, the database took %', stmt;
        EXCEPTION WHEN check_violation THEN
            NULL;
        END;
    END LOOP;
END $chk$;

COMMIT;

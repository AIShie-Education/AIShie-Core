-- AIshiteru Core — after 0017_api_tokens_for_agents.up.sql, in `make db-test-sql`
--
-- Every API token a person held is revoked, root's from bootstrap among
-- them, and one revoked already keeps the date it was; every password,
-- session, identity and invitation an agent held is revoked. Nothing else
-- is: the agents keep their tokens, and the people their passwords,
-- sessions, identities and invitations.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
DECLARE
    wrong text;
BEGIN
    SELECT string_agg(right(id::text, 3) || ' ' || kind, ', ' ORDER BY id) INTO wrong
      FROM credential
     WHERE id IN ('00000000-0000-0000-0017-0000000001a1', '00000000-0000-0000-0017-0000000001b1',
                  '00000000-0000-0000-0017-0000000001b3', '00000000-0000-0000-0017-0000000001d3',
                  '00000000-0000-0000-0017-0000000001d4', '00000000-0000-0000-0017-0000000001d5',
                  '00000000-0000-0000-0017-0000000001e2')
       AND revoked_at IS NULL;
    IF wrong IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  0017 up: still live, and never to be: %', wrong;
    END IF;
    SELECT string_agg(right(id::text, 3) || ' ' || kind, ', ' ORDER BY id) INTO wrong
      FROM credential
     WHERE id IN ('00000000-0000-0000-0017-0000000001a2', '00000000-0000-0000-0017-0000000001b4',
                  '00000000-0000-0000-0017-0000000001b5', '00000000-0000-0000-0017-0000000001b6',
                  '00000000-0000-0000-0017-0000000001c1', '00000000-0000-0000-0017-0000000001d1',
                  '00000000-0000-0000-0017-0000000001d2', '00000000-0000-0000-0017-0000000001e1')
       AND revoked_at IS NOT NULL;
    IF wrong IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  0017 up: revoked, and not to be: %', wrong;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM credential WHERE id = '00000000-0000-0000-0017-0000000001b2'
                   AND revoked_at = '2026-01-01 00:00:00+00') THEN
        RAISE EXCEPTION 'FAIL  0017 up: a token revoked already was revoked again, and lost its date';
    END IF;
    -- Nothing but what it found: the revoked rows of the fixture are those
    -- eight, and no other row of its was touched.
    IF (SELECT count(*) FROM credential WHERE id::text LIKE '00000000-0000-0000-0017-0000000001%' AND revoked_at IS NOT NULL) <> 8 THEN
        RAISE EXCEPTION 'FAIL  0017 up: it revoked more, or fewer, than it should have';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'credential_fits_actor_kind') THEN
        RAISE EXCEPTION 'FAIL  0017 up: no trigger holds the rule';
    END IF;
END $chk$;
\echo 'PASS  0017 up revokes people''s API tokens, root''s among them, and agents'' passwords, sessions, identities and invitations, and nothing else'

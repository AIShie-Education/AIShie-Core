-- AIshiteru Core — after 0014_agent_owner_fixed.down.sql, in `make db-test-sql`
--
-- The refusal is gone, and with it nothing else: both agents are as they
-- were, and the release before this one may change their owners again, as
-- its actor.set_owner does.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'actor_owner_fixed')
       OR EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'actor_owner_unchanged') THEN
        RAISE EXCEPTION 'FAIL  0014 down: the trigger or its function is still there';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM actor WHERE id = '00000000-0000-0000-0014-000000000036'
                   AND owner_actor_id = '00000000-0000-0000-0014-000000000031')
       OR NOT EXISTS (SELECT 1 FROM actor WHERE id = '00000000-0000-0000-0014-000000000035' AND owner_actor_id IS NULL) THEN
        RAISE EXCEPTION 'FAIL  0014 down: an agent did not keep the owner it had';
    END IF;
    UPDATE actor SET owner_actor_id = '00000000-0000-0000-0014-000000000032' WHERE id = '00000000-0000-0000-0014-000000000036';
    UPDATE actor SET owner_actor_id = '00000000-0000-0000-0014-000000000031' WHERE id = '00000000-0000-0000-0014-000000000035';
    UPDATE actor SET owner_actor_id = NULL WHERE id = '00000000-0000-0000-0014-000000000036';
END $chk$;
\echo 'PASS  0014 down with an owned agent and one nobody owns'

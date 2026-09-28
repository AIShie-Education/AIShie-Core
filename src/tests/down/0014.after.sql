-- AIshiteru Core — after 0014_agent_owner_and_decisions.down.sql, in `make db-test-sql`
--
-- The two refusals are gone, and with them nothing else: both agents keep
-- their owners and their seats the levels they had, and the release before
-- this one may change their owners again, as its actor.set_owner does, and
-- give an agent action_decide as it likes.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_trigger WHERE tgname IN ('actor_owner_fixed', 'course_member_agent_ceiling'))
       OR EXISTS (SELECT 1 FROM pg_proc WHERE proname IN ('actor_owner_unchanged', 'course_member_agent_decides_by_proposal')) THEN
        RAISE EXCEPTION 'FAIL  0014 down: a trigger or its function is still there';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM actor WHERE id = '00000000-0000-0000-0014-000000000036'
                   AND owner_actor_id = '00000000-0000-0000-0014-000000000031')
       OR NOT EXISTS (SELECT 1 FROM actor WHERE id = '00000000-0000-0000-0014-000000000035' AND owner_actor_id IS NULL) THEN
        RAISE EXCEPTION 'FAIL  0014 down: an agent did not keep the owner it had';
    END IF;
    IF (SELECT array_agg(perm_action_decide::text ORDER BY id) FROM course_member
         WHERE course_id = '00000000-0000-0000-0014-000000000041')
       <> ARRAY['autonomous', 'confirm_required', 'confirm_required'] THEN
        RAISE EXCEPTION 'FAIL  0014 down: a seat did not keep its levels';
    END IF;
    UPDATE course_member SET perm_action_decide = 'autonomous' WHERE id = '00000000-0000-0000-0014-000000000055';
    IF NOT EXISTS (SELECT 1 FROM course_member WHERE id = '00000000-0000-0000-0014-000000000055' AND perm_action_decide = 'autonomous') THEN
        RAISE EXCEPTION 'FAIL  0014 down: an agent is still held to deciding by proposal';
    END IF;
    UPDATE actor SET owner_actor_id = '00000000-0000-0000-0014-000000000032' WHERE id = '00000000-0000-0000-0014-000000000035';
    UPDATE actor SET owner_actor_id = NULL WHERE id = '00000000-0000-0000-0014-000000000035';
END $chk$;
\echo 'PASS  0014 down with agents owned and not, seated deciding by proposal'

-- AIshie Core — after 0007_agent_ownership.down.sql, in `make db-test-sql`
--
-- The delegate seat is removed and its proposal cancelled as a removal
-- cancels one; the agent has lost its owner and the token its owner issued;
-- the built-in presets 0007's seed brought stay, less the permissions that
-- went.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM course_member WHERE id = '00000000-0000-0000-0007-00000000005a' AND status = 'removed') THEN
        RAISE EXCEPTION 'FAIL  0007 down: the delegate seat was not removed';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM course_member WHERE id = '00000000-0000-0000-0007-000000000052' AND status = 'active') THEN
        RAISE EXCEPTION 'FAIL  0007 down: the principal seat was touched';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM action WHERE id = '00000000-0000-0000-0007-0000000000b1' AND status = 'cancelled'
                   AND result->'error'->>'code' = 'failed_precondition'
                   AND result->'error'->'details'->>'reason' = 'member_removed') THEN
        RAISE EXCEPTION 'FAIL  0007 down: the delegate''s proposal was not cancelled as a removal cancels one';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM actor WHERE id = '00000000-0000-0000-0007-000000000038' AND kind = 'agent' AND status = 'active') THEN
        RAISE EXCEPTION 'FAIL  0007 down: the agent did not survive';
    END IF;
    IF EXISTS (SELECT 1 FROM credential WHERE id = '00000000-0000-0000-0007-0000000000c1' AND revoked_at IS NULL) THEN
        RAISE EXCEPTION 'FAIL  0007 down: the agent kept a token its owner may hold';
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public'
               AND column_name IN ('owner_actor_id', 'suspended_by_actor_id', 'principal_member_id',
                                   'perm_agent_delegate', 'perm_conversation_ask', 'perm_conversation_answer', 'answers_course')) THEN
        RAISE EXCEPTION 'FAIL  0007 down: a column it added is still there';
    END IF;
    IF (SELECT count(*) FROM permission_preset WHERE dept_id IS NULL AND name IN ('delegate', 'course_tutor')) <> 2 THEN
        RAISE EXCEPTION 'FAIL  0007 down: the delegate and course_tutor presets did not stay';
    END IF;
END $chk$;
\echo 'PASS  0007 down with a delegate seat present'

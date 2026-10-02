-- AIshie Core — after 0028_changes_requested.down.sql, in `make db-test-sql`
--
-- The draft sent back for changes is rejected, as the release before has
-- it, by Lin, with what to change as its reason; the revision still waits,
-- revising nothing the schema can say, and so does the decision proposed
-- about it. What held requests for changes and revisions is gone, and a
-- request for changes cannot be written.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
DECLARE
    got text;
BEGIN
    SELECT string_agg(right(id::text, 2) || '=' || status, ' ' ORDER BY id) INTO got
      FROM action WHERE id::text LIKE '00000000-0000-0000-0028-%';
    IF got IS DISTINCT FROM 'b1=rejected b2=rejected b6=executed b7=proposed b8=proposed' THEN
        RAISE EXCEPTION 'FAIL  0028 down: the actions are %', got;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM action WHERE id = '00000000-0000-0000-0028-0000000000b2'
                   AND decided_by_member_id = '00000000-0000-0000-0018-000000000051' AND decided_at IS NOT NULL
                   AND result->'decision'->>'reason' = 'Add the dressing steps.') THEN
        RAISE EXCEPTION 'FAIL  0028 down: the draft sent back does not say who asked for what';
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'action'
               AND column_name = 'revises_action_id')
       OR EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'action_revision_check')
       OR EXISTS (SELECT 1 FROM pg_constraint WHERE conname IN ('action_changes_requested_decided', 'action_revises_fk',
                                                                'action_not_own_revision')) THEN
        RAISE EXCEPTION 'FAIL  0028 down: a column, function or constraint it added is still there';
    END IF;
    BEGIN
        UPDATE action SET status = 'changes_requested' WHERE id = '00000000-0000-0000-0028-0000000000b7';
        RAISE EXCEPTION 'FAIL  0028 down: a proposal can still end in changes_requested';
    EXCEPTION WHEN check_violation THEN
        NULL;
    END;
END $chk$;
\echo 'PASS  0028 down with a proposal sent back for changes, which is rejected, and its revision, which waits'

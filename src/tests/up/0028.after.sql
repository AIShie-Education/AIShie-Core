-- AIshie Core — after 0028_changes_requested.up.sql, in `make db-test-sql`
--
-- Every action is as it was, revising nothing; what holds a request for
-- changes and a revision is there. A proposal is written as the release
-- before writes one, naming nothing it revises; Lin sends Ho's waiting
-- draft back for changes, and Ho's next names it, where one naming the
-- draft Lin rejected is refused; undone after.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
DECLARE
    got text;
BEGIN
    SELECT string_agg(right(id::text, 2) || '=' || status, ' ' ORDER BY id) INTO got
      FROM action WHERE id::text LIKE '00000000-0000-0000-0028-%';
    IF got IS DISTINCT FROM 'b1=rejected b2=proposed' THEN
        RAISE EXCEPTION 'FAIL  0028 up: the proposals are %', got;
    END IF;
    IF EXISTS (SELECT 1 FROM action WHERE revises_action_id IS NOT NULL) THEN
        RAISE EXCEPTION 'FAIL  0028 up: an action revises something';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'action_revises_own_changes_requested')
       OR NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'action_revises_fixed')
       OR NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'action_changes_requested_decided')
       OR NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'action_revises_fk') THEN
        RAISE EXCEPTION 'FAIL  0028 up: no trigger or constraint holds the rules';
    END IF;
END $chk$;

BEGIN;
-- As the release before writes a proposal: nothing it revises.
INSERT INTO action (id, actor_id, course_id, member_id, action_type, target_type, payload, payload_hash, idempotency_key,
                    authz_result, status)
VALUES ('00000000-0000-0000-0028-0000000000b3', '00000000-0000-0000-0018-000000000032', '00000000-0000-0000-0018-000000000041',
        '00000000-0000-0000-0018-000000000052', 'document.create', 'document', '{}', repeat('0', 64), 'up28-b3',
        'confirm_required', 'proposed');
UPDATE action SET status = 'changes_requested', decided_by_member_id = '00000000-0000-0000-0018-000000000051', decided_at = now(),
                  result = '{"decision": {"decision": "request_changes", "reason": "Add the dressing steps."}}'
WHERE id = '00000000-0000-0000-0028-0000000000b2' AND status = 'proposed';
INSERT INTO action (id, actor_id, course_id, member_id, action_type, target_type, payload, payload_hash, idempotency_key,
                    authz_result, status, revises_action_id)
VALUES ('00000000-0000-0000-0028-0000000000b4', '00000000-0000-0000-0018-000000000032', '00000000-0000-0000-0018-000000000041',
        '00000000-0000-0000-0018-000000000052', 'document.create', 'document', '{}', repeat('0', 64), 'up28-b4',
        'confirm_required', 'proposed', '00000000-0000-0000-0028-0000000000b2');
DO $chk$
BEGIN
    BEGIN
        INSERT INTO action (actor_id, course_id, member_id, action_type, target_type, payload, payload_hash, idempotency_key,
                            authz_result, status, revises_action_id)
        VALUES ('00000000-0000-0000-0018-000000000032', '00000000-0000-0000-0018-000000000041',
                '00000000-0000-0000-0018-000000000052', 'document.create', 'document', '{}', repeat('0', 64), 'up28-b5',
                'confirm_required', 'proposed', '00000000-0000-0000-0028-0000000000b1');
        RAISE EXCEPTION 'FAIL  0028 up: a proposal revises one that was rejected';
    EXCEPTION WHEN check_violation THEN
        NULL;
    END;
END $chk$;
ROLLBACK;
\echo 'PASS  0028 up leaves every action as it was, revising nothing, and holds what a request for changes and a revision are'

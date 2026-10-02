-- AIshie Core — before 0028_changes_requested.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with, in NUR101 of tests/up/0018:
-- the draft of tests/up/0028 that waited, sent back by Lin for changes,
-- saying what to change; Ho's revision of it, waiting; and a decision
-- proposed to send that one back too, waiting. Committed, so that the down
-- migration runs over it.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

-- b6 the decision Lin made · b7 Ho's revision · b8 a decision proposed about it
UPDATE action SET status = 'changes_requested', decided_by_member_id = '00000000-0000-0000-0018-000000000051', decided_at = now(),
                  result = '{"decision": {"decision": "request_changes", "reason": "Add the dressing steps.",
                             "by_action_id": "00000000-0000-0000-0028-0000000000b6"}}'
WHERE id = '00000000-0000-0000-0028-0000000000b2';
INSERT INTO action (id, actor_id, course_id, member_id, action_type, target_type, target_id, payload, payload_hash,
                    idempotency_key, authz_result, status, executed_at, revises_action_id) VALUES
    ('00000000-0000-0000-0028-0000000000b6', '00000000-0000-0000-0018-000000000031', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000051', 'action.decide', 'action', '00000000-0000-0000-0028-0000000000b2',
     '{"decision": "request_changes", "reason": "Add the dressing steps."}', repeat('0', 64), 'down28-b6', 'autonomous',
     'executed', now(), NULL),
    ('00000000-0000-0000-0028-0000000000b7', '00000000-0000-0000-0018-000000000032', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000052', 'document.create', 'document', NULL, '{"title": "Wound care, with dressings"}',
     repeat('0', 64), 'down28-b7', 'confirm_required', 'proposed', NULL, '00000000-0000-0000-0028-0000000000b2'),
    ('00000000-0000-0000-0028-0000000000b8', '00000000-0000-0000-0018-000000000036', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000056', 'action.decide', 'action', '00000000-0000-0000-0028-0000000000b7',
     '{"decision": "request_changes", "reason": "And the photos."}', repeat('0', 64), 'down28-b8', 'confirm_required',
     'proposed', NULL, NULL);

COMMIT;

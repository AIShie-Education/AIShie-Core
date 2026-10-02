-- AIshie Core — before 0028_changes_requested.up.sql, in `make db-test-sql`
--
-- What the migration finds, in NUR101 of tests/up/0018: a draft Ho
-- proposed that Lin rejected, saying why, and another of Ho's waiting for
-- a decision. Committed, so that the migration runs over it; it stays, for
-- the redo and the down after it (tests/down/0028.*), and the downs after
-- that drop it with everything else.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

-- b1 Ho's draft, rejected by Lin · b2 Ho's next, waiting
INSERT INTO action (id, actor_id, course_id, member_id, action_type, target_type, payload, payload_hash, idempotency_key,
                    authz_result, status, decided_by_member_id, decided_at, result) VALUES
    ('00000000-0000-0000-0028-0000000000b1', '00000000-0000-0000-0018-000000000032', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000052', 'document.create', 'document', '{"title": "Wound care"}', repeat('0', 64),
     'up28-b1', 'confirm_required', 'rejected', '00000000-0000-0000-0018-000000000051', now(),
     '{"decision": {"decision": "reject", "reason": "Not this week."}}'),
    ('00000000-0000-0000-0028-0000000000b2', '00000000-0000-0000-0018-000000000032', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000052', 'document.create', 'document', '{"title": "Wound care, again"}', repeat('0', 64),
     'up28-b2', 'confirm_required', 'proposed', NULL, NULL, NULL);

COMMIT;

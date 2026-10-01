-- AIshie Core — before 0025_agent_hosting.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with: the agent runtime service, a
-- credential of its live and one revoked, which issued Mei's coach, a
-- runtime agent, its token, kept pointed at by the coach's site chat
-- credential for the release before; and an agent registered as the
-- release before registers one, naming no hosting. Committed, so that the
-- down migration runs over it.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

-- 4a the service · 4c1 its credential · 4c2 one revoked · 3a Mei's coach · 3b an agent of the release before's
INSERT INTO actor (id, kind, display_name, service_scope, created_by_actor_id)
VALUES ('00000000-0000-0000-0025-00000000004a', 'service', 'Agent runtime', 'agent_runtime', '00000000-0000-0000-0025-000000000031');
INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, label, issued_by_actor_id, revoked_at) VALUES
    ('00000000-0000-0000-0025-0000000004c1', '00000000-0000-0000-0025-00000000004a', 'service', 'h', 'down25svc1', 'runtime',
     '00000000-0000-0000-0025-000000000031', NULL),
    ('00000000-0000-0000-0025-0000000004c2', '00000000-0000-0000-0025-00000000004a', 'service', 'h', 'down25svc2', 'old runtime',
     '00000000-0000-0000-0025-000000000031', '2026-09-01 00:00:00+00');
INSERT INTO actor (id, kind, display_name, owner_actor_id, created_by_actor_id, hosting) VALUES
    ('00000000-0000-0000-0025-00000000003a', 'agent', 'Coach', '00000000-0000-0000-0025-000000000031',
     '00000000-0000-0000-0025-000000000031', 'runtime');
INSERT INTO actor (id, kind, display_name, owner_actor_id, created_by_actor_id) VALUES
    ('00000000-0000-0000-0025-00000000003b', 'agent', 'Old style', '00000000-0000-0000-0025-000000000031',
     '00000000-0000-0000-0025-000000000031');
-- d1 the coach's runtime token
INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, label, issued_by_actor_id, issued_to_service) VALUES
    ('00000000-0000-0000-0025-0000000000d1', '00000000-0000-0000-0025-00000000003a', 'api_token', 'h', 'down25d1', 'agent runtime',
     '00000000-0000-0000-0025-00000000004a', 'agent_runtime');
UPDATE actor SET site_chat_credential_id = '00000000-0000-0000-0025-0000000000d1' WHERE id = '00000000-0000-0000-0025-00000000003a';
-- b1 the service's action, which names it
INSERT INTO action (id, actor_id, action_type, target_type, target_id, payload, payload_hash, idempotency_key, authz_result,
                    status, executed_at)
VALUES ('00000000-0000-0000-0025-0000000000b1', '00000000-0000-0000-0025-00000000004a', 'agent_runtime.issue_token', 'actor',
        '00000000-0000-0000-0025-00000000003a', '{}', repeat('0', 64), 'down25-b1', 'autonomous', 'executed', now());

COMMIT;

DO $chk$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM actor WHERE id = '00000000-0000-0000-0025-00000000003b' AND hosting = 'mcp') THEN
        RAISE EXCEPTION 'FAIL  0025 down: an agent registered naming no hosting is not an mcp agent';
    END IF;
END $chk$;

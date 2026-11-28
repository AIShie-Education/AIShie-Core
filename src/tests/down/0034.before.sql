-- AIshie Core — before 0034_mcp_oauth.down.sql, in make db-test-sql
-- Prove that down also removes populated OAuth tables. The agent itself stays.
\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;
INSERT INTO actor (id, kind, display_name, hosting)
VALUES ('00000000-0000-0000-0034-0000000000a1', 'agent', 'OAuth migration agent', 'mcp');
INSERT INTO oauth_client (id, agent_actor_id, label, secret_hash)
VALUES ('00000000-0000-0000-0034-0000000000c1', '00000000-0000-0000-0034-0000000000a1',
        'Migration client', 'sha256:' || repeat('1', 64));
INSERT INTO oauth_access_token (id, client_id, agent_actor_id, token_hash, scopes, created_at, expires_at)
VALUES ('00000000-0000-0000-0034-0000000000d1', '00000000-0000-0000-0034-0000000000c1',
        '00000000-0000-0000-0034-0000000000a1', 'sha256:' || repeat('2', 64), ARRAY['mcp.read'], now(), now() + interval '10 minutes');
COMMIT;

-- AIshie Core — after 0034_mcp_oauth.up.sql, in make db-test-sql
\set ON_ERROR_STOP 1
\set QUIET 1

DO $chk$
DECLARE missing text;
BEGIN
    SELECT string_agg(t, ', ') INTO missing
    FROM unnest(ARRAY['oauth_client', 'oauth_access_token']) AS t
    WHERE NOT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = t);
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  0034 up: no table %', missing;
    END IF;
    SELECT string_agg(i, ', ') INTO missing
    FROM unnest(ARRAY['oauth_client_agent_idx', 'oauth_access_token_client_idx',
                      'oauth_access_token_agent_idx', 'oauth_access_token_expiry_idx']) AS i
    WHERE NOT EXISTS (SELECT 1 FROM pg_class c JOIN pg_index x ON x.indexrelid = c.oid
                      WHERE c.relname = i AND x.indisvalid);
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  0034 up: no valid index %', missing;
    END IF;
END $chk$;

BEGIN;
INSERT INTO actor (id, kind, display_name, hosting)
VALUES ('00000000-0000-0000-0034-0000000000a1', 'agent', 'OAuth migration agent', 'mcp');
INSERT INTO oauth_client (id, agent_actor_id, label, secret_hash, scopes)
VALUES ('00000000-0000-0000-0034-0000000000c1', '00000000-0000-0000-0034-0000000000a1',
        'Migration client', 'sha256:' || repeat('1', 64), ARRAY['mcp.read', 'mcp.write']);
INSERT INTO oauth_access_token (id, client_id, agent_actor_id, token_hash, scopes, created_at, expires_at)
VALUES ('00000000-0000-0000-0034-0000000000d1', '00000000-0000-0000-0034-0000000000c1',
        '00000000-0000-0000-0034-0000000000a1', 'sha256:' || repeat('2', 64), ARRAY['mcp.read'], now(), now() + interval '10 minutes');

DO $chk$
BEGIN
    BEGIN
        INSERT INTO oauth_client (id, agent_actor_id, label, secret_hash, scopes)
        VALUES ('00000000-0000-0000-0034-0000000000c2', '00000000-0000-0000-0034-0000000000a1',
                'Bad scope', 'sha256:' || repeat('3', 64), ARRAY['course.admin']);
        RAISE EXCEPTION 'FAIL  0034 up: an unsupported client scope was accepted';
    EXCEPTION WHEN check_violation THEN
        -- Expected.
    END;
    BEGIN
        INSERT INTO oauth_access_token (id, client_id, agent_actor_id, token_hash, scopes, resource, created_at, expires_at)
        VALUES ('00000000-0000-0000-0034-0000000000d2', '00000000-0000-0000-0034-0000000000c1',
                '00000000-0000-0000-0034-0000000000a1', 'sha256:' || repeat('4', 64), ARRAY['mcp.read'],
                'https://elsewhere.example/mcp', now(), now() + interval '1 minute');
        RAISE EXCEPTION 'FAIL  0034 up: a non-MCP resource was accepted';
    EXCEPTION WHEN check_violation THEN
        -- Expected.
    END;
    BEGIN
        INSERT INTO oauth_access_token (id, client_id, agent_actor_id, token_hash, scopes, created_at, expires_at)
        VALUES ('00000000-0000-0000-0034-0000000000d3', '00000000-0000-0000-0034-0000000000c1',
                '00000000-0000-0000-0034-0000000000a1', 'sha256:' || repeat('5', 64), ARRAY['mcp.read'],
                now(), now() + interval '11 minutes');
        RAISE EXCEPTION 'FAIL  0034 up: an access token longer than ten minutes was accepted';
    EXCEPTION WHEN check_violation THEN
        -- Expected.
    END;
END $chk$;
ROLLBACK;
\echo 'PASS  0034 up stores only hash shapes, MCP scopes and tokens of at most ten minutes'

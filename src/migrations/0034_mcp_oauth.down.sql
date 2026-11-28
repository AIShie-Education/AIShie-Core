-- AIshie Core — migration 0034 (down)
-- Reverts 0034_mcp_oauth.up.sql.
--
-- Lost: every OAuth client, its secret hash, and every access-token hash and
-- record. Agent actors, their legacy credentials and all course data remain.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TABLE IF EXISTS oauth_access_token;
DROP TABLE IF EXISTS oauth_client;

COMMIT;

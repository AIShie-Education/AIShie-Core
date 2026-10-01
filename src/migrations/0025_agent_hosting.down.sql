-- AIshie Core — migration 0025 (down)
-- Reverts 0025_agent_hosting.up.sql.
--
-- Lost: each agent's hosting, and which of its tokens was issued to the
-- site's agent runtime. The release before this one asks in the site the
-- agents whose site chat credential is live, which this release kept
-- pointing at each runtime agent's runtime token: they are asked as they
-- were. A runtime token stays an ordinary API token of its agent's, and the
-- tokens 0025 revoked stay revoked. The agent runtime service, which that
-- release does not know, stays, as its actions name it, as 0020's down
-- leaves a service: an agent nobody owns, suspended, its credentials
-- revoked and kept as revoked API tokens, which authenticate nobody.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TRIGGER IF EXISTS credential_fits_hosting ON credential;
DROP FUNCTION IF EXISTS credential_check_hosting();
DROP TRIGGER IF EXISTS actor_hosting_fixed ON actor;
DROP FUNCTION IF EXISTS actor_hosting_unchanged();
DROP TRIGGER IF EXISTS actor_hosting_default ON actor;
DROP FUNCTION IF EXISTS actor_hosting_by_default();
DROP INDEX IF EXISTS credential_one_runtime_token;

ALTER TABLE credential
    DROP CONSTRAINT IF EXISTS credential_issued_to_service_valid,
    DROP COLUMN IF EXISTS issued_to_service;
ALTER TABLE actor
    DROP CONSTRAINT IF EXISTS actor_hosting_is_an_agents,
    DROP CONSTRAINT IF EXISTS actor_hosting_valid,
    DROP COLUMN IF EXISTS hosting;

-- The agent runtime service's credentials are revoked as they become API
-- tokens, which credential_fits_actor_kind lets a row that is revoked do;
-- then the service becomes an agent.
UPDATE credential c
   SET kind = 'api_token', revoked_at = coalesce(c.revoked_at, now())
  FROM actor a
 WHERE a.id = c.actor_id AND a.service_scope = 'agent_runtime' AND c.kind = 'service';
UPDATE actor SET kind = 'agent', service_scope = NULL, status = 'suspended' WHERE service_scope = 'agent_runtime';

ALTER TABLE actor
    DROP CONSTRAINT actor_service_scope_valid,
    ADD CONSTRAINT actor_service_scope_valid CHECK (service_scope IS NULL OR service_scope IN ('document_text'));

COMMIT;

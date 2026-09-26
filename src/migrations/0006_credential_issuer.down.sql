-- AIshiteru Core — migration 0006 (down)
-- Reverts 0006_credential_issuer.up.sql.

BEGIN;

ALTER TABLE credential DROP COLUMN IF EXISTS issued_by_actor_id;

COMMIT;

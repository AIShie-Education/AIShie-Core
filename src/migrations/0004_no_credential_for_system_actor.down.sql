-- AIshiteru Core — migration 0004 (down)
-- Reverts 0004_no_credential_for_system_actor.up.sql.

BEGIN;

DROP TRIGGER IF EXISTS credential_not_for_system_actor ON credential;
DROP FUNCTION IF EXISTS credential_reject_system_actor();

COMMIT;

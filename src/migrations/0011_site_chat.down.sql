-- AIshiteru Core — migration 0011 (down)
-- Reverts 0011_site_chat.up.sql.
--
-- Lost: which credential declared that an agent takes conversations in the
-- site. The release before this one asks any agent that answers, whatever
-- runs it. Kept: the agents and their credentials.

BEGIN;

SET LOCAL lock_timeout = '10s';

ALTER TABLE actor
    DROP CONSTRAINT IF EXISTS actor_site_chat_credential_fk,
    DROP CONSTRAINT IF EXISTS actor_site_chat_is_agent,
    DROP COLUMN IF EXISTS site_chat_credential_id;

ALTER TABLE credential DROP CONSTRAINT IF EXISTS credential_id_actor_key;

COMMIT;

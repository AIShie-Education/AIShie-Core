-- AIshiteru Core — migration 0014 (down)
-- Reverts 0014_agent_owner_fixed.up.sql.
--
-- Lost: only the refusal. The release before this one offers actor.set_owner,
-- and an administrator may change an agent's owner with it again. Kept: every
-- agent, with the owner it has.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TRIGGER IF EXISTS actor_owner_fixed ON actor;
DROP FUNCTION IF EXISTS actor_owner_unchanged();

COMMIT;

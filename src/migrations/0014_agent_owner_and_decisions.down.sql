-- AIshiteru Core — migration 0014 (down)
-- Reverts 0014_agent_owner_and_decisions.up.sql.
--
-- Lost: only the two refusals. The release before this one offers
-- actor.set_owner, and an administrator may change an agent's owner with it
-- again; and an agent's seat may be given action_decide above
-- confirm_required again. Kept: every agent, with the owner it has, and
-- every seat and preset with the levels it has: those the up lowered stay
-- lowered, since nothing says what they were.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TRIGGER IF EXISTS course_member_agent_ceiling ON course_member;
DROP FUNCTION IF EXISTS course_member_agent_decides_by_proposal();
DROP TRIGGER IF EXISTS actor_owner_fixed ON actor;
DROP FUNCTION IF EXISTS actor_owner_unchanged();

COMMIT;

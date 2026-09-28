-- AIshiteru Core — migration 0014 (up)
-- An agent's owner never changes. It is fixed when the agent is registered —
-- by the person who makes it their own (agent.create), or by an
-- administrator who registers it for someone (actor.register) — and an agent
-- registered with no owner stays nobody's. Nothing sets, changes or takes
-- away owner_actor_id afterwards, whoever asks, root included. So a delegate
-- seat always matches its agent's owner, what an agent keeps about its owner
-- is about the same person for as long as it is kept, and a token issued for
-- an agent is never in the hands of someone it no longer answers to. Design
-- reference: docs/schema.md §2.1. PostgreSQL 13+.
--
-- The previous release keeps working while this goes in, but for one tool:
-- its actor.set_owner, with which an administrator changed an agent's owner,
-- is refused by the database from now on, and fails having changed nothing.
-- What it changed before stays as it was left: a delegate seat an agent kept
-- in an archived course when it changed hands still counts for nothing, and
-- is removed once the course is opened again.

BEGIN;

-- Waits for a busy table rather than queueing every call behind it; a
-- deploy that times out here is run again.
SET LOCAL lock_timeout = '10s';

-- Every UPDATE, whatever it names: one that leaves the owner as it is, the
-- same person named again or null left null, passes.
CREATE FUNCTION actor_owner_unchanged() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'actor %: an agent''s owner is fixed when it is registered and never changes', OLD.id
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER actor_owner_fixed
    BEFORE UPDATE ON actor
    FOR EACH ROW
    WHEN (OLD.owner_actor_id IS DISTINCT FROM NEW.owner_actor_id)
    EXECUTE FUNCTION actor_owner_unchanged();

COMMIT;

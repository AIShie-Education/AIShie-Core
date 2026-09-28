-- AIshiteru Core — migration 0014 (up)
-- Two things about agents. An agent's owner never changes, and an agent,
-- owned or not, decides and reviews only by proposal. Design reference:
-- docs/schema.md §2.1, §2.2 (Ceilings). PostgreSQL 13+.
--
-- An agent's owner is fixed when the agent is registered — by the person who
-- makes it their own (agent.create), or by an administrator who registers it
-- for someone (actor.register) — and an agent registered with no owner stays
-- nobody's. Nothing sets, changes or takes away owner_actor_id afterwards,
-- whoever asks, root included. So a delegate seat always matches its agent's
-- owner, what an agent keeps about its owner is about the same person for as
-- long as it is kept, and a token issued for an agent is never in the hands
-- of someone it no longer answers to.
--
-- An agent holds action_decide at confirm_required at most: each decision
-- and each review of an agent's is itself a proposal, which a person
-- confirms (a triage assistant). The application holds every seat to it
-- when it is made and whenever it is widened, refusing a level named above
-- it, and caps a delegate by it on every call. The database holds an
-- agent's rows to it, since authorization never reads actor.kind and so
-- cannot cap an agent nobody owns: a seat of an agent's that is not removed
-- and would say more is written saying confirm_required. That reads kind to
-- limit, never to grant, as the refusals of ownership do. Seats that say
-- more now are lowered, once, here: every agent's seat that is not removed,
-- and every preset for agents, which carries no actor and is told by its
-- role, assistant, as 0013 told them — the built-ins for agents already deny
-- it. A removed seat is history, and is left as it was.
--
-- The previous release keeps working while this goes in, but for one tool
-- and one level. Its actor.set_owner, with which an administrator changed an
-- agent's owner, is refused by the database from now on, and fails having
-- changed nothing; what it changed before stays as it was left: a delegate
-- seat an agent kept in an archived course when it changed hands still
-- counts for nothing, and is removed once the course is opened again. And
-- an agent's seat it writes with action_decide above confirm_required — an
-- instructor's agent seated with the instructor preset, say — is written at
-- confirm_required instead, rather than refused: the call works, and gives
-- the agent what it may hold.

BEGIN;

-- Waits for a busy table rather than queueing every call behind it; a
-- deploy that times out here is run again.
SET LOCAL lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- actor: the owner never changes
-- ---------------------------------------------------------------------------

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

-- ---------------------------------------------------------------------------
-- course_member, permission_preset: an agent decides only by proposal
-- ---------------------------------------------------------------------------

UPDATE course_member m
   SET perm_action_decide = 'confirm_required'
  FROM actor a
 WHERE a.id = m.actor_id AND a.kind = 'agent'
   AND m.status <> 'removed' AND m.perm_action_decide > 'confirm_required';

UPDATE permission_preset
   SET perm_action_decide = 'confirm_required'
 WHERE role = 'assistant' AND perm_action_decide > 'confirm_required';

-- Cut down, not refused: the previous release's call goes through, with what
-- the agent may hold. The application refuses a level named above it before
-- anything is written.
CREATE FUNCTION course_member_agent_decides_by_proposal() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM actor WHERE id = NEW.actor_id AND kind = 'agent') THEN
        NEW.perm_action_decide := 'confirm_required';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER course_member_agent_ceiling
    BEFORE INSERT OR UPDATE ON course_member
    FOR EACH ROW
    WHEN (NEW.status <> 'removed' AND NEW.perm_action_decide > 'confirm_required')
    EXECUTE FUNCTION course_member_agent_decides_by_proposal();

COMMIT;

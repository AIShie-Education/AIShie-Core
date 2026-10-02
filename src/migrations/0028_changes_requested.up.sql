-- AIshie Core — migration 0028 (up)
-- A reviewer asks for changes to a proposal, and the proposer's next one
-- says which it revises. Design reference: docs/schema.md §2.6 (Asking for
-- changes). PostgreSQL 13+.
--
-- A proposal was approved, rejected or cancelled. It may now also end in
-- changes_requested: someone who could reject it asked its proposer to
-- change it, saying what in a note (result.decision.reason, 1 to 2000
-- characters), and nothing of it was carried out. It is as final as a
-- rejection; what follows is a new proposal, which names the one it
-- revises (revises_action_id): the proposer's own, in the same course, that
-- ended in changes_requested. A revision may be revised in turn, so the
-- chain runs back to the first proposal.
--
-- The release before this one keeps working on this schema: it never asks
-- for changes, writes no revises_action_id, and reads a proposal that ended
-- in changes_requested as it reads any other that is over.
--
-- NOT VALID, as in 0010: every row there is passes the constraints as they
-- were, and so these, which let through all that those did. Validating
-- would scan the whole log under a lock that stops every write. Each holds
-- for every row written from now on.

BEGIN;

SET LOCAL lock_timeout = '10s';

ALTER TABLE action
    DROP CONSTRAINT action_status_valid,
    ADD CONSTRAINT action_status_valid CHECK (
        status IN ('denied', 'proposed', 'approved', 'rejected', 'changes_requested', 'cancelled', 'executed', 'failed')
    ) NOT VALID,
    DROP CONSTRAINT action_status_matches_authz,
    ADD CONSTRAINT action_status_matches_authz CHECK (
            (authz_result = 'denied') = (status = 'denied')
        AND (status NOT IN ('proposed', 'rejected', 'changes_requested', 'cancelled') OR authz_result = 'confirm_required')
        AND (decided_by_member_id IS NULL OR authz_result = 'confirm_required')
        AND (review_state = 'none' OR (authz_result = 'pending_review' AND status = 'executed'))
    ) NOT VALID,
    -- Who asked, when, and what to change: a note of 1 to 2000 characters,
    -- where a rejection's reason is the decision's own business.
    ADD CONSTRAINT action_changes_requested_decided CHECK (
        status <> 'changes_requested'
        OR (decided_by_member_id IS NOT NULL AND decided_at IS NOT NULL
            AND char_length(result->'decision'->>'reason') BETWEEN 1 AND 2000)
    ) NOT VALID;

-- ---------------------------------------------------------------------------
-- action: the proposal a call revises
-- ---------------------------------------------------------------------------

ALTER TABLE action ADD COLUMN revises_action_id uuid;
ALTER TABLE action
    ADD CONSTRAINT action_revises_fk FOREIGN KEY (revises_action_id) REFERENCES action (id) NOT VALID,
    ADD CONSTRAINT action_not_own_revision CHECK (revises_action_id <> id) NOT VALID;

-- What a call revises is its actor's own proposal, in the same course, that
-- ended in changes_requested; that proposal never changes again (it is
-- over), so asking as the row is written is enough. Once written, what a
-- row revises never changes.
CREATE FUNCTION action_revision_check() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'action %: what an action revises is said when it is made and never changes', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM action r
                   WHERE r.id = NEW.revises_action_id AND r.actor_id = NEW.actor_id
                     AND r.course_id = NEW.course_id AND r.status = 'changes_requested') THEN
        RAISE EXCEPTION 'action % revises %, which is not a proposal of its actor''s in its course that ended in changes_requested (not_revisable)',
            NEW.id, NEW.revises_action_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER action_revises_own_changes_requested
    BEFORE INSERT ON action
    FOR EACH ROW
    WHEN (NEW.revises_action_id IS NOT NULL)
    EXECUTE FUNCTION action_revision_check();

CREATE TRIGGER action_revises_fixed
    BEFORE UPDATE ON action
    FOR EACH ROW
    WHEN (OLD.revises_action_id IS DISTINCT FROM NEW.revises_action_id)
    EXECUTE FUNCTION action_revision_check();

COMMIT;

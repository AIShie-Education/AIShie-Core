-- AIshie Core — migration 0028 (down)
-- Reverts 0028_changes_requested.up.sql.
--
-- Lost: which proposal a call revised, and that changes were asked for
-- rather than a proposal rejected. A proposal that ended in
-- changes_requested becomes rejected, as the release before has it: over,
-- nothing of it carried out, decided by whoever asked, with their note as
-- its reason (result.decision.reason), which the release before shows a
-- proposer as a rejection's. The news that changes were asked for
-- (action.changes_requested) stays in the feed, which is never rewritten:
-- the release before, which has no rule for it, shows it only to the
-- proposer. A decision proposed to ask for changes and still waiting stays
-- as it is: the release before refuses it if it is approved, as a decision
-- it does not know, and it can be rejected, withdrawn or left to expire.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TRIGGER IF EXISTS action_revises_fixed ON action;
DROP TRIGGER IF EXISTS action_revises_own_changes_requested ON action;
DROP FUNCTION IF EXISTS action_revision_check();
ALTER TABLE action
    DROP CONSTRAINT IF EXISTS action_not_own_revision,
    DROP CONSTRAINT IF EXISTS action_revises_fk,
    DROP COLUMN IF EXISTS revises_action_id,
    DROP CONSTRAINT IF EXISTS action_changes_requested_decided;

UPDATE action SET status = 'rejected' WHERE status = 'changes_requested';

-- NOT VALID, as the up made them: every row passes, now that none is in
-- changes_requested, and validating would scan the whole log under a lock
-- that stops every write.
ALTER TABLE action
    DROP CONSTRAINT action_status_valid,
    ADD CONSTRAINT action_status_valid CHECK (
        status IN ('denied', 'proposed', 'approved', 'rejected', 'cancelled', 'executed', 'failed')
    ) NOT VALID,
    DROP CONSTRAINT action_status_matches_authz,
    ADD CONSTRAINT action_status_matches_authz CHECK (
            (authz_result = 'denied') = (status = 'denied')
        AND (status NOT IN ('proposed', 'rejected', 'cancelled') OR authz_result = 'confirm_required')
        AND (decided_by_member_id IS NULL OR authz_result = 'confirm_required')
        AND (review_state = 'none' OR (authz_result = 'pending_review' AND status = 'executed'))
    ) NOT VALID;

COMMIT;

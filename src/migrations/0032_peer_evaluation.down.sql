-- AIshie Core — migration 0032 (down)
-- Reverts 0032_peer_evaluation.up.sql.
--
-- Each peer adjustment becomes a delta of the same points, its reason "peer
-- evaluation" and its author the grade's grader: the number is what the
-- student was given, and stays. Then the adjustment's kinds are 0031's
-- again, and the peer tables go.
--
-- Lost:
--   * peer forms, every sheet and its entries and comments;
--   * the factors behind peer adjustments (adjust_detail), and that they
--     were peer evaluation's rather than a grader's.

BEGIN;

SET LOCAL lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- 3. A member's grade: its peer adjustment a grader's delta
-- ---------------------------------------------------------------------------

DROP TRIGGER IF EXISTS grade_peer_adjustment_unsaid ON grade;
DROP FUNCTION IF EXISTS grade_peer_adjustment_unsay();

UPDATE grade
SET adjust_kind = 'delta', adjust_reason = 'peer evaluation', adjust_by_member_id = grader_member_id, adjust_detail = NULL
WHERE adjust_kind = 'peer';

ALTER TABLE grade
    DROP CONSTRAINT IF EXISTS grade_adjust_detail_of_peer,
    DROP CONSTRAINT IF EXISTS grade_adjust_peer_unsaid,
    DROP CONSTRAINT IF EXISTS grade_adjust_kind_valid,
    ADD CONSTRAINT grade_adjust_kind_valid CHECK (adjust_kind IS NULL OR adjust_kind IN ('replace', 'delta')) NOT VALID,
    DROP COLUMN IF EXISTS adjust_detail;

-- ---------------------------------------------------------------------------
-- 2. The sheets, and 1. the form
-- ---------------------------------------------------------------------------

-- Their triggers go with them.
DROP TABLE IF EXISTS peer_review_entry;
DROP FUNCTION IF EXISTS peer_review_entry_check_kept();
DROP TABLE IF EXISTS peer_review;
DROP FUNCTION IF EXISTS peer_review_check_kept();
DROP TABLE IF EXISTS peer_form;
DROP FUNCTION IF EXISTS peer_form_check_shape_fixed();
DROP FUNCTION IF EXISTS peer_criteria_valid(jsonb);

COMMIT;

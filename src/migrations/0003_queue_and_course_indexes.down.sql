-- AIshiteru Core — migration 0003 (down)
-- Reverts 0003_queue_and_course_indexes.up.sql.

BEGIN;

DROP INDEX IF EXISTS submission_course_idx;
DROP INDEX IF EXISTS action_review_queue_idx;
CREATE INDEX action_review_queue_idx ON action (course_id, created_at) WHERE review_state = 'pending';

COMMIT;

-- AIshiteru Core — migration 0007 (down)
-- Reverts 0007_agent_ownership.up.sql.
--
-- The schema before it has no delegates, so every delegate seat not already
-- removed is removed here, with its proposals cancelled as a removal cancels
-- them; its history stays. Owned agents lose their owner and keep their
-- credentials: they are ordinary agents again, seated nowhere, for an
-- administrator to seat or suspend. A suspension an owner made is kept, and
-- is an administrator's to lift from then on. The built-in presets delegate
-- and course_tutor stay: the schema before this holds them, less the three
-- permissions, which go from both tables.

BEGIN;

SET LOCAL lock_timeout = '10s';

-- Recorded as pipeline.Cancellation records a removal's (member_removed),
-- so that a replay of one of these proposals reads like any other.
UPDATE action
   SET status = 'cancelled',
       result = jsonb_build_object('error', jsonb_build_object(
           'code', 'failed_precondition',
           'message', 'the proposal can no longer be carried out',
           'details', jsonb_build_object('reason', 'member_removed', 'member_reason', 'delegates_withdrawn')))
 WHERE status = 'proposed'
   AND member_id IN (SELECT id FROM course_member WHERE principal_member_id IS NOT NULL AND status <> 'removed');

UPDATE course_member SET status = 'removed'
 WHERE principal_member_id IS NOT NULL AND status <> 'removed';

DROP TRIGGER IF EXISTS course_member_principal_valid ON course_member;
DROP FUNCTION IF EXISTS course_member_check_principal();
DROP TRIGGER IF EXISTS actor_suspension_cleared ON actor;
DROP FUNCTION IF EXISTS actor_clear_suspender();
DROP TRIGGER IF EXISTS actor_owner_valid ON actor;
DROP FUNCTION IF EXISTS actor_owner_is_person();

DROP INDEX IF EXISTS course_member_principal_idx;
DROP INDEX IF EXISTS actor_owner_idx;

ALTER TABLE permission_preset
    DROP COLUMN IF EXISTS perm_conversation_answer,
    DROP COLUMN IF EXISTS perm_conversation_ask,
    DROP COLUMN IF EXISTS perm_agent_delegate;

ALTER TABLE course_member
    DROP CONSTRAINT IF EXISTS course_member_principal_fk,
    DROP CONSTRAINT IF EXISTS course_member_not_own_principal,
    DROP CONSTRAINT IF EXISTS course_member_answers_course_is_delegate,
    DROP COLUMN IF EXISTS perm_conversation_answer,
    DROP COLUMN IF EXISTS perm_conversation_ask,
    DROP COLUMN IF EXISTS perm_agent_delegate,
    DROP COLUMN IF EXISTS answers_course,
    DROP COLUMN IF EXISTS principal_member_id;

ALTER TABLE actor
    DROP CONSTRAINT IF EXISTS actor_owned_is_agent,
    DROP CONSTRAINT IF EXISTS actor_not_own_owner,
    DROP COLUMN IF EXISTS suspended_by_actor_id,
    DROP COLUMN IF EXISTS owner_actor_id;

COMMIT;

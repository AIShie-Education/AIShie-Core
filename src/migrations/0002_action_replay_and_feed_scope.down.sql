-- AIshiteru Core — migration 0002 (down)
-- Reverts 0002_action_replay_and_feed_scope.up.sql. Destroys stored action
-- results and payload hashes, event scope columns and every login session.

BEGIN;

DROP INDEX IF EXISTS course_member_expiry_idx;

-- Sessions cannot exist under the old kind list. They are short-lived and
-- re-created by logging in again.
DELETE FROM credential WHERE kind = 'session';

ALTER TABLE credential
    DROP CONSTRAINT IF EXISTS credential_session_expires,
    DROP CONSTRAINT IF EXISTS credential_token_lookup,
    DROP CONSTRAINT IF EXISTS credential_kind_valid,
    ADD CONSTRAINT credential_kind_valid   CHECK (kind IN ('password', 'sso', 'api_token')),
    ADD CONSTRAINT credential_token_lookup CHECK (kind <> 'api_token' OR token_prefix IS NOT NULL);

-- ALTER TABLE ... DROP COLUMN is DDL and does not fire event's row triggers.
ALTER TABLE event
    DROP COLUMN IF EXISTS assignment_id,
    DROP COLUMN IF EXISTS student_member_id;

ALTER TABLE action
    DROP CONSTRAINT IF EXISTS action_executed_at_consistent,
    DROP CONSTRAINT IF EXISTS action_status_matches_authz,
    DROP CONSTRAINT IF EXISTS action_payload_hash_valid,
    DROP COLUMN IF EXISTS result,
    DROP COLUMN IF EXISTS payload_hash;

COMMIT;

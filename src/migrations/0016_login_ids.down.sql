-- AIshiteru Core — migration 0016 (down)
-- Reverts 0016_login_ids.up.sql.
--
-- Lost: every login ID, and whether anyone vouched for it, so that a person
-- who has no email, only a login ID, can no longer sign in with a password
-- until an administrator gives them an email; and which passwords are
-- temporary, so that one an instructor set works from then on as any
-- password does, and its person is no longer made to change it. Kept: the
-- people, their emails, their passwords, temporary ones included, and their
-- sessions; the actions and events that made them.

BEGIN;

SET LOCAL lock_timeout = '10s';

ALTER TABLE credential
    DROP CONSTRAINT IF EXISTS credential_must_change_is_an_issued_password,
    DROP COLUMN IF EXISTS must_change;

DROP INDEX IF EXISTS actor_login_id_key;
ALTER TABLE actor
    DROP CONSTRAINT IF EXISTS actor_unverified_login_id_is_a_persons,
    DROP CONSTRAINT IF EXISTS actor_login_id_is_a_persons,
    DROP CONSTRAINT IF EXISTS actor_login_id_valid,
    DROP COLUMN IF EXISTS login_id_verified,
    DROP COLUMN IF EXISTS login_id;

COMMIT;

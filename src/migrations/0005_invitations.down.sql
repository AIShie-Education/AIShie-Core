-- AIshiteru Core — migration 0005 (down)
-- Reverts 0005_invitations.up.sql. Invitations are deleted: the kind no
-- longer exists. Passwords set through them stay.

BEGIN;

DROP INDEX IF EXISTS credential_one_live_invite;
DELETE FROM credential WHERE kind = 'invite';
ALTER TABLE credential
    DROP CONSTRAINT IF EXISTS credential_invite_expires,
    DROP CONSTRAINT credential_kind_valid,
    DROP CONSTRAINT credential_token_lookup,
    ADD CONSTRAINT credential_kind_valid
        CHECK (kind IN ('password', 'sso', 'api_token', 'session')),
    ADD CONSTRAINT credential_token_lookup
        CHECK (kind NOT IN ('api_token', 'session') OR token_prefix IS NOT NULL);

COMMIT;

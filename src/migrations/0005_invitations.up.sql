-- AIshiteru Core — migration 0005 (up)
-- Invitations: how a person registered by an administrator comes to have a
-- password. PostgreSQL 13+.

BEGIN;

-- An invitation is a link, handed to the person, that sets their password
-- and signs them in. It is a credential like a token, found by its prefix
-- and checked against a hash, but it opens nothing else: it is taken for
-- setting the password, once, before it expires, and is revoked as it is
-- used. An administrator who invites someone again replaces the invitation:
-- an actor has one live invitation at most.
ALTER TABLE credential
    DROP CONSTRAINT credential_kind_valid,
    DROP CONSTRAINT credential_token_lookup,
    ADD CONSTRAINT credential_kind_valid
        CHECK (kind IN ('password', 'sso', 'api_token', 'session', 'invite')),
    ADD CONSTRAINT credential_token_lookup
        CHECK (kind NOT IN ('api_token', 'session', 'invite') OR token_prefix IS NOT NULL),
    ADD CONSTRAINT credential_invite_expires
        CHECK (kind <> 'invite' OR expires_at IS NOT NULL);

CREATE UNIQUE INDEX credential_one_live_invite ON credential (actor_id)
    WHERE kind = 'invite' AND revoked_at IS NULL;

COMMIT;

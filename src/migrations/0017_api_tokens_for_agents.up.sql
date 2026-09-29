-- AIshiteru Core — migration 0017 (up)
-- API tokens are for agents; signing in is for people. Design reference:
-- docs/schema.md §2.1. PostgreSQL 13+.
--
-- A person signs in: with a password, which they choose through an
-- invitation or a join link, or through single sign-on, and is given a
-- browser session for it. For tools and scripts they use one of their
-- agents, which holds a token of its own and never more than their seat. A
-- person holds no API token: one would be the whole of their account in a
-- file, lasting until someone thinks to revoke it. An agent holds API tokens
-- and nothing else: no password, no invitation to choose one, no identity at
-- a provider, and so never a session, which only signing in makes. The
-- system actor holds nothing (0004).
--
-- What is there already is revoked, once, here: every live API token of a
-- person's, root's from bootstrap among them, and every live password,
-- invitation, identity link and session of an agent's. Revoked, not deleted:
-- each row says when it stopped working. The credential table has no column
-- for who revoked a row or why: this migration is who, and this comment is
-- why. People's passwords, sessions, invitations and identities stay as they
-- are, and so do agents' tokens.
--
-- From now on the database holds the rule, as 0004 holds the system
-- actor's: a trigger, since a credential names its actor by id, which a
-- CHECK cannot follow. It refuses a person's API token and an agent's
-- password, invitation, identity or session when one is written, and when
-- an update would leave one live that moved to another actor, changed kind
-- or had its revocation taken back. Revoking one passes. It reads kind to
-- refuse, as actor_owned_is_agent (0007) does; nothing that grants reads it.
--
-- The previous release keeps working while this goes in, but for what it
-- would give the wrong actor. Its credential.issue_token called by a person,
-- its actor.issue_token and `aishiterud token issue` for one, and its
-- bootstrap, which issues root a token, fail having changed nothing; so do a
-- password, an invitation or an identity it would give an agent. What it
-- revoked stays revoked, and a script that signed in as a person with a
-- token signs in no longer, by either release.

BEGIN;

-- Waits for a busy table rather than queueing every call behind it; a
-- deploy that times out here is run again.
SET LOCAL lock_timeout = '10s';

UPDATE credential c
   SET revoked_at = now()
  FROM actor a
 WHERE a.id = c.actor_id
   AND c.revoked_at IS NULL
   AND ((a.kind = 'human' AND c.kind = 'api_token')
     OR (a.kind = 'agent' AND c.kind IN ('password', 'invite', 'sso', 'session')));

CREATE FUNCTION credential_check_actor_kind() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    holder text;
BEGIN
    -- A revocation, or anything else done to a row that stays revoked,
    -- opens nothing.
    IF TG_OP = 'UPDATE' AND NEW.revoked_at IS NOT NULL THEN
        RETURN NEW;
    END IF;
    SELECT kind INTO holder FROM actor WHERE id = NEW.actor_id;
    IF holder = 'human' AND NEW.kind = 'api_token' THEN
        RAISE EXCEPTION 'actor % is a person, who holds no API token: API tokens are for agents', NEW.actor_id
            USING ERRCODE = 'check_violation';
    END IF;
    IF holder = 'agent' AND NEW.kind IN ('password', 'invite', 'sso', 'session') THEN
        RAISE EXCEPTION 'actor % is an agent, which holds API tokens only and never signs in: no % credential', NEW.actor_id, NEW.kind
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER credential_fits_actor_kind
    BEFORE INSERT OR UPDATE OF actor_id, kind, revoked_at ON credential
    FOR EACH ROW EXECUTE FUNCTION credential_check_actor_kind();

COMMIT;

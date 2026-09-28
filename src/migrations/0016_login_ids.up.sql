-- AIshiteru Core — migration 0016 (up)
-- Signing in by a login ID, and a temporary password someone else set.
--
-- A login ID is a person's sign-in name other than an email: at a school
-- where most students have no mailbox, the student or staff number every
-- student and teacher has and remembers (學號, 工號), and by which a school's
-- own sign-on would one day know them. A person signs in with it or with
-- their email, whichever they have. Only people have one: agents, which sign
-- in with a token, and the system actor, which never signs in, do not.
--
-- A temporary password is one someone else set: an instructor who resets a
-- student's (member.reset_password), since a student with no email cannot
-- reset their own. It is marked, and until its person has set one of their
-- own they do nothing else. Design reference: docs/schema.md §2.1.
-- PostgreSQL 13+.
--
-- The previous release keeps working while this goes in. It neither reads
-- nor writes the columns: a person it registers has no login ID and its
-- default, vouched for; a password it sets is not temporary. A person with a
-- login ID and no email cannot sign in with it by that release's hand, and
-- a temporary password works there as any password does.

BEGIN;

-- Waits for a busy table rather than queueing every call behind it; a
-- deploy that times out here is run again.
SET LOCAL lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- actor: the login ID, and whether anyone but the person vouches for it
-- ---------------------------------------------------------------------------

-- 1 to 64 letters and digits of ASCII, dots, hyphens and underscores: what a
-- student or staff number is written in, with room for a school's prefix.
-- It holds no @, so that it is never taken for an email, nor an email for
-- it, and no space, so that what is kept is trimmed. Unique in any case, as
-- an email is (actor_login_id_key). Only a person has one: the CHECK reads
-- kind to refuse, as actor_owned_is_agent (0007) does; nothing that grants
-- reads it.
--
-- login_id_verified is email_verified (0012) for the login ID. true: an
-- administrator gave it (actor.register, actor.update), or there is none.
-- false: the person typed it, registering through a join link, and nobody
-- has checked that it is theirs. A school's sign-on matching its people to
-- ours by their numbers would take only a vouched-for one at its word.
ALTER TABLE actor
    ADD COLUMN login_id text,
    ADD COLUMN login_id_verified boolean NOT NULL DEFAULT true,
    ADD CONSTRAINT actor_login_id_valid
        CHECK (login_id ~ '^[0-9A-Za-z._-]{1,64}$'),
    ADD CONSTRAINT actor_login_id_is_a_persons
        CHECK (login_id IS NULL OR kind = 'human'),
    ADD CONSTRAINT actor_unverified_login_id_is_a_persons
        CHECK (login_id_verified OR (kind = 'human' AND login_id IS NOT NULL));
-- Case-insensitive, as actor_email_key: HNU2023001 and hnu2023001 are one
-- person.
CREATE UNIQUE INDEX actor_login_id_key ON actor (lower(login_id)) WHERE login_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- credential: a password its person must change
-- ---------------------------------------------------------------------------

-- Set on a password someone else set for its person, who issued it
-- (issued_by_actor_id, which a password otherwise leaves null): the next
-- sign-in with it must set a new password before anything else. Setting one
-- revokes it, as setting a password revokes every one before.
--
-- NOT VALID: every existing row holds false, which passes. Validating would
-- scan every credential, sessions and all, under the lock this migration
-- holds, which stops every sign-in. The CHECK holds for every row written
-- from now on.
ALTER TABLE credential
    ADD COLUMN must_change boolean NOT NULL DEFAULT false;
ALTER TABLE credential
    ADD CONSTRAINT credential_must_change_is_an_issued_password
        CHECK (NOT must_change OR (kind = 'password' AND issued_by_actor_id IS NOT NULL)) NOT VALID;

COMMIT;

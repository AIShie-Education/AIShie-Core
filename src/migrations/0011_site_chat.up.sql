-- AIshiteru Core — migration 0011 (up)
-- Which agents take conversations in the site. Only a program that runs an
-- agent and answers for it on its own — an agent runtime, which polls the
-- agent's inbox — says the agent does (me.site_chat), and it says so with the
-- credential it calls with. The agent takes them only while that credential
-- is live, the agent active and its owner, if it has one, active: so when the
-- runtime's token is revoked, as when its hosting ends, it stops by itself,
-- and no flag is left behind to say otherwise. An agent driven from outside,
-- by an assistant that acts only while a person uses it, never polls, and a
-- question put to it in the site would wait for good. Design reference:
-- docs/schema.md §2.1, §2.8. PostgreSQL 13+.
--
-- The previous release keeps working while this goes in. The column is new
-- and nullable, and the previous release neither reads nor writes it: while
-- it runs, any agent that answers may be asked in the site, as before.

BEGIN;

-- Waits for a busy table rather than queueing every call behind it; a
-- deploy that times out here is run again.
SET LOCAL lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- credential: a key on whose it is, for the one below
-- ---------------------------------------------------------------------------

-- id is unique already; this names (id, actor_id) together, so that a row
-- elsewhere can name a credential and whose it must be in one key.
ALTER TABLE credential ADD CONSTRAINT credential_id_actor_key UNIQUE (id, actor_id);

-- ---------------------------------------------------------------------------
-- actor: the credential that declared site chat
-- ---------------------------------------------------------------------------

-- Only an agent takes conversations in the site this way: a person answers
-- in the site as a person, and the system actor answers nobody. The CHECK
-- reads kind to refuse, as actor_owned_is_agent (0007) does; nothing that
-- grants reads it. The credential is the agent's own: the composite key
-- holds it whichever row changes. Whether it is live is read when it is
-- needed, never kept here.
ALTER TABLE actor
    ADD COLUMN site_chat_credential_id uuid,
    ADD CONSTRAINT actor_site_chat_is_agent CHECK (site_chat_credential_id IS NULL OR kind = 'agent'),
    ADD CONSTRAINT actor_site_chat_credential_fk
        FOREIGN KEY (site_chat_credential_id, id) REFERENCES credential (id, actor_id);

COMMIT;

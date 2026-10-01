-- AIshie Core — migration 0025 (up)
-- One hosting mode for each agent, chosen when it is registered and never
-- changed, and only the site's own agent runtime hosts. Design reference:
-- docs/schema.md §2.1 (Agents' hosting, Services), §2.8 (Who is asked in the
-- site). PostgreSQL 13+.
--
-- An agent is one of two things, for good. A runtime agent is run by the
-- site's own agent runtime and by nothing else: its owner holds no token for
-- it, and its one token is the one issued to the runtime, through the site
-- service agent_runtime, by the agent's id. People in the site ask it while
-- that token lives. An mcp agent is its owner's own tools' (a chat app, an
-- editor, a script, over MCP): its owner issues it tokens, as many as they
-- like, and nobody asks it in the site. Nothing but the runtime's token
-- makes an agent answer in the site, so no other program hosts one.
--
-- actor.hosting says which, for an agent, and only an agent; the database
-- holds that it never changes (actor_hosting_fixed). credential
-- .issued_to_service says a token was issued to the runtime
-- (agent_runtime): only a runtime agent holds one, it holds no other token,
-- and it holds one at most that is not revoked (credential_fits_hosting,
-- credential_one_runtime_token). The runtime is a site service like the
-- transcriber (0020), of scope agent_runtime.
--
-- What is there already: an agent whose site chat credential (0011) is live
-- is a runtime agent, and that credential, the token its runtime declared
-- with, is taken as the one issued to the runtime, so that it is asked in
-- the site as it was; every other token of its that is not revoked is
-- revoked, since a runtime agent's owner holds none. Every other agent is an
-- mcp agent, and keeps its tokens. Nothing else changes.
--
-- The previous release keeps working while this goes in, and after a
-- rollback. An agent it registers, knowing nothing of hosting, is an mcp
-- agent (actor_hosting_default): what that release offers its owner, tokens
-- of their own. It still reads actor.site_chat_credential_id to say who is
-- asked in the site, and this release keeps that column in step for it,
-- pointing at a runtime agent's live runtime token, and reads it nowhere; a
-- later migration drops it. Issuing a runtime agent a token of the owner's,
-- that release fails having changed nothing (hosted_by_runtime).

BEGIN;

-- Waits for a busy table rather than queueing every call behind it; a
-- deploy that times out here is run again.
SET LOCAL lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- The site's agent runtime is a site service
-- ---------------------------------------------------------------------------

ALTER TABLE actor
    DROP CONSTRAINT actor_service_scope_valid,
    ADD CONSTRAINT actor_service_scope_valid
        CHECK (service_scope IS NULL OR service_scope IN ('document_text', 'agent_runtime'));

-- ---------------------------------------------------------------------------
-- actor.hosting, credential.issued_to_service
-- ---------------------------------------------------------------------------

ALTER TABLE actor
    ADD COLUMN hosting text,
    ADD CONSTRAINT actor_hosting_valid CHECK (hosting IS NULL OR hosting IN ('runtime', 'mcp'));

-- An API token issued to a site service for the agent it is a token of:
-- agent_runtime, the site's agent runtime, which runs the agent with it.
ALTER TABLE credential
    ADD COLUMN issued_to_service text,
    ADD CONSTRAINT credential_issued_to_service_valid
        CHECK (issued_to_service IS NULL OR (issued_to_service IN ('agent_runtime') AND kind = 'api_token'));

-- ---------------------------------------------------------------------------
-- What is there already
-- ---------------------------------------------------------------------------

-- A runtime agent is one whose site chat credential is live: an API token
-- of its own, neither revoked nor expired, with which what runs it said it
-- answers in the site.
UPDATE actor a
   SET hosting = CASE WHEN EXISTS (SELECT 1 FROM credential c
                                    WHERE c.id = a.site_chat_credential_id AND c.actor_id = a.id AND c.kind = 'api_token'
                                      AND c.revoked_at IS NULL AND (c.expires_at IS NULL OR c.expires_at > now()))
                      THEN 'runtime' ELSE 'mcp' END
 WHERE a.kind = 'agent';

-- That credential is the runtime's token.
UPDATE credential c
   SET issued_to_service = 'agent_runtime'
  FROM actor a
 WHERE a.hosting = 'runtime' AND c.id = a.site_chat_credential_id AND c.actor_id = a.id;

-- And it is a runtime agent's only token: the others are revoked, a token
-- revoked already keeping its date.
UPDATE credential c
   SET revoked_at = now()
  FROM actor a
 WHERE a.hosting = 'runtime' AND c.actor_id = a.id AND c.kind = 'api_token'
   AND c.issued_to_service IS NULL AND c.revoked_at IS NULL;

-- Only an agent has a hosting, and every agent has one.
ALTER TABLE actor ADD CONSTRAINT actor_hosting_is_an_agents CHECK ((kind = 'agent') = (hosting IS NOT NULL));

-- At most one runtime token of an agent's is not revoked: issuing another
-- revokes it.
CREATE UNIQUE INDEX credential_one_runtime_token ON credential (actor_id)
    WHERE issued_to_service IS NOT NULL AND revoked_at IS NULL;

-- ---------------------------------------------------------------------------
-- An agent registered by the release before is an mcp agent
-- ---------------------------------------------------------------------------

-- This release names the hosting of every agent it registers. The release
-- before names none, and an agent of its is what it offers the owner: an
-- agent of tokens of their own.
CREATE FUNCTION actor_hosting_by_default() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.kind = 'agent' AND NEW.hosting IS NULL THEN
        NEW.hosting := 'mcp';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER actor_hosting_default
    BEFORE INSERT ON actor
    FOR EACH ROW EXECUTE FUNCTION actor_hosting_by_default();

-- ---------------------------------------------------------------------------
-- An agent's hosting never changes
-- ---------------------------------------------------------------------------

-- Every UPDATE, whatever it names: one that leaves it as it is passes.
CREATE FUNCTION actor_hosting_unchanged() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'actor %: an agent''s hosting is chosen when it is registered and never changes (hosting_fixed)', OLD.id
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER actor_hosting_fixed
    BEFORE UPDATE ON actor
    FOR EACH ROW
    WHEN (OLD.hosting IS DISTINCT FROM NEW.hosting)
    EXECUTE FUNCTION actor_hosting_unchanged();

-- ---------------------------------------------------------------------------
-- A token fits its agent's hosting
-- ---------------------------------------------------------------------------

-- A runtime agent's only token is one issued to the runtime, and only a
-- runtime agent's is. Whom a token was issued to never changes. A
-- revocation, or anything else done to a row that stays revoked, opens
-- nothing, and passes, as credential_fits_actor_kind lets it (0017).
CREATE FUNCTION credential_check_hosting() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    hosted text;
BEGIN
    IF TG_OP = 'UPDATE' AND OLD.issued_to_service IS DISTINCT FROM NEW.issued_to_service THEN
        RAISE EXCEPTION 'credential %: whom a token was issued to never changes', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF (TG_OP = 'UPDATE' AND NEW.revoked_at IS NOT NULL) OR NEW.kind <> 'api_token' THEN
        RETURN NEW;
    END IF;
    SELECT hosting INTO hosted FROM actor WHERE id = NEW.actor_id;
    IF NEW.issued_to_service IS NOT NULL AND hosted IS DISTINCT FROM 'runtime' THEN
        RAISE EXCEPTION 'actor % is no runtime agent: only a runtime agent is issued a token for the site''s agent runtime (not_runtime_hosted)',
            NEW.actor_id
            USING ERRCODE = 'check_violation';
    END IF;
    IF hosted = 'runtime' AND NEW.issued_to_service IS NULL THEN
        RAISE EXCEPTION 'actor % is a runtime agent, whose one token is the one issued to the site''s agent runtime (hosted_by_runtime)',
            NEW.actor_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER credential_fits_hosting
    BEFORE INSERT OR UPDATE ON credential
    FOR EACH ROW EXECUTE FUNCTION credential_check_hosting();

COMMIT;

-- AIshie Core — migration 0020 (down)
-- Reverts 0020_document_text.up.sql.
--
-- Lost: every text version, the text staff wrote among them, which the
-- release before this one neither keeps nor reads; and services. A service
-- actor stays, as its actions name it, as what that release knows: an agent
-- nobody owns, suspended, its credentials revoked and kept as revoked API
-- tokens, which authenticate nobody. Kept: every document and version as it
-- is.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TABLE IF EXISTS document_version_text;
DROP TRIGGER IF EXISTS document_version_text_queued ON document_version;
DROP TRIGGER IF EXISTS document_version_text_purged ON document_version;
DROP FUNCTION IF EXISTS document_version_queue_text();
DROP FUNCTION IF EXISTS document_version_forget_text();
DROP FUNCTION IF EXISTS document_version_text_guarded();
ALTER TABLE document DROP CONSTRAINT IF EXISTS document_id_course_key;

DROP TRIGGER IF EXISTS course_member_not_a_service ON course_member;
DROP FUNCTION IF EXISTS course_member_reject_service();

-- A service's credentials are revoked as they become API tokens, which the
-- rule lets a row that is revoked do; then the service becomes an agent.
UPDATE credential SET kind = 'api_token', revoked_at = coalesce(revoked_at, now()) WHERE kind = 'service';
UPDATE actor SET kind = 'agent', service_scope = NULL, status = 'suspended' WHERE kind = 'service';

CREATE OR REPLACE FUNCTION credential_check_actor_kind() RETURNS trigger
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

ALTER TABLE credential
    DROP CONSTRAINT credential_kind_valid,
    DROP CONSTRAINT credential_token_lookup,
    ADD CONSTRAINT credential_kind_valid
        CHECK (kind IN ('password', 'sso', 'api_token', 'session', 'invite')),
    ADD CONSTRAINT credential_token_lookup
        CHECK (kind NOT IN ('api_token', 'session', 'invite') OR token_prefix IS NOT NULL);

DROP INDEX IF EXISTS actor_service_scope_key;
ALTER TABLE actor
    DROP CONSTRAINT actor_service_holds_no_account,
    DROP CONSTRAINT actor_service_is_scoped,
    DROP CONSTRAINT actor_service_scope_valid,
    DROP CONSTRAINT actor_kind_valid,
    ADD CONSTRAINT actor_kind_valid CHECK (kind IN ('human', 'agent', 'system')),
    DROP COLUMN service_scope;

COMMIT;

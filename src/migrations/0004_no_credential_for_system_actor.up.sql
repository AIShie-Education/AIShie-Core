-- AIshiteru Core — migration 0004 (up)
-- The system actor holds no credential. PostgreSQL 13+.

BEGIN;

-- The system actor is who the background sweeps act as. Nobody signs in as
-- it and no token is issued for it, so that its authority cannot be
-- borrowed: a token of its would act as 'system', and could take the sweeps'
-- idempotency keys before they do. A credential names its actor by id, which
-- a CHECK cannot follow, so a trigger holds the rule, for a new row and for a
-- row moved to another actor. Rows written before this migration are left
-- as they are; the server refuses them when they are presented.
CREATE FUNCTION credential_reject_system_actor() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM actor WHERE id = NEW.actor_id AND kind = 'system') THEN
        RAISE EXCEPTION 'actor % is the system actor, which holds no credential', NEW.actor_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER credential_not_for_system_actor
    BEFORE INSERT OR UPDATE OF actor_id ON credential
    FOR EACH ROW EXECUTE FUNCTION credential_reject_system_actor();

COMMIT;

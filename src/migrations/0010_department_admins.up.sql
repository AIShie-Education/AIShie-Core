-- AIshiteru Core — migration 0010 (up)
-- Departments become a tree, and each may have administrators, who manage the
-- courses of the department and of every department beneath it as a platform
-- administrator manages courses: from outside them. An administrator of a
-- department holds nothing inside its courses; one who wants to work in a
-- course is seated there like anyone else. Design reference: docs/schema.md
-- §2.1, §2.9. PostgreSQL 13+.
--
-- The previous release keeps working while this goes in. Every column is new
-- and nullable, and nothing it writes is refused: it knows no tree, so every
-- department it creates is at the top, and it reads no appointment, so while
-- it runs a department administrator may do there only what they could
-- before they were appointed.

BEGIN;

-- Waits for a busy table rather than queueing every call behind it; a
-- deploy that times out here is run again.
SET LOCAL lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- department: a tree
-- ---------------------------------------------------------------------------

-- parent_id null is a department at the top of the tree. Supersedes 0001's
-- "Takes no part in authorization": an appointment at a department reaches
-- every department beneath it, and their courses.
ALTER TABLE department
    ADD COLUMN parent_id uuid REFERENCES department (id),
    ADD CONSTRAINT department_not_own_parent CHECK (parent_id <> id);
CREATE INDEX department_parent_idx ON department (parent_id);

-- No cycle and no more than 8 levels (domain.MaxDepartmentDepth; a test
-- compares the two). It reads the tree as committed. The application takes
-- the tree lock (pg_advisory_xact_lock(1095324500, 0)) before any change to
-- the tree's shape, so two moves never pass each other unseen.
CREATE FUNCTION department_check_tree() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    above  integer := 0;      -- levels from the new parent up to the top, the parent included
    below  integer;           -- levels of the row's own subtree, the row included
    cyclic boolean := false;
BEGIN
    IF NEW.parent_id IS NOT NULL THEN
        WITH RECURSIVE up (id, parent_id, n) AS (
            SELECT id, parent_id, 1 FROM department WHERE id = NEW.parent_id
          UNION ALL
            SELECT d.id, d.parent_id, up.n + 1
            FROM department d JOIN up ON d.id = up.parent_id
            WHERE up.id <> NEW.id AND up.n < 64
        )
        SELECT max(n), coalesce(bool_or(id = NEW.id), false) INTO above, cyclic FROM up;
    END IF;
    IF cyclic THEN
        RAISE EXCEPTION 'department %: it cannot be placed under itself or under a department beneath it', NEW.id
            USING ERRCODE = 'check_violation';
    END IF;
    WITH RECURSIVE down (id, n) AS (
        SELECT NEW.id, 1
      UNION ALL
        SELECT d.id, down.n + 1 FROM department d JOIN down ON d.parent_id = down.id WHERE down.n < 64
    )
    SELECT max(n) INTO below FROM down;
    IF above + below > 8 THEN
        RAISE EXCEPTION 'department %: the tree would be % levels deep; at most 8', NEW.id, above + below
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER department_tree_valid
    BEFORE INSERT OR UPDATE OF parent_id ON department
    FOR EACH ROW EXECUTE FUNCTION department_check_tree();

-- ---------------------------------------------------------------------------
-- department_admin: who administers what, since when, appointed by whom
-- ---------------------------------------------------------------------------

-- A row is an appointment. Ending it sets removed_at and removed_by: the row
-- stays, so the history of who could manage what, and who decided it, is
-- kept. Appointing the same person again is a new row.
CREATE TABLE department_admin (
    id                    uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    dept_id               uuid        NOT NULL REFERENCES department (id),
    actor_id              uuid        NOT NULL REFERENCES actor (id),
    appointed_by_actor_id uuid        NOT NULL REFERENCES actor (id),
    appointed_at          timestamptz NOT NULL DEFAULT now(),
    removed_by_actor_id   uuid        REFERENCES actor (id),
    removed_at            timestamptz,
    CONSTRAINT department_admin_removal_recorded CHECK ((removed_at IS NULL) = (removed_by_actor_id IS NULL)),
    CONSTRAINT department_admin_not_self_appointed CHECK (appointed_by_actor_id <> actor_id)
);
-- One live appointment per person and department.
CREATE UNIQUE INDEX department_admin_one_live ON department_admin (dept_id, actor_id) WHERE removed_at IS NULL;
-- "Does this caller administer anything", on every call; and "what do they administer".
CREATE INDEX department_admin_actor_live_idx ON department_admin (actor_id, dept_id) WHERE removed_at IS NULL;

-- An administrator is a person: never an agent, never the system actor. A
-- person is nobody's (actor_owned_is_agent), so an owned agent can never
-- hold one. Nothing that grants reads kind; this refusal does, as
-- actor_owner_is_person (0007) does. An appointment is kept as it was
-- written: only its removal is ever recorded, once.
CREATE FUNCTION department_admin_check() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'department_admin %: appointments are kept; end one by setting removed_at', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF TG_OP = 'UPDATE' THEN
        IF OLD.removed_at IS NOT NULL THEN
            RAISE EXCEPTION 'department_admin %: it has ended and stays so; appoint again instead', OLD.id
                USING ERRCODE = 'restrict_violation';
        END IF;
        IF NEW.id <> OLD.id OR NEW.dept_id <> OLD.dept_id OR NEW.actor_id <> OLD.actor_id
           OR NEW.appointed_by_actor_id <> OLD.appointed_by_actor_id OR NEW.appointed_at <> OLD.appointed_at THEN
            RAISE EXCEPTION 'department_admin %: only its ending is recorded', OLD.id
                USING ERRCODE = 'restrict_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM actor WHERE id = NEW.actor_id AND kind = 'human') THEN
        RAISE EXCEPTION 'actor % cannot administer a department: an administrator is a person', NEW.actor_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER department_admin_guarded
    BEFORE INSERT OR UPDATE OR DELETE ON department_admin
    FOR EACH ROW EXECUTE FUNCTION department_admin_check();

-- ---------------------------------------------------------------------------
-- action: in what capacity it was authorized
-- ---------------------------------------------------------------------------

-- authority is 'platform' when a platform role passed the call, and
-- 'department' when a department administrator's appointment did: a call
-- about departments or about courses as a whole, never one inside a course.
-- authority_dept_id is then the department of the appointment relied on,
-- the nearest to what was acted on, or null for a call about no one
-- department. Null authority means the caller's own seat, or their own
-- account.
--
-- NOT VALID: every existing row holds nulls, which pass. Validating would
-- scan the whole log under a lock that stops every write. Both constraints
-- hold for every row written from now on.
ALTER TABLE action
    ADD COLUMN authority text,
    ADD COLUMN authority_dept_id uuid;
ALTER TABLE action
    ADD CONSTRAINT action_authority_valid CHECK (
        (authority IS NULL OR authority IN ('platform', 'department'))
        AND (authority_dept_id IS NULL OR authority = 'department')) NOT VALID,
    ADD CONSTRAINT action_authority_dept_fk FOREIGN KEY (authority_dept_id) REFERENCES department (id) NOT VALID;

COMMIT;

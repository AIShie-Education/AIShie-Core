-- AIshie Core — migration 0030 (down)
-- Reverts 0030_assignment_deletion.up.sql.
--
-- The guards are put back as 0029 left them: events append-only, a
-- submission frozen once submitted (and a draft deletable again, as 0001
-- had it), a version never deleted; grades, documents and assignments with
-- no guard of their own, as before. The queue and the record of deletions
-- go, and so do the indexes this migration made.
--
-- Lost:
--   * which actions a deletion emptied: they stay empty, since what was
--     deleted cannot come back, and no longer say by which deletion;
--   * the deletions' own rows: each deletion's action keeps its assignment,
--     its title and its counts (its payload and result);
--   * the queue of files not yet deleted from the store: nothing names them
--     any more, and the orphan sweep removes them once PROPOSAL_TTL and two
--     days have passed (never, without a TTL).

BEGIN;

SET LOCAL lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- The guards, as 0029 left them
-- ---------------------------------------------------------------------------

-- event: as 0001 made it.
DROP TRIGGER IF EXISTS event_append_only ON event;
CREATE TRIGGER event_append_only
    BEFORE UPDATE OR DELETE ON event
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();
DROP FUNCTION IF EXISTS event_guarded();

-- submission: as 0001 made it.
CREATE OR REPLACE FUNCTION submission_reject_change_after_submit() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    expected submission;
BEGIN
    IF OLD.state NOT IN ('submitted', 'late') THEN
        IF TG_OP = 'DELETE' THEN
            RETURN OLD;
        END IF;
        RETURN NEW;
    END IF;

    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'submission % has been submitted: DELETE is not allowed', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;

    expected := NEW;
    expected.state := OLD.state;
    IF NEW.state NOT IN ('submitted', 'late') OR expected IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'submission % has been submitted: only submitted <-> late may change; resubmit as a new attempt', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS submission_no_truncate ON submission;

-- document_version: as 0027 made it.
CREATE OR REPLACE FUNCTION document_version_guarded() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    expected document_version;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'document_version is append-only: DELETE is not allowed'
            USING ERRCODE = 'restrict_violation';
    END IF;
    expected := OLD;
    expected.body_md := NULL;
    expected.purged_at := NEW.purged_at;
    expected.purged_by_actor_id := NEW.purged_by_actor_id;
    expected.purge_reason := NEW.purge_reason;
    IF OLD.purged_at IS NOT NULL OR NEW.purged_at IS NULL OR NEW IS DISTINCT FROM expected THEN
        RAISE EXCEPTION 'document_version is append-only: a version changes only by being purged, once'
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS grade_kept ON grade;
DROP TRIGGER IF EXISTS grade_no_truncate ON grade;
DROP FUNCTION IF EXISTS grade_check_kept();

DROP TRIGGER IF EXISTS document_kept ON document;
DROP TRIGGER IF EXISTS document_no_truncate ON document;
DROP FUNCTION IF EXISTS document_check_kept();
DROP FUNCTION IF EXISTS document_owner_assignment(uuid);

DROP TRIGGER IF EXISTS assignment_kept ON assignment;
DROP TRIGGER IF EXISTS assignment_not_reused ON assignment;
DROP TRIGGER IF EXISTS assignment_no_truncate ON assignment;
DROP FUNCTION IF EXISTS assignment_check_kept();

-- ---------------------------------------------------------------------------
-- The record, the queue and the stubs' marks
-- ---------------------------------------------------------------------------

-- Its triggers go with it.
DROP TABLE IF EXISTS assignment_deletion;
DROP FUNCTION IF EXISTS assignment_deletion_check_whole();
DROP FUNCTION IF EXISTS assignment_being_deleted(uuid);

DROP TABLE IF EXISTS blob_deletion;

ALTER TABLE action
    DROP CONSTRAINT IF EXISTS action_redacted_empty,
    DROP CONSTRAINT IF EXISTS action_redacted_by_fk,
    DROP COLUMN IF EXISTS redacted_by_action_id;

-- ---------------------------------------------------------------------------
-- Indexes
-- ---------------------------------------------------------------------------

DROP INDEX IF EXISTS event_assignment_idx;
DROP INDEX IF EXISTS grade_superseded_by_idx;
DROP INDEX IF EXISTS submission_instructions_version_idx;
DROP INDEX IF EXISTS grade_rubric_version_idx;
DROP INDEX IF EXISTS action_course_idx;
DROP INDEX IF EXISTS assignment_instructions_document_idx;
DROP INDEX IF EXISTS assignment_rubric_document_idx;
DROP INDEX IF EXISTS conversation_message_source_document_idx;
DROP INDEX IF EXISTS conversation_message_source_version_idx;

COMMIT;

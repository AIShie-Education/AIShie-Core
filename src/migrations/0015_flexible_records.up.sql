-- AIshiteru Core — migration 0015 (up)
-- Records that were set in stone become correctable, each with its history
-- kept. A computed total may be overridden by a person, and the override is
-- kept beside the number worked out, never in its place. A document, or one
-- version of it, uploaded by mistake — personal data, say — may be purged:
-- its text and its file go, and what is left says who removed them, when and
-- why. Design reference: docs/schema.md §2.4, §2.7. PostgreSQL 13+.
--
-- The previous release keeps working while this goes in. Every column is new
-- and nullable, and it writes nothing the migration refuses: it purges
-- nothing, and it overrides nothing. What it does not know of it does not
-- keep: a total it writes again, when a grade beneath is posted, carries no
-- override and no comment, though the one it supersedes keeps them; and a
-- purged version reads to it as a version with no text and no file.

BEGIN;

-- Waits for a busy table rather than queueing every call behind it; a
-- deploy that times out here is run again.
SET LOCAL lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- grade: an override of a computed total, beside it
-- ---------------------------------------------------------------------------

-- A computed total's score stays what the scheme works out; an override is a
-- person's number for it, with their reason, and counts in its place for
-- whatever is rolled up above it. When the total is written again, the new
-- row carries the override on. Clearing it is a new row without one.
--
-- NOT VALID: every existing row holds nulls, which pass. Validating would
-- scan every grade under a lock that stops every write. Each constraint holds
-- for every row written from now on.
ALTER TABLE grade
    ADD COLUMN override_score numeric,
    ADD COLUMN override_reason text,
    ADD COLUMN override_by_member_id uuid,
    ADD COLUMN overridden_at timestamptz;
ALTER TABLE grade
    ADD CONSTRAINT grade_override_whole CHECK (
        (override_score IS NULL) = (override_reason IS NULL)
        AND (override_score IS NULL) = (override_by_member_id IS NULL)
        AND (override_score IS NULL) = (overridden_at IS NULL)) NOT VALID,
    ADD CONSTRAINT grade_override_of_computed CHECK (override_score IS NULL OR origin = 'computed') NOT VALID,
    ADD CONSTRAINT grade_override_nonneg CHECK (override_score IS NULL OR override_score >= 0) NOT VALID,
    ADD CONSTRAINT grade_override_reason_length CHECK (
        override_reason IS NULL OR char_length(override_reason) BETWEEN 1 AND 500) NOT VALID,
    ADD CONSTRAINT grade_override_by_fk FOREIGN KEY (override_by_member_id) REFERENCES course_member (id) NOT VALID;

-- ---------------------------------------------------------------------------
-- document and document_version: purged, with a tombstone
-- ---------------------------------------------------------------------------

-- Who purged it is an actor, not a seat: purging is done from outside the
-- course, by an administrator. Only material, instructions and rubrics are
-- purged; a submitted file and a feedback file belong to a submission and a
-- grade, and are archived with them as ever. A purged document is archived
-- for good.
ALTER TABLE document
    ADD COLUMN purged_at timestamptz,
    ADD COLUMN purged_by_actor_id uuid,
    ADD COLUMN purge_reason text;
ALTER TABLE document
    ADD CONSTRAINT document_purge_whole CHECK (
        (purged_at IS NULL) = (purged_by_actor_id IS NULL)
        AND (purged_at IS NULL) = (purge_reason IS NULL)) NOT VALID,
    ADD CONSTRAINT document_purged_archived CHECK (purged_at IS NULL OR status = 'archived') NOT VALID,
    ADD CONSTRAINT document_purged_course_level CHECK (
        purged_at IS NULL OR kind IN ('material', 'instructions', 'rubric')) NOT VALID,
    ADD CONSTRAINT document_purge_reason_length CHECK (
        purge_reason IS NULL OR char_length(purge_reason) BETWEEN 1 AND 500) NOT VALID,
    ADD CONSTRAINT document_purged_by_fk FOREIGN KEY (purged_by_actor_id) REFERENCES actor (id) NOT VALID;

-- Once purged, it stays purged, as it was purged.
CREATE FUNCTION document_purge_kept() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.purged_at IS NOT NULL
       AND (NEW.purged_at, NEW.purged_by_actor_id, NEW.purge_reason)
           IS DISTINCT FROM (OLD.purged_at, OLD.purged_by_actor_id, OLD.purge_reason) THEN
        RAISE EXCEPTION 'document % has been purged, and stays purged', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER document_purge_kept
    BEFORE UPDATE ON document
    FOR EACH ROW EXECUTE FUNCTION document_purge_kept();

-- A purged version keeps its place (document, seq), its author, when it was
-- made, and what it was (content_type, byte_size); its text, its file and the
-- file's checksum go. Something that pinned it — a submission its
-- instructions, a grade its rubric — still names it, and reads that it was
-- purged, by whom, when and why.
ALTER TABLE document_version
    ADD COLUMN purged_at timestamptz,
    ADD COLUMN purged_by_actor_id uuid,
    ADD COLUMN purge_reason text;
ALTER TABLE document_version
    DROP CONSTRAINT document_version_has_content,
    ADD CONSTRAINT document_version_has_content CHECK (
        body_md IS NOT NULL OR storage_key IS NOT NULL OR purged_at IS NOT NULL) NOT VALID,
    ADD CONSTRAINT document_version_purge_whole CHECK (
        (purged_at IS NULL) = (purged_by_actor_id IS NULL)
        AND (purged_at IS NULL) = (purge_reason IS NULL)) NOT VALID,
    ADD CONSTRAINT document_version_purged_empty CHECK (
        purged_at IS NULL OR (body_md IS NULL AND storage_key IS NULL AND checksum IS NULL)) NOT VALID,
    ADD CONSTRAINT document_version_purge_reason_length CHECK (
        purge_reason IS NULL OR char_length(purge_reason) BETWEEN 1 AND 500) NOT VALID,
    ADD CONSTRAINT document_version_purged_by_fk FOREIGN KEY (purged_by_actor_id) REFERENCES actor (id) NOT VALID;

-- Versions stay append-only, but for that one change, made once: 0001's
-- trigger refused every UPDATE. The row is compared whole, so a column added
-- later is frozen too.
CREATE FUNCTION document_version_guarded() RETURNS trigger
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
    expected.storage_key := NULL;
    expected.checksum := NULL;
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

DROP TRIGGER document_version_append_only ON document_version;
CREATE TRIGGER document_version_append_only
    BEFORE UPDATE OR DELETE ON document_version
    FOR EACH ROW EXECUTE FUNCTION document_version_guarded();

COMMIT;

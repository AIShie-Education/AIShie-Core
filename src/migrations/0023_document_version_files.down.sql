-- AIshie Core — migration 0023 (down)
-- Reverts 0023_document_version_files.up.sql.
--
-- Lost: every file of a version but its first, and every file's name, and
-- the text versions of those other files. A version keeps its first file,
-- in its own columns, where the release before this one reads it, and that
-- file's text version is the version's again. The other files stay in the
-- file store, under documents/, which the release before this one neither
-- reads nor sweeps; migrated up again, nothing points at them, and the
-- orphan sweep removes them once they are as old as it waits for.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TRIGGER IF EXISTS document_version_text_one_file_at_a_time ON document_version_text;
DROP FUNCTION IF EXISTS document_version_text_one_file_at_a_time();

-- A text version to a version: its first file's.
DROP TRIGGER IF EXISTS document_version_text_guarded ON document_version_text;
DELETE FROM document_version_text t
USING document_version_file f
WHERE f.id = t.file_id AND f.position > 1;
ALTER TABLE document_version_text
    DROP CONSTRAINT document_version_text_file_fk,
    DROP CONSTRAINT document_version_text_pkey,
    DROP COLUMN file_id,
    ADD CONSTRAINT document_version_text_pkey PRIMARY KEY (version_id);

CREATE OR REPLACE FUNCTION document_version_text_guarded() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF NOT EXISTS (SELECT 1 FROM document_version WHERE id = OLD.version_id AND purged_at IS NOT NULL) THEN
            RAISE EXCEPTION 'the text version of % goes only when the version is purged', OLD.version_id
                USING ERRCODE = 'restrict_violation';
        END IF;
        RETURN OLD;
    END IF;
    IF TG_OP = 'UPDATE' THEN
        IF (NEW.version_id, NEW.document_id, NEW.course_id, NEW.created_at)
           IS DISTINCT FROM (OLD.version_id, OLD.document_id, OLD.course_id, OLD.created_at) THEN
            RAISE EXCEPTION 'a text version stays the text of its version, as it was made'
                USING ERRCODE = 'restrict_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM document_version v
                   JOIN document d ON d.id = v.document_id
                   WHERE v.id = NEW.version_id AND v.storage_key IS NOT NULL AND v.purged_at IS NULL
                     AND d.kind IN ('material', 'instructions', 'rubric')) THEN
        RAISE EXCEPTION 'version % has no file of a course''s material, instructions or rubric to transcribe', NEW.version_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER document_version_text_guarded
    BEFORE INSERT OR UPDATE OR DELETE ON document_version_text
    FOR EACH ROW EXECUTE FUNCTION document_version_text_guarded();

-- Queued as the version is added, and forgotten as it is purged, as 0020
-- had it.
CREATE FUNCTION document_version_queue_text() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO document_version_text (version_id, document_id, course_id)
    SELECT NEW.id, d.id, d.course_id
    FROM document d
    WHERE d.id = NEW.document_id AND d.kind IN ('material', 'instructions', 'rubric');
    RETURN NULL;
END;
$$;

CREATE TRIGGER document_version_text_queued
    AFTER INSERT ON document_version
    FOR EACH ROW WHEN (NEW.storage_key IS NOT NULL)
    EXECUTE FUNCTION document_version_queue_text();

DROP TRIGGER IF EXISTS document_version_files_purged ON document_version;
DROP FUNCTION IF EXISTS document_version_forget_files();

CREATE FUNCTION document_version_forget_text() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM document_version_text WHERE version_id = NEW.id;
    RETURN NULL;
END;
$$;

CREATE TRIGGER document_version_text_purged
    AFTER UPDATE OF purged_at ON document_version
    FOR EACH ROW WHEN (OLD.purged_at IS NULL AND NEW.purged_at IS NOT NULL)
    EXECUTE FUNCTION document_version_forget_text();

-- The files.
DROP TRIGGER IF EXISTS document_version_files_whole ON document_version;
DROP TABLE IF EXISTS document_version_file;
DROP FUNCTION IF EXISTS document_version_check_files();
DROP FUNCTION IF EXISTS document_version_file_check_whole();
DROP FUNCTION IF EXISTS document_version_files_whole(uuid);
DROP FUNCTION IF EXISTS document_version_file_queue_text();
DROP FUNCTION IF EXISTS document_version_file_guarded();
DROP FUNCTION IF EXISTS document_version_file_check_version();
DROP FUNCTION IF EXISTS document_file_name(text, text);

COMMIT;

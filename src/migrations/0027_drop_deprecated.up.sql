-- AIshie Core — migration 0027 (up)
-- What 0023 and 0025 kept for one release goes. Design reference:
-- docs/schema.md §2.1 (Agents' hosting), §2.4 (Files of a version, Text
-- versions). PostgreSQL 13+.
--
-- 0023 gave a version several files (document_version_file) and each file
-- a text version of its own, and kept, for the release before it, a
-- version's own file columns (storage_key, content_type, byte_size,
-- checksum) naming its first file; a version written with a file in them
-- alone recorded as its one file, at commit; a text version written naming
-- no file taken as its version's first file's; and a statement writing the
-- texts of two files of one version refused. 0025 hosted each agent one way
-- for good, and kept, for the release before it, actor.site_chat_credential_id
-- pointing at a runtime agent's live runtime token, and an agent registered
-- naming no hosting made an mcp agent. Every release since writes and reads
-- a version's files, names the file of every text, and names the hosting of
-- every agent it registers; none of them reads the rest. It goes here:
--
-- - document_version loses its own file columns. A version is still text,
--   files, or both, which the database holds at commit, with its files
--   numbered 1 to n, none missing (document_version_files_whole). A purged
--   version no longer says what type and size its first file was.
-- - A text version names its file; nothing chooses one for it.
-- - document_version_text_one_file_at_a_time goes: nothing writes a text
--   by its version alone.
-- - actor.site_chat_credential_id goes, and with it the last of 0011.
-- - An agent registered naming no hosting is refused
--   (actor_hosting_is_an_agents), as it is by the application.
--
-- The release before this one does not keep working on this schema: it
-- writes and reads the columns dropped here. This migration is deployed in a
-- release of its own, only once every server runs a release with 0023 and
-- 0025, and an agent runtime that names a file (file_id) when it tries a
-- transcription credential, whose renewal naming none is now refused
-- (docs/deploying.md). While it goes in, until this release starts, the
-- release before fails what reads an actor or a version. To roll back,
-- migrate down with this release's image before deploying the release before
-- (docs/deploying.md): the down puts back everything the release before
-- reads, from what this one keeps.

BEGIN;

-- Waits for a busy table rather than queueing every call behind it; a
-- deploy that times out here is run again.
SET LOCAL lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- A version's own file columns
-- ---------------------------------------------------------------------------

DROP TRIGGER document_version_files_whole ON document_version;

ALTER TABLE document_version
    DROP CONSTRAINT document_version_has_content,
    DROP CONSTRAINT document_version_file_described,
    DROP CONSTRAINT document_version_purged_empty,
    DROP COLUMN storage_key,
    DROP COLUMN content_type,
    DROP COLUMN byte_size,
    DROP COLUMN checksum,
    ADD CONSTRAINT document_version_purged_empty CHECK (purged_at IS NULL OR body_md IS NULL);

-- 0015's rule, without the columns: a version changes only by being
-- purged, once, which takes its text; its files go with it
-- (document_version_files_purged). The row is compared whole, so a column
-- added later is frozen too.
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

-- A version as its transaction leaves it: text, files, or both, its files
-- numbered 1 to n with none missing. Asked at commit, when a version and its
-- files are all written, whichever is written first. A purged version holds
-- neither, and is asked nothing.
CREATE OR REPLACE FUNCTION document_version_files_whole(version uuid) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    v  document_version;
    n  integer;
    lo integer;
    hi integer;
BEGIN
    SELECT * INTO v FROM document_version WHERE id = version;
    IF NOT FOUND OR v.purged_at IS NOT NULL THEN
        RETURN;
    END IF;
    SELECT count(*), min(position), max(position) INTO n, lo, hi FROM document_version_file WHERE version_id = version;
    IF n = 0 AND v.body_md IS NULL THEN
        RAISE EXCEPTION 'version %: a version is text, files, or both', version
            USING ERRCODE = 'check_violation';
    END IF;
    IF n > 0 AND (lo <> 1 OR hi <> n) THEN
        RAISE EXCEPTION 'version %: its % files are numbered 1 to %, none missing', version, n, n
            USING ERRCODE = 'check_violation';
    END IF;
END;
$$;

-- Every version, now that a version of files alone says nothing of them
-- in its own row.
CREATE CONSTRAINT TRIGGER document_version_files_whole
    AFTER INSERT ON document_version
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION document_version_check_files();

-- Nothing writes a version's file in its own columns any more, to be named
-- after its document.
DROP FUNCTION document_file_name(text, text);

-- ---------------------------------------------------------------------------
-- Text versions, by their file
-- ---------------------------------------------------------------------------

-- 0023's rules, but that a text version names its file: a text version is
-- of a file of a course's material, instructions or rubric, of a version
-- not purged; it stays the text of that file, in its course, and is deleted
-- only as its version is purged.
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
        IF (NEW.version_id, NEW.file_id, NEW.document_id, NEW.course_id, NEW.created_at)
           IS DISTINCT FROM (OLD.version_id, OLD.file_id, OLD.document_id, OLD.course_id, OLD.created_at) THEN
            RAISE EXCEPTION 'a text version stays the text of its file, as it was made'
                USING ERRCODE = 'restrict_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM document_version_file f
                   JOIN document_version v ON v.id = f.version_id
                   JOIN document d ON d.id = v.document_id
                   WHERE f.id = NEW.file_id AND f.version_id = NEW.version_id AND v.purged_at IS NULL
                     AND d.kind IN ('material', 'instructions', 'rubric')) THEN
        RAISE EXCEPTION 'version % has no such file of a course''s material, instructions or rubric to transcribe', NEW.version_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER document_version_text_one_file_at_a_time ON document_version_text;
DROP FUNCTION document_version_text_one_file_at_a_time();

-- ---------------------------------------------------------------------------
-- Agents
-- ---------------------------------------------------------------------------

ALTER TABLE actor
    DROP CONSTRAINT actor_site_chat_credential_fk,
    DROP CONSTRAINT actor_site_chat_is_agent,
    DROP COLUMN site_chat_credential_id;

-- 0011's key on a credential and whose it is, which only the column above
-- named.
ALTER TABLE credential DROP CONSTRAINT credential_id_actor_key;

DROP TRIGGER actor_hosting_default ON actor;
DROP FUNCTION actor_hosting_by_default();

COMMIT;

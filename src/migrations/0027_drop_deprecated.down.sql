-- AIshie Core — migration 0027 (down)
-- Reverts 0027_drop_deprecated.up.sql.
--
-- Puts back what the release before reads, from what this release keeps:
-- each version's own file columns, naming its first file; each runtime
-- agent's site chat credential, naming the runtime token it holds that is
-- not revoked; and the rules 0023 and 0025 kept for the release before
-- them, as they were. Lost: what type and size the first file of a purged
-- version was, which a purged version said and the up dropped, whenever it
-- was purged (its files went with its purge); its columns are left empty.

BEGIN;

SET LOCAL lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- Agents
-- ---------------------------------------------------------------------------

-- As 0025 had it.
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

-- As 0011 had it, naming each runtime agent's runtime token that is not
-- revoked, as the release before keeps it.
ALTER TABLE credential ADD CONSTRAINT credential_id_actor_key UNIQUE (id, actor_id);

ALTER TABLE actor
    ADD COLUMN site_chat_credential_id uuid,
    ADD CONSTRAINT actor_site_chat_is_agent CHECK (site_chat_credential_id IS NULL OR kind = 'agent'),
    ADD CONSTRAINT actor_site_chat_credential_fk
        FOREIGN KEY (site_chat_credential_id, id) REFERENCES credential (id, actor_id);

UPDATE actor a
   SET site_chat_credential_id = c.id
  FROM credential c
 WHERE a.hosting = 'runtime' AND c.actor_id = a.id AND c.issued_to_service = 'agent_runtime' AND c.revoked_at IS NULL;

-- ---------------------------------------------------------------------------
-- Text versions
-- ---------------------------------------------------------------------------

-- As 0023 had them.
CREATE FUNCTION document_version_text_one_file_at_a_time() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    version uuid;
BEGIN
    SELECT n.version_id INTO version
    FROM new_texts n
    JOIN old_texts o ON o.version_id = n.version_id AND o.file_id = n.file_id
    WHERE n.revision <> o.revision OR n.body IS DISTINCT FROM o.body
    GROUP BY n.version_id
    HAVING count(*) > 1
    LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'version %: the text of one of its files is written at a time, by its file', version
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER document_version_text_one_file_at_a_time
    AFTER UPDATE ON document_version_text
    REFERENCING OLD TABLE AS old_texts NEW TABLE AS new_texts
    FOR EACH STATEMENT EXECUTE FUNCTION document_version_text_one_file_at_a_time();

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
    IF NEW.file_id IS NULL THEN
        SELECT f.id INTO NEW.file_id FROM document_version_file f WHERE f.version_id = NEW.version_id AND f.position = 1;
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

-- ---------------------------------------------------------------------------
-- A version's own file columns
-- ---------------------------------------------------------------------------

-- As 0023 had it.
CREATE FUNCTION document_file_name(title text, content_type text) RETURNS text
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE
    name text := btrim(regexp_replace(coalesce(title, ''), '[\x01-\x1f\x7f-\x9f/\\\u202a-\u202e\u2066-\u2069]+', ' ', 'g'));
    ext  text := CASE lower(btrim(split_part(coalesce(content_type, ''), ';', 1)))
        WHEN 'application/pdf' THEN '.pdf'
        WHEN 'application/msword' THEN '.doc'
        WHEN 'application/vnd.openxmlformats-officedocument.wordprocessingml.document' THEN '.docx'
        WHEN 'application/vnd.ms-powerpoint' THEN '.ppt'
        WHEN 'application/vnd.openxmlformats-officedocument.presentationml.presentation' THEN '.pptx'
        WHEN 'application/vnd.ms-excel' THEN '.xls'
        WHEN 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet' THEN '.xlsx'
        WHEN 'application/vnd.oasis.opendocument.text' THEN '.odt'
        WHEN 'application/vnd.oasis.opendocument.presentation' THEN '.odp'
        WHEN 'application/vnd.oasis.opendocument.spreadsheet' THEN '.ods'
        WHEN 'application/zip' THEN '.zip'
        WHEN 'application/json' THEN '.json'
        WHEN 'text/plain' THEN '.txt'
        WHEN 'text/markdown' THEN '.md'
        WHEN 'text/csv' THEN '.csv'
        WHEN 'text/html' THEN '.html'
        WHEN 'text/x-python' THEN '.py'
        WHEN 'image/png' THEN '.png'
        WHEN 'image/jpeg' THEN '.jpg'
        WHEN 'image/gif' THEN '.gif'
        WHEN 'image/webp' THEN '.webp'
        WHEN 'image/svg+xml' THEN '.svg'
        WHEN 'audio/mpeg' THEN '.mp3'
        WHEN 'video/mp4' THEN '.mp4'
    END;
BEGIN
    IF name = '' THEN
        name := 'file';
    END IF;
    IF ext IS NOT NULL AND lower(right(name, length(ext))) <> ext THEN
        RETURN btrim(left(name, 255 - length(ext))) || ext;
    END IF;
    RETURN btrim(left(name, 255));
END;
$$;

-- The columns, each version's first file in them. A version takes no
-- change but its purge, so its guard is set aside while they are filled.
DROP TRIGGER document_version_append_only ON document_version;
DROP TRIGGER document_version_files_whole ON document_version;

ALTER TABLE document_version
    DROP CONSTRAINT document_version_purged_empty,
    ADD COLUMN storage_key text,
    ADD COLUMN content_type text,
    ADD COLUMN byte_size bigint,
    ADD COLUMN checksum text;

UPDATE document_version v
   SET storage_key = f.storage_key, content_type = f.content_type, byte_size = f.byte_size, checksum = f.checksum
  FROM document_version_file f
 WHERE f.version_id = v.id AND f.position = 1;

-- As 0001 and 0015 had them.
ALTER TABLE document_version
    ADD CONSTRAINT document_version_storage_key_key UNIQUE (storage_key),
    ADD CONSTRAINT document_version_file_described CHECK (
        storage_key IS NULL OR (content_type IS NOT NULL AND byte_size IS NOT NULL AND byte_size >= 0)),
    ADD CONSTRAINT document_version_has_content CHECK (
        body_md IS NOT NULL OR storage_key IS NOT NULL OR purged_at IS NOT NULL) NOT VALID,
    ADD CONSTRAINT document_version_purged_empty CHECK (
        purged_at IS NULL OR (body_md IS NULL AND storage_key IS NULL AND checksum IS NULL)) NOT VALID;

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

CREATE TRIGGER document_version_append_only
    BEFORE UPDATE OR DELETE ON document_version
    FOR EACH ROW EXECUTE FUNCTION document_version_guarded();

CREATE OR REPLACE FUNCTION document_version_files_whole(version uuid) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    v     document_version;
    n     integer;
    lo    integer;
    hi    integer;
    first document_version_file;
BEGIN
    SELECT * INTO v FROM document_version WHERE id = version;
    IF NOT FOUND OR v.purged_at IS NOT NULL THEN
        RETURN;
    END IF;
    SELECT count(*), min(position), max(position) INTO n, lo, hi FROM document_version_file WHERE version_id = version;
    IF n = 0 THEN
        IF v.storage_key IS NOT NULL THEN
            INSERT INTO document_version_file (version_id, document_id, position, filename, storage_key, content_type,
                                               byte_size, checksum, created_at)
            SELECT v.id, v.document_id, 1, document_file_name(d.title, v.content_type), v.storage_key, v.content_type,
                   v.byte_size, v.checksum, v.created_at
            FROM document d WHERE d.id = v.document_id;
        END IF;
        RETURN;
    END IF;
    IF lo <> 1 OR hi <> n THEN
        RAISE EXCEPTION 'version %: its % files are numbered 1 to %, none missing', version, n, n
            USING ERRCODE = 'check_violation';
    END IF;
    SELECT * INTO first FROM document_version_file WHERE version_id = version AND position = 1;
    IF (v.storage_key, v.content_type, v.byte_size, v.checksum)
       IS DISTINCT FROM (first.storage_key, first.content_type, first.byte_size, first.checksum) THEN
        RAISE EXCEPTION 'version %: its own file columns name its first file', version
            USING ERRCODE = 'check_violation';
    END IF;
END;
$$;

CREATE CONSTRAINT TRIGGER document_version_files_whole
    AFTER INSERT ON document_version
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW WHEN (NEW.storage_key IS NOT NULL)
    EXECUTE FUNCTION document_version_check_files();

COMMIT;

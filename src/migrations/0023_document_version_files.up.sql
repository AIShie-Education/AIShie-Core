-- AIshie Core — migration 0023 (up)
-- A version of a document holds several files. Design reference:
-- docs/schema.md §2.4 (Content, Files of a version, Text versions).
-- PostgreSQL 13+.
--
-- One uploaded document may be several files: a lecture's slides, its
-- handout and a sample program, in one version, with text beside them or
-- none. Each row here is one file of one version: its place among the
-- version's files, the name it is shown and downloaded under, and what the
-- file store says of it. A version's files are written with it, in its
-- transaction, and kept as they are, as the version is: nothing is added to
-- a version afterwards, and a file changes or goes only as its version is
-- purged, when it goes with it.
--
-- document_version keeps its own file columns (storage_key, content_type,
-- byte_size, checksum), filled, for a version with files, with its first
-- file, for one release: the release before this one reads a version's file
-- there. They are deprecated, and a later migration drops them. Every
-- version with a file has its row here, the first release's among them: a
-- file already there is recorded as its version's one file, named after its
-- document (document_file_name).
--
-- A text version is now of one file, not of a version: each file is
-- transcribed on its own. Each existing one is its version's one file's.
--
-- The previous release keeps working while this goes in, and after a
-- rollback, for a version with one file as before: it writes a version's
-- file in the version's own columns, which the database records here too,
-- at commit, named after the document, and queues for its text; it reads a
-- version's first file there; it purges a version, whose files and texts
-- go with it (it removes the first file from the store, and the sweep the
-- others). Of a version with several files it knows the first alone, and
-- its text: writing that text, it would write every file's, which is
-- refused (document_version_text_one_file_at_a_time). This release puts
-- what it is given to upload under documents/, which the previous release's
-- orphan sweep does not list: it would take a version's other files, which
-- it does not see, for uploads nothing came to point at.

BEGIN;

-- Waits for a busy table rather than queueing every call behind it; a
-- deploy that times out here is run again.
SET LOCAL lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- Files of versions
-- ---------------------------------------------------------------------------

-- The name a file is given when nothing else names it: its document's
-- title, made a name (no control characters, none that turn the text
-- round, no slashes), with the extension of its type where the title has
-- none, at most 255 characters; "file" for a title that leaves nothing.
-- The application names a file the same way (tools.legacyFilename).
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

-- position orders a version's files, 1, 2, 3, as they were given, the
-- first being the one the version's own columns name. The composite key
-- makes "the file, its version and its document are one" a database fact.
-- storage_key is the file store's key for it, made by the server; it is
-- unique here, as a message's file's is among those. filename is what its
-- uploader called it, a name and not a path, on one line: what a reader is
-- shown, and the name it downloads under.
CREATE TABLE document_version_file (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    version_id   uuid        NOT NULL,
    document_id  uuid        NOT NULL,
    position     integer     NOT NULL,
    filename     text        NOT NULL,
    storage_key  text        NOT NULL UNIQUE,
    content_type text        NOT NULL,
    byte_size    bigint      NOT NULL,
    checksum     text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (version_id, position),
    -- Target of the text version's key.
    UNIQUE (id, version_id),
    FOREIGN KEY (version_id, document_id) REFERENCES document_version (id, document_id),
    -- At most 100 files to a version, whatever DOCUMENT_MAX_FILES_PER_VERSION says.
    CONSTRAINT document_version_file_position_valid     CHECK (position BETWEEN 1 AND 100),
    CONSTRAINT document_version_file_filename_valid     CHECK (
        char_length(filename) BETWEEN 1 AND 255 AND filename = btrim(filename) AND filename !~ '[\x01-\x1f\x7f/\\]'
    ),
    CONSTRAINT document_version_file_content_type_valid CHECK (char_length(content_type) BETWEEN 1 AND 200),
    CONSTRAINT document_version_file_size_valid         CHECK (byte_size >= 0)
);
-- A document's files, every version's, which the version list reads at once.
CREATE INDEX document_version_file_document_idx ON document_version_file (document_id);

-- A file is written with its version, in its transaction, which dates both
-- alike: one dated otherwise would be a file added to a version afterwards.
-- A purged version holds nothing, and takes nothing.
CREATE FUNCTION document_version_file_check_version() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    written timestamptz;
    purged  timestamptz;
BEGIN
    SELECT created_at, purged_at INTO written, purged FROM document_version WHERE id = NEW.version_id;
    IF NOT FOUND THEN
        RETURN NEW; -- the foreign key says so
    END IF;
    IF purged IS NOT NULL THEN
        RAISE EXCEPTION 'version % was purged, and holds no file', NEW.version_id
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.created_at IS DISTINCT FROM written THEN
        RAISE EXCEPTION 'file %: a file is written with its version %, and nothing is added to a version afterwards',
            NEW.id, NEW.version_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER document_version_file_with_its_version
    BEFORE INSERT ON document_version_file
    FOR EACH ROW EXECUTE FUNCTION document_version_file_check_version();

-- Kept as it was written; deleted only as its version is purged.
CREATE FUNCTION document_version_file_guarded() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF NOT EXISTS (SELECT 1 FROM document_version WHERE id = OLD.version_id AND purged_at IS NOT NULL) THEN
            RAISE EXCEPTION 'the files of version % go only when it is purged', OLD.version_id
                USING ERRCODE = 'restrict_violation';
        END IF;
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'document_version_file is append-only: a file is kept as it was written'
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER document_version_file_kept
    BEFORE UPDATE OR DELETE ON document_version_file
    FOR EACH ROW EXECUTE FUNCTION document_version_file_guarded();

CREATE TRIGGER document_version_file_no_truncate
    BEFORE TRUNCATE ON document_version_file
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- Every version with a file already, its one file, named after its
-- document. A purged version has none.
INSERT INTO document_version_file (version_id, document_id, position, filename, storage_key, content_type, byte_size,
                                   checksum, created_at)
SELECT v.id, v.document_id, 1, document_file_name(d.title, v.content_type), v.storage_key, v.content_type, v.byte_size,
       v.checksum, v.created_at
FROM document_version v
JOIN document d ON d.id = v.document_id
WHERE v.storage_key IS NOT NULL AND v.purged_at IS NULL;

-- A version's files, as its transaction leaves them: numbered 1 to n with
-- none missing, the first being the file the version's own columns name.
-- A version written with a file in its own columns alone, by a release that
-- knows one file to a version, is given it here as its one file, named
-- after its document. Asked at commit, when a version and its files are all
-- written, whichever is written first.
CREATE FUNCTION document_version_files_whole(version uuid) RETURNS void
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

CREATE FUNCTION document_version_check_files() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM document_version_files_whole(NEW.id);
    RETURN NULL;
END;
$$;

CREATE FUNCTION document_version_file_check_whole() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM document_version_files_whole(NEW.version_id);
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER document_version_files_whole
    AFTER INSERT ON document_version
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW WHEN (NEW.storage_key IS NOT NULL)
    EXECUTE FUNCTION document_version_check_files();

CREATE CONSTRAINT TRIGGER document_version_file_whole
    AFTER INSERT ON document_version_file
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION document_version_file_check_whole();

-- ---------------------------------------------------------------------------
-- Text versions, one to a file
-- ---------------------------------------------------------------------------

-- Each text version there is is its version's one file's.
ALTER TABLE document_version_text ADD COLUMN file_id uuid;
UPDATE document_version_text t SET file_id = f.id
FROM document_version_file f
WHERE f.version_id = t.version_id AND f.position = 1;
ALTER TABLE document_version_text
    ALTER COLUMN file_id SET NOT NULL,
    DROP CONSTRAINT document_version_text_pkey,
    ADD CONSTRAINT document_version_text_pkey PRIMARY KEY (version_id, file_id),
    ADD CONSTRAINT document_version_text_file_fk FOREIGN KEY (file_id, version_id) REFERENCES document_version_file (id, version_id);

-- 0020's rules, of a file: a text version is of a file of a course's
-- material, instructions or rubric, of a version not purged; it stays the
-- text of that file, in its course, and is deleted only as its version is
-- purged. A release that knows one file to a version names none, and means
-- its version's first.
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

-- Queued as each file is added, in its version's transaction, whichever
-- release adds it; no longer as the version is.
DROP TRIGGER document_version_text_queued ON document_version;
DROP FUNCTION document_version_queue_text();

CREATE FUNCTION document_version_file_queue_text() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO document_version_text (version_id, file_id, document_id, course_id)
    SELECT NEW.version_id, NEW.id, d.id, d.course_id
    FROM document d
    WHERE d.id = NEW.document_id AND d.kind IN ('material', 'instructions', 'rubric');
    RETURN NULL;
END;
$$;

CREATE TRIGGER document_version_file_text_queued
    AFTER INSERT ON document_version_file
    FOR EACH ROW EXECUTE FUNCTION document_version_file_queue_text();

-- Deleted as the version is purged, the texts first and then the files
-- they are of: the text is the file's, and goes with it.
DROP TRIGGER document_version_text_purged ON document_version;
DROP FUNCTION document_version_forget_text();

CREATE FUNCTION document_version_forget_files() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM document_version_text WHERE version_id = NEW.id;
    DELETE FROM document_version_file WHERE version_id = NEW.id;
    RETURN NULL;
END;
$$;

CREATE TRIGGER document_version_files_purged
    AFTER UPDATE OF purged_at ON document_version
    FOR EACH ROW WHEN (OLD.purged_at IS NULL AND NEW.purged_at IS NOT NULL)
    EXECUTE FUNCTION document_version_forget_files();

-- One file's text at a time. This release writes a text, or discards it,
-- by its file; the release before, by its version alone, which for a
-- version with several files would write the same text over each of them.
-- A statement that changes the text of two files of one version is
-- refused. Claiming, releasing and failing several at once change none.
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

COMMIT;

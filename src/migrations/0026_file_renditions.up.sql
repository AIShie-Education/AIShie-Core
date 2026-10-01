-- AIshie Core — migration 0026 (up)
-- Every Office or OpenDocument file Core keeps is given a PDF rendition,
-- converted once by the site's agent runtime. Design reference:
-- docs/schema.md §2.4 (Renditions). PostgreSQL 13+.
--
-- A file of a document's version, of every kind (material, instructions, a
-- rubric, a submission, feedback), and a file a message of a conversation
-- carries, is previewed in the site as a PDF: a Word, Excel or PowerPoint
-- file, or an OpenDocument one, is converted to PDF on the server, once, by
-- the site's agent runtime, and never in the browser. Each row here is the
-- rendition of one such file: where it stands in the queue the runtime takes
-- it from, and, once it is done, the PDF in the file store (storage_key,
-- under renditions/), its size, checksum and page count. Who may read a
-- rendition is who may read its file, which the application decides.
--
-- Which files: those whose name ends in one of the extensions of the
-- table below and whose declared type is one of its types, or bytes of no
-- particular type (file_rendition_convertible). Never a PDF, a picture, a
-- text, an archive. The runtime holds the same table.
--
-- A rendition is queued by the database as its file is recorded, in its
-- transaction, whichever release records it: a version's file as it is
-- written, at commit for one the release before writes in the version's
-- own columns; a message's file as the message is written. What is there
-- already is queued once, here, behind every file that comes after it
-- (backfill). It goes with its file (ON DELETE CASCADE): a version purged
-- takes its files' renditions with it, and the PDF is removed from the file
-- store by the purge, or, by a release that does not know of it, by the
-- orphan sweep.
--
-- The previous release keeps working while this goes in, and after a
-- rollback: it records files, which are queued as any are, and purges
-- versions, whose renditions go with their files; it reads nothing here,
-- and its orphan sweep does not look under renditions/.

BEGIN;

-- Waits for a busy table rather than queueing every call behind it; a
-- deploy that times out here is run again.
SET LOCAL lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- Which files are converted
-- ---------------------------------------------------------------------------

-- The one table: a file is converted when its name's extension is one of
-- these, and its declared type, without its parameters, is one of these
-- types or says nothing in particular of the bytes (application/octet-stream;
-- application/zip and application/x-zip-compressed, as some clients call an
-- Office Open XML or OpenDocument file, which is a zip; application/vnd.ms-office,
-- as some call an older Office file). Both are asked: a .csv a browser
-- declares an Excel file is not converted, nor a .docx declared text/html.
--
--   doc dot                       application/msword
--   docx                          application/vnd.openxmlformats-officedocument.wordprocessingml.document
--   docm                          application/vnd.ms-word.document.macroenabled.12
--   dotx                          application/vnd.openxmlformats-officedocument.wordprocessingml.template
--   xls xlt                       application/vnd.ms-excel
--   xlsx                          application/vnd.openxmlformats-officedocument.spreadsheetml.sheet
--   xlsm                          application/vnd.ms-excel.sheet.macroenabled.12
--   xltx                          application/vnd.openxmlformats-officedocument.spreadsheetml.template
--   ppt pps pot                   application/vnd.ms-powerpoint
--   pptx                          application/vnd.openxmlformats-officedocument.presentationml.presentation
--   pptm                          application/vnd.ms-powerpoint.presentation.macroenabled.12
--   ppsx                          application/vnd.openxmlformats-officedocument.presentationml.slideshow
--   potx                          application/vnd.openxmlformats-officedocument.presentationml.template
--   odt                           application/vnd.oasis.opendocument.text
--   ods                           application/vnd.oasis.opendocument.spreadsheet
--   odp                           application/vnd.oasis.opendocument.presentation
--   odg                           application/vnd.oasis.opendocument.graphics
--   rtf                           application/rtf, text/rtf
--
-- Extensions and types are compared in lower case.
CREATE FUNCTION file_rendition_convertible(filename text, content_type text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT coalesce(
        lower(substring(filename FROM '\.([^./\\]+)$')) IN (
            'doc', 'dot', 'docx', 'docm', 'dotx',
            'xls', 'xlt', 'xlsx', 'xlsm', 'xltx',
            'ppt', 'pps', 'pot', 'pptx', 'pptm', 'ppsx', 'potx',
            'odt', 'ods', 'odp', 'odg',
            'rtf')
        AND lower(btrim(split_part(content_type, ';', 1))) IN (
            'application/msword',
            'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
            'application/vnd.ms-word.document.macroenabled.12',
            'application/vnd.openxmlformats-officedocument.wordprocessingml.template',
            'application/vnd.ms-excel',
            'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
            'application/vnd.ms-excel.sheet.macroenabled.12',
            'application/vnd.openxmlformats-officedocument.spreadsheetml.template',
            'application/vnd.ms-powerpoint',
            'application/vnd.openxmlformats-officedocument.presentationml.presentation',
            'application/vnd.ms-powerpoint.presentation.macroenabled.12',
            'application/vnd.openxmlformats-officedocument.presentationml.slideshow',
            'application/vnd.openxmlformats-officedocument.presentationml.template',
            'application/vnd.oasis.opendocument.text',
            'application/vnd.oasis.opendocument.spreadsheet',
            'application/vnd.oasis.opendocument.presentation',
            'application/vnd.oasis.opendocument.graphics',
            'application/rtf', 'text/rtf',
            'application/octet-stream', 'application/zip', 'application/x-zip-compressed', 'application/vnd.ms-office'),
        false)
$$;

-- ---------------------------------------------------------------------------
-- Renditions
-- ---------------------------------------------------------------------------

-- One per convertible file: of a version (file_id) or of a message
-- (attachment_id), never both, and one to a file. status is where it
-- stands: queued, waiting to be claimed; claimed, by the runtime until
-- claimed_until (lease_id is the claim's); done, with its PDF; failed or
-- skipped, saying why (reason: the runtime's password_protected, timeout,
-- conversion_failed, too_large, unsupported, or Core's attempts_exhausted).
-- attempts counts the claims since it was last queued; backfill marks what
-- this migration queued, which the queue takes after everything else.
-- claimed_by_credential_id and claimed_at are the last claim's, kept after
-- it ends. A done rendition's PDF is storage_key, byte_size, checksum (as
-- the store gives it) and page_count, made at produced_at; done is for
-- good.
CREATE TABLE file_rendition (
    id                       uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    course_id                uuid        NOT NULL REFERENCES course (id),
    file_id                  uuid        UNIQUE REFERENCES document_version_file (id) ON DELETE CASCADE,
    attachment_id            uuid        UNIQUE REFERENCES conversation_attachment (id) ON DELETE CASCADE,
    status                   text        NOT NULL DEFAULT 'queued',
    reason                   text,
    attempts                 integer     NOT NULL DEFAULT 0,
    backfill                 boolean     NOT NULL DEFAULT false,
    queued_at                timestamptz NOT NULL DEFAULT now(),
    lease_id                 uuid,
    claimed_until            timestamptz,
    claimed_by_credential_id uuid        REFERENCES credential (id),
    claimed_at               timestamptz,
    storage_key              text        UNIQUE,
    byte_size                bigint,
    checksum                 text,
    page_count               integer,
    produced_at              timestamptz,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT file_rendition_one_source CHECK (num_nonnulls(file_id, attachment_id) = 1),
    CONSTRAINT file_rendition_status_valid CHECK (status IN ('queued', 'claimed', 'done', 'failed', 'skipped')),
    -- Failed and skipped say why, in one of a few words; nothing else does.
    CONSTRAINT file_rendition_reason_given CHECK (
        (status IN ('failed', 'skipped')) = (reason IS NOT NULL)
        AND (reason IS NULL OR reason IN ('password_protected', 'timeout', 'conversion_failed', 'too_large', 'unsupported',
                                          'attempts_exhausted'))),
    -- There is a PDF exactly when it is done, with its size, page count and
    -- when it was made.
    CONSTRAINT file_rendition_done_has_pdf CHECK (
        (status = 'done') = (storage_key IS NOT NULL)
        AND (storage_key IS NULL) = (byte_size IS NULL)
        AND (storage_key IS NULL) = (page_count IS NULL)
        AND (storage_key IS NULL) = (produced_at IS NULL)
        AND (storage_key IS NOT NULL OR checksum IS NULL)),
    -- A PDF starts with %PDF-, five bytes.
    CONSTRAINT file_rendition_pdf_valid CHECK (
        (byte_size IS NULL OR byte_size >= 5) AND (page_count IS NULL OR page_count BETWEEN 1 AND 100000)
        AND (storage_key IS NULL OR char_length(storage_key) BETWEEN 1 AND 1024)
        AND (checksum IS NULL OR char_length(checksum) BETWEEN 1 AND 200)),
    -- A claim holds a lease until it ends, by whichever credential made it.
    CONSTRAINT file_rendition_lease_held CHECK (
        (status = 'claimed') = (lease_id IS NOT NULL)
        AND (lease_id IS NULL) = (claimed_until IS NULL)
        AND (status <> 'claimed' OR (claimed_by_credential_id IS NOT NULL AND claimed_at IS NOT NULL))),
    CONSTRAINT file_rendition_attempts_valid CHECK (attempts >= 0)
);
-- The queue: what waits, uploads first and the backfill after them.
CREATE INDEX file_rendition_queue_idx ON file_rendition (backfill, queued_at) WHERE status IN ('queued', 'claimed');
-- What a credential claimed, released when it is revoked; and the
-- credential's key, which deleting a session's asks of.
CREATE INDEX file_rendition_claimed_by_idx ON file_rendition (claimed_by_credential_id);

-- A rendition is made queued, of a convertible file, in its file's course.
-- It stays the rendition of that file, in that course, as it was made;
-- done, it never changes; and it goes only when its file has gone.
CREATE FUNCTION file_rendition_guarded() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    course uuid;
    name   text;
    kind   text;
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF EXISTS (SELECT 1 FROM document_version_file WHERE id = OLD.file_id)
           OR EXISTS (SELECT 1 FROM conversation_attachment WHERE id = OLD.attachment_id) THEN
            RAISE EXCEPTION 'the rendition % goes only with its file', OLD.id
                USING ERRCODE = 'restrict_violation';
        END IF;
        RETURN OLD;
    END IF;
    IF TG_OP = 'UPDATE' THEN
        IF (NEW.id, NEW.course_id, NEW.file_id, NEW.attachment_id, NEW.created_at)
           IS DISTINCT FROM (OLD.id, OLD.course_id, OLD.file_id, OLD.attachment_id, OLD.created_at) THEN
            RAISE EXCEPTION 'a rendition stays the rendition of its file, as it was made'
                USING ERRCODE = 'restrict_violation';
        END IF;
        IF OLD.status = 'done' THEN
            RAISE EXCEPTION 'the rendition % is done, and its PDF is kept as it was made', OLD.id
                USING ERRCODE = 'restrict_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.status <> 'queued' OR NEW.attempts <> 0 THEN
        RAISE EXCEPTION 'a rendition is made queued, never claimed'
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.file_id IS NOT NULL THEN
        SELECT d.course_id, f.filename, f.content_type INTO course, name, kind
        FROM document_version_file f JOIN document d ON d.id = f.document_id
        WHERE f.id = NEW.file_id;
    ELSE
        SELECT a.course_id, a.filename, a.content_type INTO course, name, kind
        FROM conversation_attachment a WHERE a.id = NEW.attachment_id;
    END IF;
    IF NOT FOUND THEN
        RETURN NEW; -- the foreign key says so
    END IF;
    IF NOT file_rendition_convertible(name, kind) THEN
        RAISE EXCEPTION 'the file % (%) is no Office or OpenDocument file, and is not converted', name, kind
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.course_id IS DISTINCT FROM course THEN
        RAISE EXCEPTION 'a rendition is in its file''s course'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER file_rendition_guarded
    BEFORE INSERT OR UPDATE OR DELETE ON file_rendition
    FOR EACH ROW EXECUTE FUNCTION file_rendition_guarded();

CREATE TRIGGER file_rendition_no_truncate
    BEFORE TRUNCATE ON file_rendition
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- Queued as a version's file is recorded, in its transaction, whichever
-- release records it, and whatever kind of document it is.
CREATE FUNCTION document_version_file_queue_rendition() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO file_rendition (course_id, file_id)
    SELECT d.course_id, NEW.id FROM document d WHERE d.id = NEW.document_id;
    RETURN NULL;
END;
$$;

CREATE TRIGGER document_version_file_rendition_queued
    AFTER INSERT ON document_version_file
    FOR EACH ROW WHEN (file_rendition_convertible(NEW.filename, NEW.content_type))
    EXECUTE FUNCTION document_version_file_queue_rendition();

-- Queued as a message's file is recorded, with its message: an upload no
-- message came to carry is never queued.
CREATE FUNCTION conversation_attachment_queue_rendition() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO file_rendition (course_id, attachment_id) VALUES (NEW.course_id, NEW.id);
    RETURN NULL;
END;
$$;

CREATE TRIGGER conversation_attachment_rendition_queued
    AFTER INSERT ON conversation_attachment
    FOR EACH ROW WHEN (file_rendition_convertible(NEW.filename, NEW.content_type))
    EXECUTE FUNCTION conversation_attachment_queue_rendition();

-- ---------------------------------------------------------------------------
-- The backfill
-- ---------------------------------------------------------------------------

-- Every convertible file there is, of every version and every message, in
-- every course, archived or not, queued behind every file to come, and
-- dated as its file is.
INSERT INTO file_rendition (course_id, file_id, backfill, queued_at)
SELECT d.course_id, f.id, true, f.created_at
FROM document_version_file f
JOIN document d ON d.id = f.document_id
WHERE file_rendition_convertible(f.filename, f.content_type);

INSERT INTO file_rendition (course_id, attachment_id, backfill, queued_at)
SELECT a.course_id, a.id, true, a.created_at
FROM conversation_attachment a
WHERE file_rendition_convertible(a.filename, a.content_type);

COMMIT;

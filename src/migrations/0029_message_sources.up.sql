-- AIshie Core — migration 0029 (up)
-- What an answer relied on. Design reference: docs/schema.md §2.8
-- (Sources of an answer). PostgreSQL 13+.
--
-- An agent answering in a conversation may say which of the course's
-- materials its answer relied on (conversation.answer's sources): each a
-- version of a document, and, if it says so, one of the version's files, a
-- page or a slide of it and a part of its text. They are kept here, a row
-- each, written with the answer, in its transaction, and kept as they are.
-- A row each rather than a list in the message, so that every source names
-- a version and a file the database holds, in the answer's course, and so
-- that a reader's sources are read with their documents as they stand now
-- — their titles, whether they are archived, which version is published,
-- whether a version was purged — in one statement, for a page of messages.
--
-- A purged version keeps its row, a tombstone (§2.4), and so does its
-- source; the file a source names goes with its version's files, and the
-- source says no file from then on (file_id and file_version_id set null).
-- Nothing else of a source changes, and none is deleted.
--
-- Nothing that was written is changed. The previous release keeps working
-- while this goes in and after a rollback: it writes and reads nothing
-- here, and the table's keys cost it nothing on what it writes, but on a
-- purge, which clears the file of each source that named one of the
-- version's files.

BEGIN;

-- Waits for a busy table rather than queueing every call behind it; a
-- deploy that times out here is run again.
SET LOCAL lock_timeout = '10s';

CREATE TABLE conversation_message_source (
    message_id      uuid        NOT NULL,
    course_id       uuid        NOT NULL,
    position        integer     NOT NULL,
    document_id     uuid        NOT NULL,
    version_id      uuid        NOT NULL,
    -- The file, when the source names one: its id, and its version again,
    -- so that the key below holds it to the source's version and both are
    -- cleared together when the version is purged and the file goes.
    file_id         uuid,
    file_version_id uuid,
    page            integer,
    slide           integer,
    part            integer,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (message_id, position),
    FOREIGN KEY (message_id, course_id)        REFERENCES conversation_message (id, course_id),
    FOREIGN KEY (document_id, course_id)       REFERENCES document (id, course_id),
    FOREIGN KEY (version_id, document_id)      REFERENCES document_version (id, document_id),
    FOREIGN KEY (file_id, file_version_id)     REFERENCES document_version_file (id, version_id) ON DELETE SET NULL,
    -- At most 20 sources to an answer, as conversation.answer takes them.
    CONSTRAINT conversation_message_source_position_valid CHECK (position BETWEEN 1 AND 20),
    CONSTRAINT conversation_message_source_file_of_version CHECK (
        (file_id IS NULL) = (file_version_id IS NULL) AND (file_version_id IS NULL OR file_version_id = version_id)
    ),
    -- A page or a slide, as the text of a file heads them (## 第 N 頁,
    -- ## Slide N), not both; and a part of the text as document.text reads
    -- it in parts.
    CONSTRAINT conversation_message_source_page_or_slide CHECK (page IS NULL OR slide IS NULL),
    CONSTRAINT conversation_message_source_page_valid    CHECK (page BETWEEN 1 AND 100000),
    CONSTRAINT conversation_message_source_slide_valid   CHECK (slide BETWEEN 1 AND 100000),
    CONSTRAINT conversation_message_source_part_valid    CHECK (part BETWEEN 1 AND 100000)
);
-- The key the purge of a file clears a source's file by.
CREATE INDEX conversation_message_source_file_idx ON conversation_message_source (file_id, file_version_id)
    WHERE file_id IS NOT NULL;

-- A source is written with its message, in its transaction, which dates
-- both alike, as a message's file is; the message is an answer (it replies
-- to the opener, which only the respondent does); the document is a
-- course's material, instructions or rubric, never a student's work or a
-- grader's feedback; and the version is not purged, nor its document.
CREATE FUNCTION conversation_message_source_check() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    written timestamptz;
    answers uuid;
    kind    text;
    gone    timestamptz;
BEGIN
    SELECT created_at, in_reply_to_message_id INTO written, answers FROM conversation_message WHERE id = NEW.message_id;
    IF NOT FOUND THEN
        RETURN NEW; -- the foreign key says so
    END IF;
    IF NEW.created_at IS DISTINCT FROM written THEN
        RAISE EXCEPTION 'message %: a source is written with its message, and nothing is added to a message afterwards',
            NEW.message_id
            USING ERRCODE = 'check_violation';
    END IF;
    IF answers IS NULL THEN
        RAISE EXCEPTION 'message %: only an answer names its sources', NEW.message_id
            USING ERRCODE = 'check_violation';
    END IF;
    SELECT d.kind, coalesce(v.purged_at, d.purged_at) INTO kind, gone
    FROM document_version v JOIN document d ON d.id = v.document_id
    WHERE v.id = NEW.version_id AND v.document_id = NEW.document_id;
    IF NOT FOUND THEN
        RETURN NEW; -- the foreign key says so
    END IF;
    IF kind NOT IN ('material', 'instructions', 'rubric') THEN
        RAISE EXCEPTION 'message %: a source is a course''s material, instructions or rubric, not a %', NEW.message_id, kind
            USING ERRCODE = 'check_violation';
    END IF;
    IF gone IS NOT NULL THEN
        RAISE EXCEPTION 'message %: version % is purged, and nothing relies on it', NEW.message_id, NEW.version_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER conversation_message_source_with_its_answer
    BEFORE INSERT ON conversation_message_source
    FOR EACH ROW EXECUTE FUNCTION conversation_message_source_check();

-- Kept as written. The one change is the purge's: the file a source names
-- goes with its version's files, and the source names no file from then on.
CREATE FUNCTION conversation_message_source_guarded() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    expected conversation_message_source;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'conversation_message_source is append-only: DELETE is not allowed'
            USING ERRCODE = 'restrict_violation';
    END IF;
    expected := OLD;
    expected.file_id := NULL;
    expected.file_version_id := NULL;
    IF NEW IS DISTINCT FROM expected OR EXISTS (SELECT 1 FROM document_version_file WHERE id = OLD.file_id) THEN
        RAISE EXCEPTION 'conversation_message_source is append-only: only a purge, taking its file, changes it'
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER conversation_message_source_append_only
    BEFORE UPDATE OR DELETE ON conversation_message_source
    FOR EACH ROW EXECUTE FUNCTION conversation_message_source_guarded();

CREATE TRIGGER conversation_message_source_no_truncate
    BEFORE TRUNCATE ON conversation_message_source
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

COMMIT;

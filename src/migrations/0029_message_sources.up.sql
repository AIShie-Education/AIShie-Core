-- AIshie Core — migration 0029 (up)
-- What an answer relied on. Design reference: docs/schema.md §2.8
-- (What an answer relied on). PostgreSQL 13+.
--
-- An agent answering in a conversation may say which of the course's
-- materials its answer relied on (conversation.answer's sources): each a
-- version of a document, and, if it says so, one of the version's files, a
-- page or a slide of it and a part of its text. They are kept here, a row
-- each, written with the answer, in its transaction, and kept as they are.
-- A row each rather than a list in the message, so that every source names
-- a version and a file the database holds, in the answer's course, of a
-- course's material, instructions or rubric, not purged; and so that a
-- purge, which deletes a version's files, takes the file from each source
-- that named one.
--
-- An answer that says what it relied on says so even when it relied on
-- nothing (sources, empty): conversation_message.sources_stated, set as the
-- answer is written. An answer that said nothing — every answer before
-- this, and any from a runtime that does not say — has it false, and
-- readers are told nothing of its sources, rather than that it had none.
--
-- A purged version keeps its row, a tombstone (§2.4), and so does its
-- source; the file a source names goes with its version's files, and the
-- source says no file from then on (file_id and file_version_id set null).
-- Nothing else of a source changes, and none is deleted.
--
-- Nothing that was written is changed. The previous release keeps working
-- while this goes in and after a rollback: it writes and reads nothing
-- here, its messages say nothing of their sources (sources_stated takes
-- its default), and the table's keys cost it nothing on what it writes,
-- but on a purge, which clears the file of each source that named one of
-- the version's files. An answer proposed with sources under this release
-- and still waiting when the release before decides it is cancelled
-- (tool_no_longer_here): its input schema takes no sources
-- (docs/deploying.md, Migration 0029).

BEGIN;

-- Waits for a busy table rather than queueing every call behind it; a
-- deploy that times out here is run again.
SET LOCAL lock_timeout = '10s';

-- Whether the answer said what it relied on, its sources, even none; false
-- for every message written before, for a question, and for an answer that
-- did not say. Only an answer says. NOT VALID: every existing row holds
-- false, which passes. Validating would scan every message under the lock
-- this migration holds, which stops every write to the table. The check
-- holds for every row written from now on.
ALTER TABLE conversation_message
    ADD COLUMN sources_stated boolean NOT NULL DEFAULT false;
ALTER TABLE conversation_message
    ADD CONSTRAINT conversation_message_sources_of_an_answer
        CHECK (NOT sources_stated OR in_reply_to_message_id IS NOT NULL) NOT VALID;

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
-- both alike, as a message's file is; the message is an answer that says
-- what it relied on (sources_stated, which only an answer does); the
-- document is a course's material, instructions or rubric, never a
-- student's work or a grader's feedback; and the version is not purged,
-- nor its document.
CREATE FUNCTION conversation_message_source_check() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    written timestamptz;
    stated  boolean;
    kind    text;
    gone    timestamptz;
BEGIN
    SELECT created_at, sources_stated INTO written, stated FROM conversation_message WHERE id = NEW.message_id;
    IF NOT FOUND THEN
        RETURN NEW; -- the foreign key says so
    END IF;
    IF NEW.created_at IS DISTINCT FROM written THEN
        RAISE EXCEPTION 'message %: a source is written with its message, and nothing is added to a message afterwards',
            NEW.message_id
            USING ERRCODE = 'check_violation';
    END IF;
    IF NOT stated THEN
        RAISE EXCEPTION 'message %: only an answer that says what it relied on names its sources', NEW.message_id
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

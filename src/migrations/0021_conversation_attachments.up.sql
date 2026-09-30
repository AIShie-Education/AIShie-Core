-- AIshie Core — migration 0021 (up)
-- Files attached to the messages of conversations. Design reference:
-- docs/schema.md §2.8 (Attachments). PostgreSQL 13+.
--
-- A message may carry files: uploaded first (conversation.upload_url), as a
-- document's file is, and named by their upload tokens in the call that
-- writes the message (conversation.open, .ask, .answer), which records them
-- here in its own transaction. Each row is one file of one message: its
-- place among the message's files, the name its uploader gave it, and what
-- the file store says of it — where it is, its type, its size and its
-- checksum. The bytes are in the file store, under conversations/, beside
-- the courses/ of documents' files.
--
-- A file is its message's: written with it, dated as it is, and kept as it
-- is, as the message is. Nothing is added to a message afterwards, and
-- nothing here is changed or deleted. Who may read a file is who may read
-- its message, and a retraction hides a message's files from its readers as
-- it hides its text: those are the application's rules, and the rows and
-- the files stay for the record, as the text stays in the action that wrote
-- it.
--
-- The previous release keeps working while this goes in. It writes no row
-- here and reads none, and its orphan sweep lists only courses/ (and
-- attached/courses/), so it leaves the files under conversations/ alone,
-- whatever their age: a release that knew nothing of them must not take
-- them for uploads nothing came to point at.

BEGIN;

-- Waits for a busy table rather than queueing every call behind it; a
-- deploy that times out here is run again. The foreign keys below take a
-- lock on conversation and conversation_message while they are made.
SET LOCAL lock_timeout = '10s';

-- position orders a message's files, 1, 2, 3, as they were given. The
-- composite keys make "the file, its message, its conversation and its
-- course are one" a database fact. storage_key is the file store's key for
-- it, made by the server; it is unique here as a version's is among
-- versions, and the two never share one, each under a prefix of its own.
-- filename is what its uploader called it, a name and not a path, on one
-- line: what a reader is shown, and the name it downloads under.
CREATE TABLE conversation_attachment (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id      uuid        NOT NULL,
    conversation_id uuid        NOT NULL,
    course_id       uuid        NOT NULL,
    position        integer     NOT NULL,
    filename        text        NOT NULL,
    storage_key     text        NOT NULL UNIQUE,
    content_type    text        NOT NULL,
    byte_size       bigint      NOT NULL,
    checksum        text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (message_id, position),
    FOREIGN KEY (message_id, conversation_id) REFERENCES conversation_message (id, conversation_id),
    FOREIGN KEY (conversation_id, course_id)  REFERENCES conversation (id, course_id),
    CONSTRAINT conversation_attachment_position_valid     CHECK (position >= 1),
    CONSTRAINT conversation_attachment_filename_valid     CHECK (
        char_length(filename) BETWEEN 1 AND 255 AND filename = btrim(filename) AND filename !~ '[\x01-\x1f\x7f/\\]'
    ),
    CONSTRAINT conversation_attachment_content_type_valid CHECK (char_length(content_type) BETWEEN 1 AND 200),
    CONSTRAINT conversation_attachment_size_valid         CHECK (byte_size >= 0)
);
-- What a conversation holds in files already, which a new message's files
-- are added to and held to a limit with.
CREATE INDEX conversation_attachment_conversation_idx ON conversation_attachment (conversation_id);

-- A file is written with its message, in its transaction, which dates both
-- alike: one dated otherwise would be a file added to a message afterwards.
CREATE FUNCTION conversation_attachment_check_message() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    written timestamptz;
BEGIN
    SELECT created_at INTO written FROM conversation_message WHERE id = NEW.message_id;
    IF NOT FOUND THEN
        RETURN NEW; -- the foreign key says so
    END IF;
    IF NEW.created_at IS DISTINCT FROM written THEN
        RAISE EXCEPTION 'attachment %: a file is written with its message %, and nothing is added to a message afterwards',
            NEW.id, NEW.message_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER conversation_attachment_with_its_message
    BEFORE INSERT ON conversation_attachment
    FOR EACH ROW EXECUTE FUNCTION conversation_attachment_check_message();

CREATE TRIGGER conversation_attachment_append_only
    BEFORE UPDATE OR DELETE ON conversation_attachment
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

CREATE TRIGGER conversation_attachment_no_truncate
    BEFORE TRUNCATE ON conversation_attachment
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

COMMIT;

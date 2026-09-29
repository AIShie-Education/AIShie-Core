-- AIshiteru Core — migration 0019 (up)
-- An answer's draft, while it is written: what the respondent's runtime is
-- doing (its steps: thinking, reading a document, …) and the answer's text
-- so far, for those reading the conversation to watch, until the answer is
-- posted or proposed. Design reference: docs/schema.md §2.8 (Drafts).
-- PostgreSQL 13+.
--
-- A draft is not a record. It is written many times a second
-- (conversation.draft, an ephemeral write: no action, no event), a newer one
-- replaces it whole, and it means nothing once the answer is there. So it is
-- kept in an UNLOGGED table: no WAL, and so none on a standby either, and
-- emptied by a crash of the server, which loses nothing anyone relies on: the
-- next write brings the draft back, and until then the conversation reads as
-- it would with none. One row per conversation, the last write winning; a
-- row written more than 120 seconds ago is no draft, and is swept.
--
-- The previous release knows none of this and goes on working: it writes no
-- draft, reads none, and leaves the drafts of the conversations it answers
-- or closes to the sweep, which reads leave out meanwhile once they are
-- stale.

BEGIN;

-- Waits for a busy table rather than queueing every call behind it; a
-- deploy that times out here is run again. The foreign key takes a lock on
-- conversation while it is made.
SET LOCAL lock_timeout = '10s';

-- attempt and version are the runtime's: which attempt at an answer this
-- is, and how far into it; a write that is not newer than the one kept is
-- passed over, so that one arriving late does not undo a newer one. body is
-- the whole text so far, or null for none yet; steps the whole list of what
-- the runtime has done so far, each {kind, target?, state}. done marks an
-- attempt given up or finished: nothing is shown, and a write of that attempt
-- arriving after it is passed over, until another attempt replaces it or it
-- is 120 seconds old.
CREATE UNLOGGED TABLE conversation_draft (
    conversation_id uuid        PRIMARY KEY,
    course_id       uuid        NOT NULL,
    attempt         text        NOT NULL,
    version         bigint      NOT NULL,
    body            text,
    steps           jsonb       NOT NULL DEFAULT '[]',
    done            boolean     NOT NULL DEFAULT false,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (conversation_id, course_id) REFERENCES conversation (id, course_id),
    CONSTRAINT conversation_draft_attempt_valid CHECK (char_length(attempt) BETWEEN 1 AND 64),
    CONSTRAINT conversation_draft_version_valid CHECK (version >= 1),
    CONSTRAINT conversation_draft_body_valid    CHECK (body IS NULL OR char_length(body) <= 20000),
    -- At most 20 steps, each an object of kind, state and, if it says what
    -- it is about, target: a string of at most 120 characters.
    CONSTRAINT conversation_draft_steps_valid CHECK (CASE WHEN jsonb_typeof(steps) = 'array' THEN
        jsonb_array_length(steps) <= 20
        AND NOT jsonb_path_exists(steps, '$[*] ? (@.type() != "object"
              || !(@.kind == "thinking" || @.kind == "reading_document" || @.kind == "listing_documents"
                   || @.kind == "reading_assignment" || @.kind == "reading_submission" || @.kind == "searching_memory"
                   || @.kind == "writing" || @.kind == "tool")
              || !(@.state == "running" || @.state == "done")
              || (exists(@.target) && !(@.target.type() == "string" && @.target like_regex "^.{0,120}$" flag "s")))')
        AND NOT jsonb_path_exists(steps, '$[*].keyvalue() ? (!(@.key == "kind" || @.key == "target" || @.key == "state"))')
        ELSE false END),
    CONSTRAINT conversation_draft_done_empty CHECK (NOT done OR (body IS NULL AND steps = '[]'::jsonb))
);

COMMIT;

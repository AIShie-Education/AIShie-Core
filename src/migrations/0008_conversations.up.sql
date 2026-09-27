-- AIshiteru Core — migration 0008 (up)
-- Conversations: a member asks one other member questions, and that member
-- answers them. Design reference: docs/schema.md §2.8. PostgreSQL 13+.
--
-- The rule they come with is the application's, not the database's: nobody
-- gains through a conversation more than they hold. A member may address a
-- respondent only if the respondent can see and do nothing the member
-- cannot, or is the member's own delegate, so that a question cannot make an
-- agent a confused deputy over what its seat reads (docs/schema.md §2.8); a
-- respondent that answers several people also holds what each wrote to it,
-- which the read tools say. What the database holds is
-- the shape: who the two participants are never changes, only they write, a
-- closed conversation stays closed, an answer answers the opener, and what
-- was written stays written. A message is withdrawn by a retraction row
-- beside it, never by changing it.
--
-- Everything here is new, and the previous release knows none of it: it goes
-- on working, and a seat it removes leaves that seat's conversations open,
-- where nobody can write any more, since both participants must be live to
-- (docs/schema.md §2.8).

BEGIN;

-- Waits for a busy table rather than queueing every call behind it; a
-- deploy that times out here is run again. The foreign keys below take a
-- lock on course_member and action while they are made.
SET LOCAL lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- conversation: one opener, one respondent, in one course
-- ---------------------------------------------------------------------------

-- last_message_at and last_author_member_id say who spoke last, for the
-- respondent's inbox (the opener spoke last: an answer is awaited) and for
-- the lists. Writing a message updates them first, WHERE status = 'open', and
-- only then inserts the message: the update takes the conversation's row
-- lock, so a close and a message never pass each other, and messages in one
-- conversation are written one at a time, in seq order.
CREATE TABLE conversation (
    id                    uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    course_id             uuid        NOT NULL REFERENCES course (id),
    opener_member_id      uuid        NOT NULL,
    respondent_member_id  uuid        NOT NULL,
    title                 text,
    status                text        NOT NULL DEFAULT 'open',
    closed_reason         text,
    created_at            timestamptz NOT NULL DEFAULT now(),
    last_message_at       timestamptz,
    last_author_member_id uuid,
    -- Target of the messages' composite foreign key.
    UNIQUE (id, course_id),
    FOREIGN KEY (course_id, opener_member_id)      REFERENCES course_member (course_id, id),
    FOREIGN KEY (course_id, respondent_member_id)  REFERENCES course_member (course_id, id),
    FOREIGN KEY (course_id, last_author_member_id) REFERENCES course_member (course_id, id),
    CONSTRAINT conversation_status_valid        CHECK (status IN ('open', 'closed')),
    CONSTRAINT conversation_not_with_self       CHECK (opener_member_id <> respondent_member_id),
    CONSTRAINT conversation_title_length        CHECK (title IS NULL OR char_length(title) BETWEEN 1 AND 200),
    CONSTRAINT conversation_closed_reason_valid CHECK (
        (status = 'closed' OR closed_reason IS NULL) AND (closed_reason IS NULL OR char_length(closed_reason) <= 500)
    ),
    CONSTRAINT conversation_last_author_valid   CHECK (
        (last_message_at IS NULL) = (last_author_member_id IS NULL)
        AND (last_author_member_id IS NULL OR last_author_member_id IN (opener_member_id, respondent_member_id))
    )
);
-- A respondent's inbox, and closing a removed seat's conversations.
CREATE INDEX conversation_respondent_open_idx ON conversation (respondent_member_id, last_message_at) WHERE status = 'open';
CREATE INDEX conversation_opener_idx          ON conversation (opener_member_id, last_message_at);
-- The lists page by id within a course.
CREATE INDEX conversation_course_idx          ON conversation (course_id, id);

-- Who the participants are, and where, never changes, and a closed
-- conversation stays as it is: starting again is a new conversation. Nor is
-- one deleted: history is kept.
CREATE FUNCTION conversation_check_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'conversation % is kept: DELETE is not allowed; close it instead', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF OLD.status = 'closed' THEN
        RAISE EXCEPTION 'conversation % is closed and stays as it is; start a new one', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.id <> OLD.id OR NEW.course_id <> OLD.course_id OR NEW.opener_member_id <> OLD.opener_member_id
       OR NEW.respondent_member_id <> OLD.respondent_member_id OR NEW.created_at <> OLD.created_at
       OR NEW.title IS DISTINCT FROM OLD.title THEN
        RAISE EXCEPTION 'conversation %: only its status and who spoke last change', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER conversation_guarded
    BEFORE UPDATE OR DELETE ON conversation
    FOR EACH ROW EXECUTE FUNCTION conversation_check_change();

-- ---------------------------------------------------------------------------
-- conversation_message: append-only
-- ---------------------------------------------------------------------------

-- seq orders a conversation's messages, 1, 2, 3, ..., in the order they were
-- written: each is written under the conversation's row lock (above), so the
-- order is that of the lock and not of any instance's clock. "Is there a
-- newer question than the one being answered" is asked of seq.
--
-- An answer names the question it answers (in_reply_to_message_id), which
-- must be the opener's, in the same conversation. course_id is denormalised
-- so that the composite keys make "the message, its conversation and its
-- author are in one course" a database fact. Every message names the action
-- that wrote it, whose payload holds the same body (docs/schema.md §7).
CREATE TABLE conversation_message (
    id                     uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id        uuid        NOT NULL,
    course_id              uuid        NOT NULL,
    seq                    integer     NOT NULL,
    author_member_id       uuid        NOT NULL,
    in_reply_to_message_id uuid,
    body                   text        NOT NULL,
    created_by_action_id   uuid        NOT NULL REFERENCES action (id),
    created_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (conversation_id, seq),
    -- Targets of the reply's and the retraction's composite foreign keys.
    UNIQUE (id, conversation_id),
    UNIQUE (id, course_id),
    FOREIGN KEY (conversation_id, course_id)              REFERENCES conversation (id, course_id),
    FOREIGN KEY (course_id, author_member_id)             REFERENCES course_member (course_id, id),
    FOREIGN KEY (in_reply_to_message_id, conversation_id) REFERENCES conversation_message (id, conversation_id),
    CONSTRAINT conversation_message_seq_positive CHECK (seq >= 1),
    CONSTRAINT conversation_message_body_length  CHECK (char_length(body) BETWEEN 1 AND 20000)
);
CREATE INDEX conversation_message_action_idx ON conversation_message (created_by_action_id);

-- Only the two participants write, and only while the conversation is open;
-- an answer is the respondent's, to a message of the opener's. The
-- conversation is read FOR SHARE, so that a close cannot pass a message
-- written without the update the tools make first.
CREATE FUNCTION conversation_message_check_author() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    c        conversation;
    answered uuid;
BEGIN
    SELECT * INTO c FROM conversation WHERE id = NEW.conversation_id FOR SHARE;
    IF NOT FOUND THEN
        RETURN NEW; -- the foreign key says so
    END IF;
    IF NEW.author_member_id NOT IN (c.opener_member_id, c.respondent_member_id) THEN
        RAISE EXCEPTION 'message %: only the two participants write in conversation %', NEW.id, c.id
            USING ERRCODE = 'check_violation';
    END IF;
    IF c.status <> 'open' THEN
        RAISE EXCEPTION 'message %: conversation % is closed', NEW.id, c.id
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.in_reply_to_message_id IS NOT NULL THEN
        SELECT author_member_id INTO answered FROM conversation_message WHERE id = NEW.in_reply_to_message_id;
        IF NEW.author_member_id <> c.respondent_member_id OR answered IS DISTINCT FROM c.opener_member_id THEN
            RAISE EXCEPTION 'message %: an answer is the respondent''s, to a message of the opener''s', NEW.id
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER conversation_message_author_valid
    BEFORE INSERT ON conversation_message
    FOR EACH ROW EXECUTE FUNCTION conversation_message_check_author();

-- ---------------------------------------------------------------------------
-- conversation_message_retraction: append-only
-- ---------------------------------------------------------------------------

-- Withdrawing a message is a row beside it; the message itself never
-- changes. The read tools withhold a retracted message's body. Who may
-- retract — its author, or someone who decides actions for the opener — is
-- the application's rule.
CREATE TABLE conversation_message_retraction (
    message_id             uuid        PRIMARY KEY,
    course_id              uuid        NOT NULL,
    retracted_by_member_id uuid        NOT NULL,
    created_by_action_id   uuid        NOT NULL REFERENCES action (id),
    reason                 text,
    created_at             timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (message_id, course_id)             REFERENCES conversation_message (id, course_id),
    FOREIGN KEY (course_id, retracted_by_member_id) REFERENCES course_member (course_id, id),
    CONSTRAINT conversation_retraction_reason_length CHECK (reason IS NULL OR char_length(reason) <= 500)
);

-- ---------------------------------------------------------------------------
-- Append-only enforcement
-- ---------------------------------------------------------------------------

CREATE TRIGGER conversation_message_append_only
    BEFORE UPDATE OR DELETE ON conversation_message
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

CREATE TRIGGER conversation_message_no_truncate
    BEFORE TRUNCATE ON conversation_message
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

CREATE TRIGGER conversation_retraction_append_only
    BEFORE UPDATE OR DELETE ON conversation_message_retraction
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

CREATE TRIGGER conversation_retraction_no_truncate
    BEFORE TRUNCATE ON conversation_message_retraction
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

COMMIT;

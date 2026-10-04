-- AIshie Core — migration 0030 (up)
-- An assignment is deleted for good. Design reference: docs/schema.md §2.5
-- (An assignment is deleted for good), §2.6, §4. PostgreSQL 13+.
--
-- Until now nothing of a course's work was ever deleted: an assignment, a
-- submission, a grade, a submitted or feedback file, a version and an event
-- stayed for good, and the database refused to delete the most of them.
-- assignment.delete deletes an assignment with everything that is its: its
-- submissions, of every attempt and state, the grades given on them, their
-- files, its events and the scope rows that name it; and it empties the
-- action log's records of it, which stay as stubs (redacted_by_action_id)
-- because totals, events and decisions point at them. No other document of
-- the course goes or changes with it: its instructions and rubric, and every
-- material, are left in the course as they are, since purging one is an
-- administrator's (document.purge) and deleting an assignment is not.
--
-- The guards that kept those rows open one path, and one only: a row of
-- assignment_deletion naming the assignment, which the deletion writes
-- first, in its own transaction, and which must find the assignment gone by
-- the time it commits (assignment_deletion_whole), as a version's purged_at
-- opens its files to deletion. Through it go the submitted files of the
-- assignment's submissions and the feedback files of the grades given on
-- them, and no other document: what a document is, and whose submission
-- it is of, never changes (document_kept). Outside it they refuse as
-- before, and a little more: a draft submission is no longer deleted
-- either, and nothing deleted one. An assignment's id is never used again
-- once it was deleted (assignment_not_reused), so that the row that opened
-- the path for it opens it for nothing else.
--
-- The files leave the store after the deletion commits, not in it: the
-- deletion queues their keys in blob_deletion, which the job runner drains,
-- retrying what the store refused. Attaching refuses a key that is queued.
--
-- What stays of a deletion is its own action, kept whole (who, when, which
-- assignment, its title and counts), its assignment_deletion row, one event
-- (assignment.deleted, saying its title), the stubs, the totals' history
-- with the deleted assignment's line of their working replaced by a line
-- saying it was deleted, and the documents it named, as they were.
--
-- Indexes, so that one deletion does not read the site's tables once for
-- each row it deletes: the foreign keys to what it deletes are checked by
-- them, and the action log is read a course at a time. Each is made only if
-- it is not there already: a large site builds them first with CREATE INDEX
-- CONCURRENTLY under the same names (docs/deploying.md, Migration 0030), and
-- this migration then takes no lock building them.
--
-- The previous release keeps working while this goes in and after a
-- rollback. It never deletes, so no guard bites it; it reads a stub as an
-- action with an empty payload, and a retry of a stubbed key is refused as
-- an idempotency conflict, the hash having changed; it does not drain
-- blob_deletion, whose files its orphan sweep removes once PROPOSAL_TTL and
-- two days have passed, since nothing names them (never, without a TTL); it
-- cancels a waiting assignment.delete proposal it is asked to decide
-- (tool_removed); and a call of its that races a deletion fails, having
-- changed nothing.

BEGIN;

-- Waits for a busy table rather than queueing every call behind it; a
-- deploy that times out here is run again. Building an index holds off
-- writes to its table while it is built.
SET LOCAL lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- assignment_deletion: the record of a deletion, and the key to its path
-- ---------------------------------------------------------------------------

-- One row for each assignment deleted, written by the deletion before it
-- deletes anything: which assignment of which course, its title, whether
-- students could see it, when, by whom (the actor, and the seat it acted
-- from), by which action, and how much went with it. No foreign key to the
-- assignment, which goes. Kept as it was written.
CREATE TABLE assignment_deletion (
    assignment_id        uuid        PRIMARY KEY,
    course_id            uuid        NOT NULL REFERENCES course (id),
    title                text        NOT NULL,
    was_published        boolean     NOT NULL,
    action_id            uuid        NOT NULL UNIQUE REFERENCES action (id),
    deleted_by_actor_id  uuid        NOT NULL REFERENCES actor (id),
    deleted_by_member_id uuid        NOT NULL,
    deleted_at           timestamptz NOT NULL,
    submissions          integer     NOT NULL,
    grades               integer     NOT NULL,
    files                integer     NOT NULL,
    proposals            integer     NOT NULL,
    totals               integer     NOT NULL,
    FOREIGN KEY (course_id, deleted_by_member_id) REFERENCES course_member (course_id, id),
    CONSTRAINT assignment_deletion_counts_valid CHECK (least(submissions, grades, files, proposals, totals) >= 0)
);

CREATE TRIGGER assignment_deletion_kept
    BEFORE UPDATE OR DELETE ON assignment_deletion
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

CREATE TRIGGER assignment_deletion_no_truncate
    BEFORE TRUNCATE ON assignment_deletion
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- Whether the assignment is being deleted: whether a row of
-- assignment_deletion names it. Only the transaction that wrote the row sees
-- it before it commits, and once it commits the assignment is gone.
CREATE FUNCTION assignment_being_deleted(a uuid) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT EXISTS (SELECT 1 FROM assignment_deletion WHERE assignment_id = a)
$$;

-- A deletion is whole by the time it commits: the assignment it names is
-- gone. A row that would open the guarded path for an assignment that
-- stays is refused.
CREATE FUNCTION assignment_deletion_check_whole() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM assignment WHERE id = NEW.assignment_id) THEN
        RAISE EXCEPTION 'assignment % is recorded as deleted and is still there: a deletion deletes it in the same transaction',
            NEW.assignment_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER assignment_deletion_whole
    AFTER INSERT ON assignment_deletion
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION assignment_deletion_check_whole();

-- ---------------------------------------------------------------------------
-- blob_deletion: files to leave the store once their rows have gone
-- ---------------------------------------------------------------------------

-- One row for each key of the file store whose file is to be deleted:
-- written by the deletion that took its rows, in its transaction, and
-- deleted by the job runner once the store has deleted the file. A store
-- that refuses is asked again later: attempts counts the refusals,
-- next_try_at is when to ask again, and last_error says what it said.
CREATE TABLE blob_deletion (
    storage_key         text        PRIMARY KEY,
    course_id           uuid        NOT NULL REFERENCES course (id),
    queued_by_action_id uuid        NOT NULL REFERENCES action (id),
    queued_at           timestamptz NOT NULL,
    attempts            integer     NOT NULL DEFAULT 0,
    next_try_at         timestamptz NOT NULL,
    last_error          text,
    CONSTRAINT blob_deletion_attempts_valid   CHECK (attempts >= 0),
    CONSTRAINT blob_deletion_key_valid        CHECK (char_length(storage_key) BETWEEN 1 AND 1024),
    CONSTRAINT blob_deletion_last_error_valid CHECK (last_error IS NULL OR char_length(last_error) BETWEEN 1 AND 500)
);
-- The queue: what is due, the oldest first.
CREATE INDEX blob_deletion_due_idx ON blob_deletion (next_try_at);

-- ---------------------------------------------------------------------------
-- action: stubs of what was done to a deleted assignment
-- ---------------------------------------------------------------------------

-- An action about an assignment deleted for good keeps who did it, when,
-- which tool, at what level, on what, and what became of it; what it was
-- given and what it returned go (payload '{}', result null), and
-- redacted_by_action_id names the deletion. Its payload_hash is the hash of
-- an empty call, so that no retry of it is taken for the call it was.
--
-- NOT VALID: every existing row holds null, which passes. Validating would
-- scan the whole log under a lock that stops every write.
ALTER TABLE action ADD COLUMN redacted_by_action_id uuid;
ALTER TABLE action
    ADD CONSTRAINT action_redacted_by_fk FOREIGN KEY (redacted_by_action_id) REFERENCES action (id) NOT VALID,
    ADD CONSTRAINT action_redacted_empty CHECK (
        redacted_by_action_id IS NULL
        OR (payload = '{}'::jsonb AND result IS NULL AND redacted_by_action_id <> id)) NOT VALID;

-- ---------------------------------------------------------------------------
-- The guards, open to a deletion alone
-- ---------------------------------------------------------------------------

-- event: append-only, as 0001 made it, but that the events of an assignment
-- being deleted go with it.
CREATE FUNCTION event_guarded() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' AND OLD.assignment_id IS NOT NULL AND assignment_being_deleted(OLD.assignment_id) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'event is append-only: % is not allowed', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;

DROP TRIGGER event_append_only ON event;
CREATE TRIGGER event_append_only
    BEFORE UPDATE OR DELETE ON event
    FOR EACH ROW EXECUTE FUNCTION event_guarded();

-- submission: frozen once submitted, as 0001 made it, and never deleted,
-- in any state, but with its assignment. 0001 let a draft be deleted, and
-- nothing deleted one.
CREATE OR REPLACE FUNCTION submission_reject_change_after_submit() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    expected submission;
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF assignment_being_deleted(OLD.assignment_id) THEN
            RETURN OLD;
        END IF;
        RAISE EXCEPTION 'submission % is kept: DELETE is not allowed; it goes only with its assignment, deleted for good', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;

    IF OLD.state NOT IN ('submitted', 'late') THEN
        RETURN NEW;
    END IF;

    expected := NEW;
    expected.state := OLD.state;
    IF NEW.state NOT IN ('submitted', 'late') OR expected IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'submission % has been submitted: only submitted <-> late may change; resubmit as a new attempt', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER submission_no_truncate
    BEFORE TRUNCATE ON submission
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- grade: kept, but a grade given on a submission to an assignment being
-- deleted. A grade of a component, a computed total among them, is never
-- deleted.
CREATE FUNCTION grade_check_kept() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.submission_id IS NOT NULL
       AND EXISTS (SELECT 1 FROM submission s WHERE s.id = OLD.submission_id AND assignment_being_deleted(s.assignment_id)) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'grade % is kept: DELETE is not allowed; it goes only with its submission''s assignment, deleted for good', OLD.id
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER grade_kept
    BEFORE DELETE ON grade
    FOR EACH ROW EXECUTE FUNCTION grade_check_kept();

CREATE TRIGGER grade_no_truncate
    BEFORE TRUNCATE ON grade
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- The assignment whose work an owned document is: a submitted file's, its
-- submission's; a feedback file's, its grade's submission's. Null for any
-- other document (material, instructions, a rubric), and for feedback on a
-- component's grade.
CREATE FUNCTION document_owner_assignment(doc uuid) RETURNS uuid
LANGUAGE sql STABLE AS $$
    SELECT s.assignment_id
    FROM document d
    JOIN submission s ON s.id = CASE d.kind
                                    WHEN 'submission' THEN d.submission_id
                                    WHEN 'feedback' THEN (SELECT g.submission_id FROM grade g WHERE g.id = d.grade_id)
                                END
    WHERE d.id = doc
$$;

-- document: kept, but a submitted or feedback file whose assignment is
-- being deleted; and what a document is, and whose, never changes: its kind,
-- its course, and the submission a submitted file is of; a feedback file
-- moves, with its grade written again (MoveFeedbackFiles), only to another
-- grade on the same submission, or from a total to a total. Material,
-- instructions and rubrics are therefore never deleted, nor made into a
-- file an assignment's deletion takes, and no file of other work is moved
-- into one: an administrator purges a course's document (document.purge),
-- and deleting an assignment leaves it as it is.
CREATE FUNCTION document_check_kept() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF (NEW.kind, NEW.course_id, NEW.submission_id) IS DISTINCT FROM (OLD.kind, OLD.course_id, OLD.submission_id) THEN
            RAISE EXCEPTION 'document % keeps its kind, its course and its submission', OLD.id
                USING ERRCODE = 'restrict_violation';
        END IF;
        IF NEW.grade_id IS DISTINCT FROM OLD.grade_id
           AND (SELECT g.submission_id FROM grade g WHERE g.id = NEW.grade_id)
               IS DISTINCT FROM (SELECT g.submission_id FROM grade g WHERE g.id = OLD.grade_id) THEN
            RAISE EXCEPTION 'feedback file % moves only to a grade given on the same submission as its own', OLD.id
                USING ERRCODE = 'restrict_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.kind IN ('submission', 'feedback') AND assignment_being_deleted(document_owner_assignment(OLD.id)) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'document % is kept: DELETE is not allowed; a submitted or feedback file goes only with its assignment, deleted for good',
        OLD.id
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER document_kept
    BEFORE UPDATE OR DELETE ON document
    FOR EACH ROW EXECUTE FUNCTION document_check_kept();

CREATE TRIGGER document_no_truncate
    BEFORE TRUNCATE ON document
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- document_version: 0027's rule, and a version deleted once it is purged,
-- of a submitted or feedback file whose assignment is being deleted. The
-- row is compared whole, so a column added later is frozen too.
CREATE OR REPLACE FUNCTION document_version_guarded() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    expected document_version;
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.purged_at IS NOT NULL AND assignment_being_deleted(document_owner_assignment(OLD.document_id)) THEN
            RETURN OLD;
        END IF;
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

-- assignment: kept, but one being deleted; and an id deleted once is never
-- used again.
CREATE FUNCTION assignment_check_kept() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF assignment_being_deleted(NEW.id) THEN
            RAISE EXCEPTION 'assignment % was deleted for good, and its id is not used again', NEW.id
                USING ERRCODE = 'unique_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF assignment_being_deleted(OLD.id) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'assignment % is kept: DELETE is not allowed; assignment.delete deletes it for good', OLD.id
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER assignment_kept
    BEFORE DELETE ON assignment
    FOR EACH ROW EXECUTE FUNCTION assignment_check_kept();

CREATE TRIGGER assignment_not_reused
    BEFORE INSERT ON assignment
    FOR EACH ROW EXECUTE FUNCTION assignment_check_kept();

CREATE TRIGGER assignment_no_truncate
    BEFORE TRUNCATE ON assignment
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- ---------------------------------------------------------------------------
-- Indexes, for the foreign keys a deletion checks and the log it reads
-- ---------------------------------------------------------------------------

-- The events filed under an assignment.
CREATE INDEX IF NOT EXISTS event_assignment_idx ON event (assignment_id) WHERE assignment_id IS NOT NULL;
-- The grade that replaced a grade.
CREATE INDEX IF NOT EXISTS grade_superseded_by_idx ON grade (superseded_by) WHERE superseded_by IS NOT NULL;
-- A course's actions.
CREATE INDEX IF NOT EXISTS action_course_idx ON action (course_id) WHERE course_id IS NOT NULL;
-- What points at a version, or at a document, from outside it: checked for
-- each version of a submitted or feedback file deleted, and for each such
-- file. A submission pins its instructions' version, a grade its rubric's;
-- an assignment names its instructions and rubric; an answer names the
-- documents and versions it relied on.
CREATE INDEX IF NOT EXISTS submission_instructions_version_idx ON submission (instructions_version_id)
    WHERE instructions_version_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS grade_rubric_version_idx ON grade (rubric_version_id) WHERE rubric_version_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS assignment_instructions_document_idx ON assignment (instructions_document_id)
    WHERE instructions_document_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS assignment_rubric_document_idx ON assignment (rubric_document_id)
    WHERE rubric_document_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS conversation_message_source_document_idx ON conversation_message_source (document_id);
CREATE INDEX IF NOT EXISTS conversation_message_source_version_idx ON conversation_message_source (version_id);

COMMIT;

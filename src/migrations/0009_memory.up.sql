-- AIshiteru Core — migration 0009 (up)
-- Agent memory: what an agent keeps between conversations, held in Core so
-- that whatever runs the agent reads and writes the same. Design reference:
-- docs/schema.md §2.9. PostgreSQL 13+.
--
-- Unlike everything else here, memory is deleted, not retired: what a person
-- asks to be forgotten is gone, and so is what a retention period ends. No
-- other row points at a memory entry. The actions that wrote entries stay,
-- without the text (the tools declare it SecretIn).
--
-- The previous release knows none of this and goes on working. It ignores
-- memory, and its removals of seats are noticed by this release's sweep.

BEGIN;

-- Waits for a busy table rather than queueing every call behind it; a
-- deploy that times out here is run again. The foreign keys below take a
-- lock on actor, course, course_member and action while they are made.
SET LOCAL lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- memory_entry: one thing an agent keeps, in one of three scopes
-- ---------------------------------------------------------------------------

-- owner: about the agent's owner, across courses; course_id, when set, only
-- says where it was learnt. asker: about one person who asks the agent, in
-- one course, keyed on both seats. course: a course's shared memory, keyed
-- on the agent's seat; what the agent writes there waits for review
-- (proposed) before it is used, and a rejection keeps the reviewer's reason
-- without the text.
CREATE TABLE memory_entry (
    id                   uuid        PRIMARY KEY,
    holder_actor_id      uuid        NOT NULL REFERENCES actor (id),
    scope                text        NOT NULL,
    course_id            uuid        REFERENCES course (id),
    holder_member_id     uuid,
    subject_actor_id     uuid        REFERENCES actor (id),
    subject_member_id    uuid,
    -- The limit and the uniqueness of text are per bucket: one per owner,
    -- one per asker's seat, one per the agent's seat for the shared memory.
    bucket               text        NOT NULL GENERATED ALWAYS AS (
        CASE scope
            WHEN 'owner' THEN 'owner'
            WHEN 'asker' THEN 'asker:' || subject_member_id::text
            ELSE 'course:' || holder_member_id::text
        END) STORED,
    status               text        NOT NULL DEFAULT 'active',
    body                 text,
    -- Written by the application (internal/memory.SearchText): the body
    -- normalised, lower-cased, with runs of CJK characters as bigrams, so
    -- that the 'simple' configuration can match Chinese and Japanese.
    search_text          text        NOT NULL DEFAULT '',
    search               tsvector    GENERATED ALWAYS AS (to_tsvector('simple', search_text)) STORED,
    -- sha256 of the normalised body: the same text twice in one bucket is
    -- one entry.
    text_hash            bytea,
    tags                 text[]      NOT NULL DEFAULT '{}',
    pinned               boolean     NOT NULL DEFAULT false,
    source               text        NOT NULL,
    version              integer     NOT NULL DEFAULT 1,
    -- A proposal that corrects an active shared entry names it; approving
    -- the proposal deletes it.
    replaces_id          uuid,
    created_by_actor_id  uuid        NOT NULL REFERENCES actor (id),
    created_by_action_id uuid        NOT NULL REFERENCES action (id),
    updated_by_actor_id  uuid        NOT NULL REFERENCES actor (id),
    updated_by_action_id uuid        NOT NULL REFERENCES action (id),
    created_at           timestamptz NOT NULL,
    updated_at           timestamptz NOT NULL,
    decided_by_member_id uuid,
    decided_at           timestamptz,
    decision_reason      text,
    -- Set when the entry is frozen: its seat removed, its course archived.
    -- Nothing reads a frozen entry for an agent; the sweep deletes it then.
    purge_after          timestamptz,
    purge_reason         text,
    FOREIGN KEY (course_id, holder_member_id)     REFERENCES course_member (course_id, id),
    FOREIGN KEY (course_id, subject_member_id)    REFERENCES course_member (course_id, id),
    FOREIGN KEY (course_id, decided_by_member_id) REFERENCES course_member (course_id, id),
    CONSTRAINT memory_scope_valid  CHECK (scope IN ('owner', 'asker', 'course')),
    CONSTRAINT memory_status_valid CHECK (status IN ('active', 'proposed', 'rejected')),
    CONSTRAINT memory_source_valid CHECK (source IN ('agent', 'owner', 'staff')),
    CONSTRAINT memory_shape_valid  CHECK (
           (scope = 'owner'  AND subject_actor_id IS NOT NULL AND subject_member_id IS NULL
                             AND holder_member_id IS NULL AND status = 'active')
        OR (scope = 'asker'  AND course_id IS NOT NULL AND holder_member_id IS NOT NULL
                             AND subject_member_id IS NOT NULL AND subject_actor_id IS NOT NULL
                             AND status = 'active')
        OR (scope = 'course' AND course_id IS NOT NULL AND holder_member_id IS NOT NULL
                             AND subject_actor_id IS NULL AND subject_member_id IS NULL)
    ),
    CONSTRAINT memory_body_valid CHECK (
           (status = 'rejected' AND body IS NULL AND text_hash IS NULL AND search_text = '')
        OR (status <> 'rejected' AND body IS NOT NULL AND text_hash IS NOT NULL
            AND char_length(body) BETWEEN 1 AND 1000 AND octet_length(body) <= 4000)
    ),
    CONSTRAINT memory_tags_valid     CHECK (cardinality(tags) <= 5),
    CONSTRAINT memory_decision_valid CHECK (
            (status <> 'proposed' OR (decided_at IS NULL AND decided_by_member_id IS NULL))
        AND (status <> 'rejected' OR decided_at IS NOT NULL)
        AND (decision_reason IS NULL OR char_length(decision_reason) <= 500)
    ),
    CONSTRAINT memory_replaces_valid CHECK (replaces_id IS NULL OR (scope = 'course' AND status = 'proposed')),
    CONSTRAINT memory_purge_valid    CHECK ((purge_after IS NULL) = (purge_reason IS NULL)
        AND (purge_reason IS NULL OR purge_reason IN ('seat_removed', 'course_archived')))
);

-- Listing, counting and ranking one bucket.
CREATE INDEX memory_bucket_idx       ON memory_entry (holder_actor_id, bucket, status, updated_at DESC);
-- One text per bucket among what is live.
CREATE UNIQUE INDEX memory_text_key  ON memory_entry (holder_actor_id, bucket, text_hash)
    WHERE status IN ('active', 'proposed');
-- "What agents remember about me", and erasure.
CREATE INDEX memory_subject_idx      ON memory_entry (subject_actor_id) WHERE subject_actor_id IS NOT NULL;
-- A course's shared memory, for its managers.
CREATE INDEX memory_course_idx       ON memory_entry (course_id, holder_member_id) WHERE scope = 'course';
-- The sweep's joins to the seats.
CREATE INDEX memory_holder_seat_idx  ON memory_entry (holder_member_id) WHERE holder_member_id IS NOT NULL;
CREATE INDEX memory_subject_seat_idx ON memory_entry (subject_member_id) WHERE subject_member_id IS NOT NULL;
CREATE INDEX memory_purge_idx        ON memory_entry (purge_after) WHERE purge_after IS NOT NULL;
CREATE INDEX memory_proposed_idx     ON memory_entry (created_at) WHERE status = 'proposed';
CREATE INDEX memory_search_idx       ON memory_entry USING gin (search);

-- What the database holds of the shape: whose, about whom, where. Who may
-- write what is the application's (internal/tools, memoryAccess).
CREATE FUNCTION memory_entry_check() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    holder actor%ROWTYPE;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.id <> OLD.id OR NEW.holder_actor_id <> OLD.holder_actor_id OR NEW.scope <> OLD.scope
           OR NEW.course_id IS DISTINCT FROM OLD.course_id
           OR NEW.holder_member_id IS DISTINCT FROM OLD.holder_member_id
           OR NEW.subject_actor_id IS DISTINCT FROM OLD.subject_actor_id
           OR NEW.subject_member_id IS DISTINCT FROM OLD.subject_member_id
           OR NEW.created_at <> OLD.created_at OR NEW.created_by_actor_id <> OLD.created_by_actor_id
           OR NEW.created_by_action_id <> OLD.created_by_action_id THEN
            RAISE EXCEPTION 'memory entry %: whose it is, about whom and where never change', OLD.id
                USING ERRCODE = 'restrict_violation';
        END IF;
        IF OLD.status = 'rejected' THEN
            RAISE EXCEPTION 'memory entry % was rejected and stays as it is until it is deleted', OLD.id
                USING ERRCODE = 'restrict_violation';
        END IF;
        RETURN NEW;
    END IF;
    SELECT * INTO holder FROM actor WHERE id = NEW.holder_actor_id;
    IF NOT FOUND THEN
        RETURN NEW; -- the foreign key says so
    END IF;
    IF holder.kind <> 'agent' THEN
        RAISE EXCEPTION 'memory is held by agents' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.scope = 'owner' AND NEW.subject_actor_id IS DISTINCT FROM holder.owner_actor_id THEN
        RAISE EXCEPTION 'owner memory is about the agent''s owner' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.holder_member_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM course_member WHERE id = NEW.holder_member_id AND actor_id = NEW.holder_actor_id) THEN
        RAISE EXCEPTION 'the holder''s seat is not the holder''s' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.subject_member_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM course_member WHERE id = NEW.subject_member_id AND actor_id = NEW.subject_actor_id) THEN
        RAISE EXCEPTION 'the subject''s seat is not the subject''s' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER memory_entry_guarded
    BEFORE INSERT OR UPDATE ON memory_entry
    FOR EACH ROW EXECUTE FUNCTION memory_entry_check();

-- ---------------------------------------------------------------------------
-- memory_setting, memory_write_count
-- ---------------------------------------------------------------------------

-- Whether an agent keeps memory at all, as its owner says. No row: on.
CREATE TABLE memory_setting (
    holder_actor_id     uuid        PRIMARY KEY REFERENCES actor (id),
    enabled             boolean     NOT NULL,
    updated_by_actor_id uuid        NOT NULL REFERENCES actor (id),
    updated_at          timestamptz NOT NULL
);

-- Writes an agent made, an hour at a time, for the hourly and daily
-- limits. The sweep deletes rows older than two days.
CREATE TABLE memory_write_count (
    holder_actor_id uuid        NOT NULL REFERENCES actor (id),
    hour            timestamptz NOT NULL,
    n               integer     NOT NULL CHECK (n > 0),
    PRIMARY KEY (holder_actor_id, hour)
);

COMMIT;

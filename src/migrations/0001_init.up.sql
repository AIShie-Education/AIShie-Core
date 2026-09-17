-- AIshideru Core — migration 0001 (up)
-- Design reference: docs/schema.md
--
-- Requires PostgreSQL 13+. gen_random_uuid() is built in from 13, so applying
-- this needs no extension and no elevated privileges.
--
-- IDs: the column default generates a v4 UUID as a fallback only. Application
-- code is expected to supply v7 UUIDs (docs/schema.md §0). event is keyed by a
-- bigserial instead, because nothing references it and it is read in order.
--
-- Status columns are text with a CHECK rather than enum types, so adding or
-- renaming a value is a one-line migration. autonomy_level is the only enum,
-- because it has to be ordered.
--
-- Deletion policy: domain rows are retired through status columns and never
-- hard-deleted. Foreign keys therefore default to NO ACTION, so an accidental
-- DELETE fails loudly instead of cascading through grades and history.
-- ON DELETE CASCADE appears only on rows with no meaning of their own:
-- credentials and scope entries.
--
-- Who-did-it columns inside a course point at course_member rather than actor,
-- so a record carries the role it was made under and stays distinct when the
-- same actor is removed and re-added. created_by / added_by point at actor,
-- because the creator may not be a member: an admin creating a course, or the
-- system actor syncing a roster.

BEGIN;

-- ORDERED. Declaration order is comparison order, so `<`, MIN() and MAX() work
-- directly.
CREATE TYPE autonomy_level AS ENUM (
    'denied',
    'confirm_required',
    'pending_review',
    'autonomous'
);

-- Attached to tables whose rows must never change once written. Corrections
-- are new rows.
CREATE FUNCTION reject_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not allowed', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;

-- ===========================================================================
-- 1. Global
-- ===========================================================================

CREATE TABLE term (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name      text NOT NULL,
    starts_on date NOT NULL,
    ends_on   date NOT NULL,
    CONSTRAINT term_ends_after_start CHECK (ends_on >= starts_on)
);

-- Groups courses. Takes no part in authorization.
CREATE TABLE department (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Humans, agents and the system actor share this table. `kind` is for display
-- and audit only; nothing branches on it. What an actor may do inside a course
-- is entirely on its course_member row; platform_role covers the few
-- operations that happen outside any course.
CREATE TABLE actor (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    kind                text        NOT NULL,
    display_name        text        NOT NULL,
    email               text,
    status              text        NOT NULL DEFAULT 'active',
    platform_role       text,
    -- Null only for the root actor seeded at install time.
    created_by_actor_id uuid        REFERENCES actor (id),
    created_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT actor_kind_valid          CHECK (kind IN ('human', 'agent', 'system')),
    CONSTRAINT actor_status_valid        CHECK (status IN ('active', 'suspended')),
    CONSTRAINT actor_platform_role_valid CHECK (platform_role IS NULL OR platform_role IN ('root', 'admin'))
);
-- Case-insensitive: Yuki@example.edu and yuki@example.edu are one person.
CREATE UNIQUE INDEX actor_email_key ON actor (lower(email)) WHERE email IS NOT NULL;

-- A password, an SSO identity and an API token are three kinds of the same
-- thing. SSO rows hold no secret: the identity provider does the checking.
CREATE TABLE credential (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_id     uuid        NOT NULL REFERENCES actor (id) ON DELETE CASCADE,
    kind         text        NOT NULL,
    secret_hash  text,
    provider     text,
    subject      text,
    token_prefix text,
    label        text,
    last_used_at timestamptz,
    expires_at   timestamptz,
    revoked_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, subject),
    UNIQUE (token_prefix),
    CONSTRAINT credential_kind_valid   CHECK (kind IN ('password', 'sso', 'api_token')),
    CONSTRAINT credential_has_secret   CHECK (kind = 'sso' OR secret_hash IS NOT NULL),
    CONSTRAINT credential_sso_identity CHECK (kind <> 'sso' OR (provider IS NOT NULL AND subject IS NOT NULL)),
    CONSTRAINT credential_token_lookup CHECK (kind <> 'api_token' OR token_prefix IS NOT NULL)
);
CREATE INDEX credential_live_idx ON credential (actor_id) WHERE revoked_at IS NULL;

-- One row per offering: CS101 section A in autumn 2026. Another section or
-- another term is another row with its own members, work and grades.
CREATE TABLE course (
    id                    uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    dept_id               uuid        NOT NULL REFERENCES department (id),
    term_id               uuid        NOT NULL REFERENCES term (id),
    code                  text        NOT NULL,
    section               text        NOT NULL DEFAULT '',
    title                 text        NOT NULL,
    description           text,
    status                text        NOT NULL DEFAULT 'draft',
    copied_from_course_id uuid        REFERENCES course (id),
    created_by_actor_id   uuid        NOT NULL REFERENCES actor (id),
    created_at            timestamptz NOT NULL DEFAULT now(),
    UNIQUE (term_id, code, section),
    CONSTRAINT course_status_valid CHECK (status IN ('draft', 'active', 'archived'))
);
CREATE INDEX course_dept_idx ON course (dept_id);

-- ===========================================================================
-- 2. Membership
-- ===========================================================================

-- A named bundle of role, scope and permission values, copied onto a
-- course_member row when a member is added. dept_id null means available
-- everywhere; the built-ins are seeded by src/seed/presets.sql. A department
-- may define its own. Editing a preset never changes existing members.
--
-- The perm_* columns must stay identical to course_member's; a test asserts
-- it. Adding an action type means adding the column to both tables.
CREATE TABLE permission_preset (
    id                       uuid           PRIMARY KEY DEFAULT gen_random_uuid(),
    dept_id                  uuid           REFERENCES department (id),
    name                     text           NOT NULL,
    description              text,
    role                     text           NOT NULL,
    student_scope            text           NOT NULL,
    assignment_scope         text           NOT NULL,
    perm_document_read       autonomy_level NOT NULL DEFAULT 'denied',
    perm_document_read_draft autonomy_level NOT NULL DEFAULT 'denied',
    perm_document_write      autonomy_level NOT NULL DEFAULT 'denied',
    perm_rubric_read         autonomy_level NOT NULL DEFAULT 'denied',
    perm_assignment_write    autonomy_level NOT NULL DEFAULT 'denied',
    perm_submission_read     autonomy_level NOT NULL DEFAULT 'denied',
    perm_submission_write    autonomy_level NOT NULL DEFAULT 'denied',
    perm_grade_read          autonomy_level NOT NULL DEFAULT 'denied',
    perm_grade_submit        autonomy_level NOT NULL DEFAULT 'denied',
    perm_grade_post          autonomy_level NOT NULL DEFAULT 'denied',
    perm_member_read         autonomy_level NOT NULL DEFAULT 'denied',
    perm_member_manage       autonomy_level NOT NULL DEFAULT 'denied',
    perm_action_decide       autonomy_level NOT NULL DEFAULT 'denied',
    -- Null for built-ins seeded at install time.
    created_by_actor_id      uuid           REFERENCES actor (id),
    created_at               timestamptz    NOT NULL DEFAULT now(),
    CONSTRAINT permission_preset_role_valid  CHECK (role IN ('student', 'instructor', 'ta', 'observer', 'assistant')),
    CONSTRAINT permission_preset_scope_valid CHECK (
        student_scope IN ('all', 'listed') AND assignment_scope IN ('all', 'listed')
    )
);
-- Names are unique among the built-ins and within each department.
CREATE UNIQUE INDEX permission_preset_global_name_key ON permission_preset (name)          WHERE dept_id IS NULL;
CREATE UNIQUE INDEX permission_preset_dept_name_key   ON permission_preset (dept_id, name) WHERE dept_id IS NOT NULL;

-- One row per (actor, course): roster, role and permissions in one place, for
-- humans and agents alike. The roster is the rows with role = 'student'.
--
-- Permissions are one column per action type holding the autonomy level for
-- that action. They are COPIED from a permission_preset when the row is
-- created and may be edited afterwards; preset_id only records which one was
-- used. Authorization reads this row and nothing else.
--
-- student_scope / assignment_scope say whether the member may touch every
-- student / assignment or only those listed in the scope tables. 'listed'
-- with no rows means none, so forgetting to add rows fails closed. A student
-- is 'listed' with a single row pointing at itself.
CREATE TABLE course_member (
    id                       uuid           PRIMARY KEY DEFAULT gen_random_uuid(),
    course_id                uuid           NOT NULL REFERENCES course (id),
    actor_id                 uuid           NOT NULL REFERENCES actor (id),
    role                     text           NOT NULL,
    status                   text           NOT NULL DEFAULT 'active',
    preset_id                uuid           REFERENCES permission_preset (id),
    added_by_actor_id        uuid           NOT NULL REFERENCES actor (id),
    expires_at               timestamptz,
    student_scope            text           NOT NULL,
    assignment_scope         text           NOT NULL,
    perm_document_read       autonomy_level NOT NULL DEFAULT 'denied',
    perm_document_read_draft autonomy_level NOT NULL DEFAULT 'denied',
    perm_document_write      autonomy_level NOT NULL DEFAULT 'denied',
    perm_rubric_read         autonomy_level NOT NULL DEFAULT 'denied',
    perm_assignment_write    autonomy_level NOT NULL DEFAULT 'denied',
    perm_submission_read     autonomy_level NOT NULL DEFAULT 'denied',
    perm_submission_write    autonomy_level NOT NULL DEFAULT 'denied',
    perm_grade_read          autonomy_level NOT NULL DEFAULT 'denied',
    perm_grade_submit        autonomy_level NOT NULL DEFAULT 'denied',
    perm_grade_post          autonomy_level NOT NULL DEFAULT 'denied',
    perm_member_read         autonomy_level NOT NULL DEFAULT 'denied',
    perm_member_manage       autonomy_level NOT NULL DEFAULT 'denied',
    perm_action_decide       autonomy_level NOT NULL DEFAULT 'denied',
    created_at               timestamptz    NOT NULL DEFAULT now(),
    -- Target of the composite foreign key from submission.
    UNIQUE (course_id, id),
    CONSTRAINT course_member_role_valid   CHECK (role IN ('student', 'instructor', 'ta', 'observer', 'assistant')),
    CONSTRAINT course_member_status_valid CHECK (status IN ('active', 'paused', 'removed')),
    CONSTRAINT course_member_scope_valid  CHECK (
        student_scope IN ('all', 'listed') AND assignment_scope IN ('all', 'listed')
    )
);
-- One live membership per actor per course. Removed rows stay for history;
-- re-adding creates a new row with a new id.
CREATE UNIQUE INDEX course_member_one_live ON course_member (course_id, actor_id) WHERE status <> 'removed';
CREATE INDEX course_member_actor_idx ON course_member (actor_id);

CREATE TABLE member_student_scope (
    member_id         uuid NOT NULL REFERENCES course_member (id) ON DELETE CASCADE,
    student_member_id uuid NOT NULL REFERENCES course_member (id),
    PRIMARY KEY (member_id, student_member_id)
);

-- ===========================================================================
-- 3. Grading scheme
-- ===========================================================================

-- A tree per course. The root is the course total and is created with the
-- course; assignments hang off leaves. Only the parameters live here; how
-- they combine (weighted mean, drop-lowest, normalisation) is application
-- code.
CREATE TABLE grade_component (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    course_id       uuid        NOT NULL REFERENCES course (id),
    parent_id       uuid        REFERENCES grade_component (id),
    name            text        NOT NULL,
    weight          numeric     NOT NULL DEFAULT 1,
    drop_lowest     integer     NOT NULL DEFAULT 0,
    -- Set on components graded directly (an exam) rather than from children.
    points_possible numeric,
    sort_order      integer     NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT grade_component_not_own_parent CHECK (parent_id <> id),
    CONSTRAINT grade_component_weight_nonneg  CHECK (weight >= 0),
    CONSTRAINT grade_component_drop_nonneg    CHECK (drop_lowest >= 0),
    CONSTRAINT grade_component_points_nonneg  CHECK (points_possible IS NULL OR points_possible >= 0)
);
CREATE UNIQUE INDEX grade_component_one_root ON grade_component (course_id) WHERE parent_id IS NULL;
CREATE INDEX grade_component_parent_idx ON grade_component (parent_id);

-- ===========================================================================
-- 4. Content
-- ===========================================================================

-- Everything readable is a document: course material, assignment
-- instructions, a rubric, a submitted file, a feedback file. `kind` says
-- which, and for the last two the owner column says whose.
--
-- Material is versioned and published by moving published_version_id;
-- students read the published version, instructors the latest. The other
-- kinds usually have exactly one version.
CREATE TABLE document (
    id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    course_id            uuid        NOT NULL REFERENCES course (id),
    kind                 text        NOT NULL,
    title                text        NOT NULL,
    -- Foreign keys added below, once submission and grade exist.
    submission_id        uuid,
    grade_id             uuid,
    -- Foreign key added below, once document_version exists.
    published_version_id uuid,
    sort_order           integer     NOT NULL DEFAULT 0,
    status               text        NOT NULL DEFAULT 'active',
    created_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT document_kind_valid   CHECK (kind IN ('material', 'instructions', 'rubric', 'submission', 'feedback')),
    CONSTRAINT document_status_valid CHECK (status IN ('active', 'archived')),
    CONSTRAINT document_owner_matches_kind CHECK (
        (submission_id IS NOT NULL) = (kind = 'submission')
        AND (grade_id IS NOT NULL) = (kind = 'feedback')
    )
);
CREATE INDEX document_course_idx     ON document (course_id);
CREATE INDEX document_submission_idx ON document (submission_id) WHERE submission_id IS NOT NULL;
CREATE INDEX document_grade_idx      ON document (grade_id)      WHERE grade_id IS NOT NULL;

-- Append-only. A version is text, a file in object storage, or both; the
-- model reads files directly, so there is no extracted-text step.
CREATE TABLE document_version (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id      uuid        NOT NULL REFERENCES document (id),
    seq              integer     NOT NULL,
    body_md          text,
    storage_key      text        UNIQUE,
    content_type     text,
    byte_size        bigint,
    checksum         text,
    author_member_id uuid        NOT NULL REFERENCES course_member (id),
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (document_id, seq),
    -- Target of document's published-pointer composite foreign key.
    UNIQUE (id, document_id),
    CONSTRAINT document_version_seq_positive   CHECK (seq >= 1),
    CONSTRAINT document_version_has_content    CHECK (body_md IS NOT NULL OR storage_key IS NOT NULL),
    CONSTRAINT document_version_file_described CHECK (
        storage_key IS NULL OR (content_type IS NOT NULL AND byte_size IS NOT NULL AND byte_size >= 0)
    )
);

-- The published pointer must name a version of THIS document.
ALTER TABLE document
    ADD CONSTRAINT document_published_version_fk
    FOREIGN KEY (published_version_id, id) REFERENCES document_version (id, document_id);

-- ===========================================================================
-- 5. Assignments and submissions
-- ===========================================================================

CREATE TABLE assignment (
    id                       uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    course_id                uuid        NOT NULL REFERENCES course (id),
    -- Null: not part of the grade (practice work).
    component_id             uuid        REFERENCES grade_component (id),
    title                    text        NOT NULL,
    instructions_document_id uuid        REFERENCES document (id),
    rubric_document_id       uuid        REFERENCES document (id),
    points_possible          numeric     NOT NULL,
    due_at                   timestamptz,
    published_at             timestamptz,
    created_at               timestamptz NOT NULL DEFAULT now(),
    -- Target of submission's composite foreign key.
    UNIQUE (id, course_id),
    CONSTRAINT assignment_points_nonneg CHECK (points_possible >= 0)
);
CREATE INDEX assignment_course_idx    ON assignment (course_id);
CREATE INDEX assignment_component_idx ON assignment (component_id) WHERE component_id IS NOT NULL;

CREATE TABLE member_assignment_scope (
    member_id     uuid NOT NULL REFERENCES course_member (id) ON DELETE CASCADE,
    assignment_id uuid NOT NULL REFERENCES assignment (id),
    PRIMARY KEY (member_id, assignment_id)
);

-- course_id is denormalized so the composite foreign keys can enforce, in the
-- database, that the assignment and the submitting member belong to the same
-- course. That the member is a student is an application check.
--
-- Frozen once submitted (trigger at the end of this file): resubmitting is a
-- new attempt, so the work a grade was given for never changes under it.
-- Submitted files are documents with kind = 'submission' pointing here.
CREATE TABLE submission (
    id                      uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    assignment_id           uuid        NOT NULL,
    course_id               uuid        NOT NULL,
    student_member_id       uuid        NOT NULL,
    attempt                 integer     NOT NULL DEFAULT 1,
    body                    text,
    -- Pinned at submit time: mid-flight instruction edits are a recurring
    -- source of disputes.
    instructions_version_id uuid        REFERENCES document_version (id),
    state                   text        NOT NULL DEFAULT 'draft',
    submitted_at            timestamptz,
    created_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (assignment_id, student_member_id, attempt),
    -- Target of grade's composite foreign key.
    UNIQUE (id, student_member_id),
    FOREIGN KEY (assignment_id, course_id)     REFERENCES assignment (id, course_id),
    FOREIGN KEY (course_id, student_member_id) REFERENCES course_member (course_id, id),
    CONSTRAINT submission_state_valid      CHECK (state IN ('draft', 'submitted', 'late', 'missing')),
    CONSTRAINT submission_attempt_positive CHECK (attempt >= 1),
    CONSTRAINT submission_time_recorded CHECK (
        state NOT IN ('submitted', 'late') OR submitted_at IS NOT NULL
    )
);
CREATE INDEX submission_student_idx    ON submission (student_member_id);
CREATE INDEX submission_assignment_idx ON submission (assignment_id);

-- ===========================================================================
-- 6. Activity
-- ===========================================================================

-- One row per attempt to do something, written before it happens, including
-- attempts that were denied. The approval queue is WHERE status = 'proposed';
-- the review queue is WHERE review_state = 'pending'. Which status values may
-- follow which authz_result is application logic.
CREATE TABLE action (
    id                    uuid           PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_id              uuid           NOT NULL REFERENCES actor (id),
    -- Null for platform-level operations (creating a course).
    course_id             uuid           REFERENCES course (id),
    -- Null when the actor is not a member of the course.
    member_id             uuid           REFERENCES course_member (id),
    action_type           text           NOT NULL,
    target_type           text           NOT NULL,
    target_id             uuid,
    -- The request. For a confirm_required proposal this is the whole
    -- proposal; nothing else is written until it is approved.
    payload               jsonb          NOT NULL DEFAULT '{}'::jsonb,
    -- Not optional. A tool call retried after a timeout would otherwise post
    -- a second grade silently.
    idempotency_key       text           NOT NULL,
    authz_result          autonomy_level NOT NULL,
    status                text           NOT NULL,
    decided_by_member_id  uuid           REFERENCES course_member (id),
    decided_at            timestamptz,
    review_state          text           NOT NULL DEFAULT 'none',
    reviewed_by_member_id uuid           REFERENCES course_member (id),
    reviewed_at           timestamptz,
    executed_at           timestamptz,
    created_at            timestamptz    NOT NULL DEFAULT now(),
    UNIQUE (actor_id, idempotency_key),
    CONSTRAINT action_status_valid       CHECK (
        status IN ('denied', 'proposed', 'approved', 'rejected', 'cancelled', 'executed', 'failed')
    ),
    CONSTRAINT action_review_state_valid CHECK (review_state IN ('none', 'pending', 'reviewed', 'escalated')),
    -- Nobody approves or reviews their own action.
    CONSTRAINT action_not_self_decided   CHECK (decided_by_member_id <> member_id),
    CONSTRAINT action_not_self_reviewed  CHECK (reviewed_by_member_id <> member_id)
);
CREATE INDEX action_approval_queue_idx ON action (course_id, created_at) WHERE status = 'proposed';
CREATE INDEX action_review_queue_idx   ON action (course_id, created_at) WHERE review_state = 'pending';
CREATE INDEX action_target_idx         ON action (target_type, target_id);
CREATE INDEX action_actor_idx          ON action (actor_id, created_at);
CREATE INDEX action_member_idx         ON action (member_id, created_at) WHERE member_id IS NOT NULL;

-- ===========================================================================
-- 7. Grades
-- ===========================================================================

-- A grade is for exactly one target: a submission, or a component for a
-- student. Draft / posted / superseded is read off posted_at and
-- superseded_by rather than stored again. A regrade appends: the old row
-- gets superseded_by, the new row is posted.
--
-- Rolled-up components and the course total are computed on read and stored
-- only when posted, as an origin = 'computed' snapshot: the number a student
-- was shown must not drift when a lower grade changes later. Feedback files
-- are documents with kind = 'feedback' pointing here.
CREATE TABLE grade (
    id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    student_member_id    uuid        NOT NULL REFERENCES course_member (id),
    submission_id        uuid,
    component_id         uuid        REFERENCES grade_component (id),
    origin               text        NOT NULL,
    score                numeric     NOT NULL,
    feedback             text,
    -- Per-criterion detail for submission grades:
    -- [{criterion, points, max, comment}, ...]
    breakdown            jsonb,
    rubric_version_id    uuid        REFERENCES document_version (id),
    grader_member_id     uuid        NOT NULL REFERENCES course_member (id),
    created_by_action_id uuid        NOT NULL REFERENCES action (id),
    posted_at            timestamptz,
    posted_by_member_id  uuid        REFERENCES course_member (id),
    -- Foreign key added below (deferrable; see the regrade note).
    superseded_by        uuid,
    created_at           timestamptz NOT NULL DEFAULT now(),
    -- Target of the superseded_by composite foreign key.
    UNIQUE (id, student_member_id),
    -- The submission must belong to the student the grade is for.
    FOREIGN KEY (submission_id, student_member_id) REFERENCES submission (id, student_member_id),
    CONSTRAINT grade_one_target             CHECK (num_nonnulls(submission_id, component_id) = 1),
    CONSTRAINT grade_origin_valid           CHECK (origin IN ('entered', 'computed')),
    CONSTRAINT grade_score_nonneg           CHECK (score >= 0),
    CONSTRAINT grade_posted_consistent      CHECK ((posted_at IS NULL) = (posted_by_member_id IS NULL)),
    CONSTRAINT grade_not_superseded_by_self CHECK (superseded_by <> id)
);
CREATE INDEX grade_student_idx    ON grade (student_member_id);
CREATE INDEX grade_submission_idx ON grade (submission_id) WHERE submission_id IS NOT NULL;

-- At most one live grade per target.
CREATE UNIQUE INDEX one_live_submission_grade ON grade (submission_id)
    WHERE posted_at IS NOT NULL AND superseded_by IS NULL;
CREATE UNIQUE INDEX one_live_component_grade ON grade (component_id, student_member_id)
    WHERE posted_at IS NOT NULL AND superseded_by IS NULL;

-- A grade may only be superseded by another grade of the SAME student.
--
-- Regrade, in one transaction, in this order:
--   1. UPDATE the old row:  superseded_by = <new id>
--   2. INSERT the new row:  posted
-- Step 1 points at a row that does not exist yet, which is why this key is
-- DEFERRABLE INITIALLY DEFERRED and checked at COMMIT. The opposite order
-- would briefly produce two live rows and trip the unique index.
ALTER TABLE grade
    ADD CONSTRAINT grade_superseded_by_fk
    FOREIGN KEY (superseded_by, student_member_id) REFERENCES grade (id, student_member_id)
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE document
    ADD CONSTRAINT document_submission_fk FOREIGN KEY (submission_id) REFERENCES submission (id),
    ADD CONSTRAINT document_grade_fk      FOREIGN KEY (grade_id)      REFERENCES grade (id);

-- ===========================================================================
-- 8. Events
-- ===========================================================================

-- Something that happened, written after it did, in the same transaction as
-- the state change. Not every event has an action behind it (a due date
-- passing), and one action may emit several (posting a batch of grades).
-- Append-only. seq is the cursor for "everything since".
CREATE TABLE event (
    seq          bigserial   PRIMARY KEY,
    type         text        NOT NULL,
    course_id    uuid        REFERENCES course (id),
    action_id    uuid        REFERENCES action (id),
    subject_type text        NOT NULL,
    subject_id   uuid,
    payload      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    occurred_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX event_course_idx  ON event (course_id, seq);
CREATE INDEX event_subject_idx ON event (subject_type, subject_id);
CREATE INDEX event_action_idx  ON event (action_id) WHERE action_id IS NOT NULL;

-- ===========================================================================
-- Append-only enforcement
-- ===========================================================================

CREATE TRIGGER document_version_append_only
    BEFORE UPDATE OR DELETE ON document_version
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

CREATE TRIGGER document_version_no_truncate
    BEFORE TRUNCATE ON document_version
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

CREATE TRIGGER event_append_only
    BEFORE UPDATE OR DELETE ON event
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

CREATE TRIGGER event_no_truncate
    BEFORE TRUNCATE ON event
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- ===========================================================================
-- Submission freeze
-- ===========================================================================

-- Once a submission is submitted or late it may not be deleted, and the only
-- permitted change is correcting lateness (submitted <-> late). Everything
-- else is compared as a whole row, so columns added later are frozen by
-- default.
CREATE FUNCTION submission_reject_change_after_submit() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    expected submission;
BEGIN
    IF OLD.state NOT IN ('submitted', 'late') THEN
        IF TG_OP = 'DELETE' THEN
            RETURN OLD;
        END IF;
        RETURN NEW;
    END IF;

    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'submission % has been submitted: DELETE is not allowed', OLD.id
            USING ERRCODE = 'restrict_violation';
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

CREATE TRIGGER submission_frozen_after_submit
    BEFORE UPDATE OR DELETE ON submission
    FOR EACH ROW EXECUTE FUNCTION submission_reject_change_after_submit();

COMMIT;

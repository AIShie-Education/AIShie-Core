-- AIshie Core — migration 0031 (up)
-- Group assignments. Design reference: docs/schema.md §2.5a (Groups), §2.5,
-- §2.7, §3, §4. PostgreSQL 13+.
--
-- A course keeps reusable group sets (a set of project groups, of lab
-- groups), each holding groups, and each group its members, kept as history:
-- who joined which group when, by whom and how, and when they left. An
-- assignment that names a set is a group assignment: each group hands in one
-- piece of work per attempt, a submission owned by the group, and whose work
-- it is — its students, frozen as it was handed in or recorded missing — is
-- a new table, submission_member, which every submission has, an individual
-- one its one student. A grade on a submission is given to one of them: its
-- foreign key moves from the submission's student to submission_member. A
-- group's work is graded once, as a group grade (group_grade), and each
-- member is given an ordinary grade from it, with an individual adjustment
-- where the grader made one, saying what, by how much, why and by whom.
--
-- What counts as a group's member is worked out as it is asked, never
-- stored: a membership not ended, of a seat that is a student's, not removed
-- and not past its expiry (live_group_members). Removing a seat or changing
-- its role needs no second write, and the release before, which knows no
-- groups, cannot leave a stale count behind.
--
-- The release before keeps working while this goes in and after a rollback.
-- It never writes group work, so no guard bites what it does with
-- individual work: a submission it writes gets its submission_member row
-- from the database (submission_member_own), so its grades satisfy the moved
-- foreign key; it posts and writes totals as ever; and it deletes an
-- individual assignment as ever, submission_member going by cascade. Where
-- this release has made group work, reading a group's submission fails that
-- read (a null student it cannot scan), and deleting a group assignment
-- fails whole, changing nothing; an individual submission it tries to write
-- to a group assignment is refused, but an individual 'missing' row there
-- is passed over unwritten (submission_fits_assignment), so its due sweep
-- records nothing for a group assignment, and this release's sweep, under a
-- key of its own, still does.
--
-- One index is rebuilt and two are made on submission and grade, which
-- holds off writes to them while it is built: a large site builds them first
-- with CREATE INDEX CONCURRENTLY under the names this migration gives them,
-- and the migration skips them (docs/deploying.md, Migration 0031).

BEGIN;

SET LOCAL lock_timeout = '10s';

-- ===========================================================================
-- 1. Group sets, groups and their memberships
-- ===========================================================================

-- A set of groups in a course, reused by any number of its assignments:
-- 專題小組, 實驗小組. Its name is unique among the course's sets not
-- archived. Students sign themselves up to its groups while signup_open, and
-- until signup_closes_at if there is one. Archived, never deleted.
CREATE TABLE group_set (
    id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    course_id            uuid        NOT NULL REFERENCES course (id),
    name                 text        NOT NULL,
    description          text,
    signup_open          boolean     NOT NULL DEFAULT false,
    signup_closes_at     timestamptz,
    archived_at          timestamptz,
    created_by_member_id uuid        NOT NULL,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (course_id, id),
    FOREIGN KEY (course_id, created_by_member_id) REFERENCES course_member (course_id, id),
    CONSTRAINT group_set_name_valid CHECK (
        char_length(name) BETWEEN 1 AND 100 AND name = btrim(name) AND name !~ '[\x01-\x1f\x7f]'),
    CONSTRAINT group_set_description_valid CHECK (description IS NULL OR char_length(description) <= 2000)
);
CREATE UNIQUE INDEX group_set_name_key ON group_set (course_id, lower(name)) WHERE archived_at IS NULL;

-- A group of a set. Its name is unique among the set's groups not archived;
-- capacity is what sign-up is held to. Archived, never deleted.
CREATE TABLE course_group (
    id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    course_id            uuid        NOT NULL,
    set_id               uuid        NOT NULL,
    name                 text        NOT NULL,
    capacity             integer,
    archived_at          timestamptz,
    created_by_member_id uuid        NOT NULL,
    created_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (course_id, id),
    UNIQUE (set_id, id),
    FOREIGN KEY (course_id, set_id) REFERENCES group_set (course_id, id),
    FOREIGN KEY (course_id, created_by_member_id) REFERENCES course_member (course_id, id),
    CONSTRAINT course_group_name_valid CHECK (
        char_length(name) BETWEEN 1 AND 100 AND name = btrim(name) AND name !~ '[\x01-\x1f\x7f]'),
    CONSTRAINT course_group_capacity_valid CHECK (capacity IS NULL OR capacity BETWEEN 1 AND 500)
);
CREATE UNIQUE INDEX course_group_name_key ON course_group (set_id, lower(name)) WHERE archived_at IS NULL;

-- Who was in which group of a set when: one row for each stay, begun by
-- whom, how and by which action, and ended once, the same way. A student is
-- in at most one group of a set at a time. Moving a student is two rows: the
-- old stay ended (moved), a new one begun, in one action.
CREATE TABLE group_membership (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    course_id           uuid        NOT NULL,
    set_id              uuid        NOT NULL,
    group_id            uuid        NOT NULL,
    member_id           uuid        NOT NULL,
    joined_at           timestamptz NOT NULL,
    joined_by_member_id uuid        NOT NULL,
    joined_how          text        NOT NULL,
    joined_action_id    uuid        NOT NULL REFERENCES action (id),
    left_at             timestamptz,
    left_by_member_id   uuid,
    left_how            text,
    left_action_id      uuid        REFERENCES action (id),
    FOREIGN KEY (set_id, group_id) REFERENCES course_group (set_id, id),
    FOREIGN KEY (course_id, set_id) REFERENCES group_set (course_id, id),
    FOREIGN KEY (course_id, member_id) REFERENCES course_member (course_id, id),
    FOREIGN KEY (course_id, joined_by_member_id) REFERENCES course_member (course_id, id),
    FOREIGN KEY (course_id, left_by_member_id) REFERENCES course_member (course_id, id),
    CONSTRAINT group_membership_joined_how_valid CHECK (joined_how IN ('assigned', 'split', 'signup')),
    CONSTRAINT group_membership_left_how_valid CHECK (
        left_how IS NULL OR left_how IN ('moved', 'unassigned', 'split', 'left', 'switched')),
    CONSTRAINT group_membership_left_whole CHECK (
        (left_at IS NULL) = (left_by_member_id IS NULL)
        AND (left_at IS NULL) = (left_how IS NULL)
        AND (left_at IS NULL) = (left_action_id IS NULL)),
    CONSTRAINT group_membership_left_after_joined CHECK (left_at IS NULL OR left_at >= joined_at)
);
-- One group of a set at a time.
CREATE UNIQUE INDEX group_membership_one_live ON group_membership (set_id, member_id) WHERE left_at IS NULL;
-- A group's members now.
CREATE INDEX group_membership_group_live_idx ON group_membership (group_id) WHERE left_at IS NULL;
-- A student's history.
CREATE INDEX group_membership_member_idx ON group_membership (member_id);

-- A set and a group stay in their course, and a group in its set; neither is
-- deleted, but archived.
CREATE FUNCTION group_set_check_kept() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'group set % is kept: DELETE is not allowed; archive it', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.course_id IS DISTINCT FROM OLD.course_id THEN
        RAISE EXCEPTION 'group set % stays in its course', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER group_set_kept
    BEFORE UPDATE OR DELETE ON group_set
    FOR EACH ROW EXECUTE FUNCTION group_set_check_kept();

CREATE TRIGGER group_set_no_truncate
    BEFORE TRUNCATE ON group_set
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

CREATE FUNCTION course_group_check_kept() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'group % is kept: DELETE is not allowed; archive it', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF (NEW.course_id, NEW.set_id) IS DISTINCT FROM (OLD.course_id, OLD.set_id) THEN
        RAISE EXCEPTION 'group % stays in its course and its set', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER course_group_kept
    BEFORE UPDATE OR DELETE ON course_group
    FOR EACH ROW EXECUTE FUNCTION course_group_check_kept();

CREATE TRIGGER course_group_no_truncate
    BEFORE TRUNCATE ON course_group
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- A stay is never deleted, and changes only by being ended, once: the four
-- left_ columns, from nothing to something. The rest is compared whole, so a
-- column added later is kept too.
CREATE FUNCTION group_membership_check_kept() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    expected group_membership;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'group membership % is history: DELETE is not allowed', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    expected := OLD;
    expected.left_at := NEW.left_at;
    expected.left_by_member_id := NEW.left_by_member_id;
    expected.left_how := NEW.left_how;
    expected.left_action_id := NEW.left_action_id;
    IF OLD.left_at IS NOT NULL OR NEW IS DISTINCT FROM expected THEN
        RAISE EXCEPTION 'group membership % changes only by being ended, once', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER group_membership_kept
    BEFORE UPDATE OR DELETE ON group_membership
    FOR EACH ROW EXECUTE FUNCTION group_membership_check_kept();

CREATE TRIGGER group_membership_no_truncate
    BEFORE TRUNCATE ON group_membership
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- Who counts as a group's member, now: a stay not ended, of a seat whose
-- roster role is student, not removed, and not past its expiry. A paused
-- student is still one. Reading the role here is reading the roster, as the
-- due sweep does; nothing authorizes on it.
CREATE FUNCTION live_group_members(g uuid) RETURNS SETOF uuid
LANGUAGE sql STABLE AS $$
    SELECT gm.member_id
    FROM group_membership gm
    JOIN course_member m ON m.id = gm.member_id
    WHERE gm.group_id = g AND gm.left_at IS NULL
      AND m.role = 'student' AND m.status <> 'removed'
      AND (m.expires_at IS NULL OR m.expires_at > now())
$$;

-- ===========================================================================
-- 2. An assignment's group set
-- ===========================================================================

-- An assignment that names a set is a group assignment. NOT VALID: every
-- existing row holds null, which passes; the key is checked for every row
-- written from now on.
ALTER TABLE assignment ADD COLUMN group_set_id uuid;
ALTER TABLE assignment
    ADD CONSTRAINT assignment_group_set_fk FOREIGN KEY (course_id, group_set_id) REFERENCES group_set (course_id, id) NOT VALID;
CREATE INDEX assignment_group_set_idx ON assignment (group_set_id) WHERE group_set_id IS NOT NULL;

-- Which set it names changes only while nothing has been started on it: work
-- already there belongs to a student or to a group, and changing the kind
-- would leave it the other kind. assignment.update holds the assignment FOR
-- UPDATE as it changes it, which a submission's key share waits for.
CREATE FUNCTION assignment_check_group_set_fixed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.group_set_id IS DISTINCT FROM OLD.group_set_id
       AND EXISTS (SELECT 1 FROM submission s WHERE s.assignment_id = OLD.id) THEN
        RAISE EXCEPTION 'assignment % has submissions: its group set no longer changes', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER assignment_group_set_fixed
    BEFORE UPDATE OF group_set_id ON assignment
    FOR EACH ROW EXECUTE FUNCTION assignment_check_group_set_fixed();

-- ===========================================================================
-- 3. Submissions: a student's, or a group's
-- ===========================================================================

-- group_id: the group whose work it is, in place of a student. revision: a
-- draft's body changed counts one, dated (revised_at) and, by this release,
-- said by whom (revised_by_member_id), so that several people writing one
-- draft each name the revision they wrote over. submitted_by_member_id: the
-- seat whose hand-in it was. Every row so far is a student's; the new
-- columns take their defaults, which needs no rewrite.
ALTER TABLE submission
    ADD COLUMN group_id uuid,
    ADD COLUMN revision integer NOT NULL DEFAULT 1,
    ADD COLUMN revised_at timestamptz,
    ADD COLUMN revised_by_member_id uuid,
    ADD COLUMN submitted_by_member_id uuid,
    ALTER COLUMN student_member_id DROP NOT NULL;
-- NOT VALID: every row has a student and no group, which passes.
ALTER TABLE submission
    ADD CONSTRAINT submission_one_owner CHECK (num_nonnulls(student_member_id, group_id) = 1) NOT VALID,
    ADD CONSTRAINT submission_revision_positive CHECK (revision >= 1) NOT VALID,
    ADD CONSTRAINT submission_group_fk FOREIGN KEY (course_id, group_id) REFERENCES course_group (course_id, id) NOT VALID,
    ADD CONSTRAINT submission_revised_by_fk FOREIGN KEY (course_id, revised_by_member_id)
        REFERENCES course_member (course_id, id) NOT VALID,
    ADD CONSTRAINT submission_submitted_by_fk FOREIGN KEY (course_id, submitted_by_member_id)
        REFERENCES course_member (course_id, id) NOT VALID;
-- A group's attempts, as a student's are numbered: one row per attempt.
CREATE UNIQUE INDEX IF NOT EXISTS submission_group_attempt_key ON submission (assignment_id, group_id, attempt)
    WHERE group_id IS NOT NULL;

-- Whose work it is, its assignment and its course never change.
CREATE FUNCTION submission_check_owner_fixed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.student_member_id, NEW.group_id, NEW.assignment_id, NEW.course_id)
       IS DISTINCT FROM (OLD.student_member_id, OLD.group_id, OLD.assignment_id, OLD.course_id) THEN
        RAISE EXCEPTION 'submission % stays whose it is, and of its assignment and course', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER submission_owner_fixed
    BEFORE UPDATE ON submission
    FOR EACH ROW EXECUTE FUNCTION submission_check_owner_fixed();

-- A group's work goes only to an assignment naming the group's set, and a
-- student's only to one naming none. The assignment is read under its key
-- share, which a change of its set (FOR UPDATE) holds off or is held off by.
-- A student's 'missing' row for a group assignment is passed over, not
-- written: the release before's due sweep writes one for every student who
-- handed nothing in, and finds it written by nobody.
CREATE FUNCTION submission_check_fits_assignment() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    set_of_assignment uuid;
BEGIN
    SELECT a.group_set_id INTO set_of_assignment FROM assignment a WHERE a.id = NEW.assignment_id FOR KEY SHARE;
    IF NEW.group_id IS NOT NULL THEN
        IF set_of_assignment IS NULL
           OR NOT EXISTS (SELECT 1 FROM course_group g WHERE g.id = NEW.group_id AND g.set_id = set_of_assignment) THEN
            RAISE EXCEPTION 'submission %: a group''s work goes only to an assignment of the group''s set', NEW.id
                USING ERRCODE = 'check_violation';
        END IF;
    ELSIF set_of_assignment IS NOT NULL THEN
        IF NEW.state = 'missing' THEN
            RETURN NULL;
        END IF;
        RAISE EXCEPTION 'submission %: a group assignment takes a group''s work, not a student''s', NEW.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER submission_fits_assignment
    BEFORE INSERT ON submission
    FOR EACH ROW EXECUTE FUNCTION submission_check_fits_assignment();

-- A draft's body changed counts a revision and dates it, whichever release
-- writes it: this release's edit says which revision it was made from, and
-- a stale one is refused.
CREATE FUNCTION submission_count_revision() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.state = 'draft' AND NEW.state = 'draft' AND NEW.body IS DISTINCT FROM OLD.body THEN
        NEW.revision := OLD.revision + 1;
        IF NEW.revised_at IS NOT DISTINCT FROM OLD.revised_at THEN
            NEW.revised_at := now();
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER submission_draft_revised
    BEFORE UPDATE OF body ON submission
    FOR EACH ROW EXECUTE FUNCTION submission_count_revision();

-- A group's work, its live members now, and every one of them.
CREATE INDEX IF NOT EXISTS submission_group_idx ON submission (group_id) WHERE group_id IS NOT NULL;

-- ===========================================================================
-- 4. Whose work a submission is
-- ===========================================================================

-- One row for each student whose work it is: an individual submission's one
-- row is its student, written with it by the database; a group's are its
-- members as it was handed in or recorded missing, frozen then. Every grade
-- on a submission is given to one of them. course_id, assignment_id and
-- group_id are the submission's, copied as the row is written.
CREATE TABLE submission_member (
    submission_id      uuid        NOT NULL REFERENCES submission (id) ON DELETE CASCADE,
    member_id          uuid        NOT NULL,
    course_id          uuid        NOT NULL,
    assignment_id      uuid        NOT NULL,
    group_id           uuid,
    added_at           timestamptz NOT NULL,
    added_how          text        NOT NULL,
    added_by_member_id uuid,
    PRIMARY KEY (submission_id, member_id),
    FOREIGN KEY (course_id, member_id) REFERENCES course_member (course_id, id),
    FOREIGN KEY (course_id, added_by_member_id) REFERENCES course_member (course_id, id),
    CONSTRAINT submission_member_added_how_valid CHECK (added_how IN ('own', 'hand_in', 'missing', 'corrected'))
);
CREATE INDEX submission_member_member_idx ON submission_member (member_id);
CREATE INDEX submission_member_assignment_idx ON submission_member (assignment_id, member_id);

-- The guard. A row is written for a submission as it stands: an
-- individual's for its student; a group's only once it is handed in or
-- recorded missing, and never for a student another group's work for the
-- same assignment names. A row never changes. An individual's goes only with
-- its assignment, deleted for good. A group's goes while its submission is a
-- draft again (a 'missing' row taken over by late work), by a correction of
-- whose work it is, which says so in its own transaction
-- (aishie.correcting_submission names the submission), or with its
-- assignment; the grade's foreign key refuses it while a grade names it.
CREATE FUNCTION submission_member_check_guarded() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    work submission;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT * INTO work FROM submission WHERE id = NEW.submission_id;
        IF NOT FOUND THEN
            RETURN NEW; -- the foreign key refuses it
        END IF;
        NEW.course_id := work.course_id;
        NEW.assignment_id := work.assignment_id;
        NEW.group_id := work.group_id;
        IF work.group_id IS NULL THEN
            IF NEW.member_id IS DISTINCT FROM work.student_member_id OR NEW.added_how <> 'own' THEN
                RAISE EXCEPTION 'submission % is its student''s alone', work.id
                    USING ERRCODE = 'check_violation';
            END IF;
            RETURN NEW;
        END IF;
        IF NEW.added_how = 'own' THEN
            RAISE EXCEPTION 'submission % is a group''s: none of it is one student''s own', work.id
                USING ERRCODE = 'check_violation';
        END IF;
        IF work.state = 'draft' THEN
            RAISE EXCEPTION 'submission % is a draft: whose it is is its group''s members now, until it is handed in', work.id
                USING ERRCODE = 'check_violation';
        END IF;
        IF EXISTS (SELECT 1 FROM submission_member x
                   WHERE x.assignment_id = work.assignment_id AND x.member_id = NEW.member_id
                     AND x.group_id IS DISTINCT FROM work.group_id) THEN
            RAISE EXCEPTION 'member % is part of another group''s work for assignment %', NEW.member_id, work.assignment_id
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'whose work submission % is changes by a row added or removed, never by one changed', OLD.submission_id
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF assignment_being_deleted(OLD.assignment_id) THEN
        RETURN OLD;
    END IF;
    IF OLD.group_id IS NOT NULL
       AND (EXISTS (SELECT 1 FROM submission x WHERE x.id = OLD.submission_id AND x.state = 'draft')
            OR current_setting('aishie.correcting_submission', true) = OLD.submission_id::text) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'whose work submission % is, is kept: DELETE is not allowed', OLD.submission_id
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER submission_member_guarded
    BEFORE INSERT OR UPDATE OR DELETE ON submission_member
    FOR EACH ROW EXECUTE FUNCTION submission_member_check_guarded();

CREATE TRIGGER submission_member_no_truncate
    BEFORE TRUNCATE ON submission_member
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- An individual submission's one row, written with it, whichever release
-- writes it.
CREATE FUNCTION submission_member_write_own() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO submission_member (submission_id, member_id, course_id, assignment_id, added_at, added_how)
    VALUES (NEW.id, NEW.student_member_id, NEW.course_id, NEW.assignment_id, NEW.created_at, 'own');
    RETURN NULL;
END;
$$;

CREATE TRIGGER submission_member_own
    AFTER INSERT ON submission
    FOR EACH ROW WHEN (NEW.student_member_id IS NOT NULL)
    EXECUTE FUNCTION submission_member_write_own();

-- Every submission so far is its student's.
INSERT INTO submission_member (submission_id, member_id, course_id, assignment_id, added_at, added_how)
SELECT s.id, s.student_member_id, s.course_id, s.assignment_id, s.created_at, 'own'
FROM submission s
WHERE s.student_member_id IS NOT NULL;

-- The students of a submission: its student, for an individual one; for a
-- group's draft, the group's live members; for a group's work handed in or
-- recorded missing, its rows. Every scope check and list filter about a
-- submission goes through it.
CREATE FUNCTION submission_students(sub uuid) RETURNS SETOF uuid
LANGUAGE sql STABLE AS $$
    SELECT s.student_member_id FROM submission s WHERE s.id = sub AND s.student_member_id IS NOT NULL
    UNION ALL
    SELECT live_group_members(s.group_id) FROM submission s WHERE s.id = sub AND s.group_id IS NOT NULL AND s.state = 'draft'
    UNION ALL
    SELECT x.member_id FROM submission s JOIN submission_member x ON x.submission_id = s.id
    WHERE s.id = sub AND s.group_id IS NOT NULL AND s.state <> 'draft'
$$;

-- ===========================================================================
-- 5. Group grades, and the members' grades from them
-- ===========================================================================

-- What a group's work was given, as a group: kept as written. Each member's
-- grade is an ordinary grade row given from it (grade.group_grade_id), which
-- is what counts; this is the shared record, with the feedback files.
CREATE TABLE group_grade (
    id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    course_id            uuid        NOT NULL,
    submission_id        uuid        NOT NULL REFERENCES submission (id),
    score                numeric     NOT NULL,
    out_of               numeric     NOT NULL,
    allow_extra          boolean     NOT NULL DEFAULT false,
    feedback             text,
    breakdown            jsonb,
    rubric_version_id    uuid        REFERENCES document_version (id),
    grader_member_id     uuid        NOT NULL,
    created_by_action_id uuid        NOT NULL REFERENCES action (id),
    created_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, submission_id),
    FOREIGN KEY (course_id, grader_member_id) REFERENCES course_member (course_id, id),
    CONSTRAINT group_grade_score_nonneg CHECK (score >= 0),
    CONSTRAINT group_grade_out_of_nonneg CHECK (out_of >= 0)
);
CREATE INDEX group_grade_submission_idx ON group_grade (submission_id);

-- Given only to a group's work handed in or recorded missing, in its course;
-- kept as written; deleted only with its assignment, deleted for good.
CREATE FUNCTION group_grade_check_kept() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    work submission;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT * INTO work FROM submission WHERE id = NEW.submission_id;
        IF NOT FOUND THEN
            RETURN NEW; -- the foreign key refuses it
        END IF;
        IF work.group_id IS NULL OR work.state = 'draft' THEN
            RAISE EXCEPTION 'a group grade is given to a group''s work handed in or recorded missing, and submission % is not', work.id
                USING ERRCODE = 'check_violation';
        END IF;
        NEW.course_id := work.course_id;
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'group grade % is kept as written', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF EXISTS (SELECT 1 FROM submission s WHERE s.id = OLD.submission_id AND assignment_being_deleted(s.assignment_id)) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'group grade % is kept: DELETE is not allowed; it goes only with its assignment, deleted for good', OLD.id
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER group_grade_kept
    BEFORE INSERT OR UPDATE OR DELETE ON group_grade
    FOR EACH ROW EXECUTE FUNCTION group_grade_check_kept();

CREATE TRIGGER group_grade_no_truncate
    BEFORE TRUNCATE ON group_grade
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- A member's grade from a group grade, and its adjustment: replace (a score
-- of its own) or delta (plus or minus the group's), with a reason and who
-- made it. NOT VALID: every row holds nulls, which pass.
ALTER TABLE grade
    ADD COLUMN group_grade_id uuid,
    ADD COLUMN adjust_kind text,
    ADD COLUMN adjust_points numeric,
    ADD COLUMN adjust_reason text,
    ADD COLUMN adjust_by_member_id uuid;
ALTER TABLE grade
    ADD CONSTRAINT grade_group_grade_fk FOREIGN KEY (group_grade_id, submission_id)
        REFERENCES group_grade (id, submission_id) NOT VALID,
    ADD CONSTRAINT grade_adjust_by_fk FOREIGN KEY (adjust_by_member_id) REFERENCES course_member (id) NOT VALID,
    ADD CONSTRAINT grade_group_grade_of_submission CHECK (group_grade_id IS NULL OR submission_id IS NOT NULL) NOT VALID,
    ADD CONSTRAINT grade_adjust_kind_valid CHECK (adjust_kind IS NULL OR adjust_kind IN ('replace', 'delta')) NOT VALID,
    ADD CONSTRAINT grade_adjust_whole CHECK (
        (adjust_kind IS NULL) = (adjust_points IS NULL)
        AND (adjust_kind IS NOT NULL OR (adjust_reason IS NULL AND adjust_by_member_id IS NULL))) NOT VALID,
    ADD CONSTRAINT grade_adjust_from_group_grade CHECK (adjust_kind IS NULL OR group_grade_id IS NOT NULL) NOT VALID,
    ADD CONSTRAINT grade_adjust_said CHECK (
        adjust_kind IS NULL OR adjust_kind NOT IN ('replace', 'delta')
        OR (adjust_reason IS NOT NULL AND char_length(adjust_reason) BETWEEN 1 AND 500 AND adjust_by_member_id IS NOT NULL)) NOT VALID,
    ADD CONSTRAINT grade_adjust_replace_nonneg CHECK (adjust_kind IS DISTINCT FROM 'replace' OR adjust_points >= 0) NOT VALID;
CREATE INDEX grade_group_grade_idx ON grade (group_grade_id) WHERE group_grade_id IS NOT NULL;

-- A grade on a submission is given to someone whose work it is: the key
-- moves from the submission's student to submission_member. NOT VALID: every
-- existing grade's row was written above.
ALTER TABLE grade DROP CONSTRAINT grade_submission_id_student_member_id_fkey;
ALTER TABLE grade
    ADD CONSTRAINT grade_submission_member_fk FOREIGN KEY (submission_id, student_member_id)
        REFERENCES submission_member (submission_id, member_id) NOT VALID;

-- One live posted grade per student on a piece of work, where it was one per
-- submission: a group's work has several. Built under a name of its own, or
-- found built so (CONCURRENTLY, docs/deploying.md), then put in the old
-- one's place.
CREATE UNIQUE INDEX IF NOT EXISTS one_live_submission_member_grade ON grade (submission_id, student_member_id)
    WHERE posted_at IS NOT NULL AND superseded_by IS NULL;
DROP INDEX one_live_submission_grade;
ALTER INDEX one_live_submission_member_grade RENAME TO one_live_submission_grade;

-- ===========================================================================
-- 6. A group grade's feedback files
-- ===========================================================================

-- A feedback file is a grade's or a group grade's, one of them. NOT VALID:
-- every row names no group grade.
ALTER TABLE document ADD COLUMN group_grade_id uuid;
ALTER TABLE document
    ADD CONSTRAINT document_group_grade_fk FOREIGN KEY (group_grade_id) REFERENCES group_grade (id) NOT VALID,
    DROP CONSTRAINT document_owner_matches_kind,
    ADD CONSTRAINT document_owner_one CHECK (
        (submission_id IS NOT NULL) = (kind = 'submission')
        AND (num_nonnulls(grade_id, group_grade_id) = 1) = (kind = 'feedback')
        AND num_nonnulls(grade_id, group_grade_id) <= 1) NOT VALID;
CREATE INDEX document_group_grade_idx ON document (group_grade_id) WHERE group_grade_id IS NOT NULL;

-- The assignment whose work an owned document is: a submitted file's, its
-- submission's; a feedback file's, its grade's or its group grade's
-- submission's. Null for any other document, and for feedback on a
-- component's grade.
CREATE OR REPLACE FUNCTION document_owner_assignment(doc uuid) RETURNS uuid
LANGUAGE sql STABLE AS $$
    SELECT s.assignment_id
    FROM document d
    JOIN submission s ON s.id = CASE
                                    WHEN d.kind = 'submission' THEN d.submission_id
                                    WHEN d.kind = 'feedback' AND d.group_grade_id IS NOT NULL
                                        THEN (SELECT gg.submission_id FROM group_grade gg WHERE gg.id = d.group_grade_id)
                                    WHEN d.kind = 'feedback' THEN (SELECT g.submission_id FROM grade g WHERE g.id = d.grade_id)
                                END
    WHERE d.id = doc
$$;

-- document: as 0030 made it, and a group grade's feedback file moves only to
-- another group grade on the same submission, never to a grade, nor a
-- grade's to a group grade.
CREATE OR REPLACE FUNCTION document_check_kept() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF (NEW.kind, NEW.course_id, NEW.submission_id) IS DISTINCT FROM (OLD.kind, OLD.course_id, OLD.submission_id) THEN
            RAISE EXCEPTION 'document % keeps its kind, its course and its submission', OLD.id
                USING ERRCODE = 'restrict_violation';
        END IF;
        IF (NEW.grade_id IS NULL) <> (OLD.grade_id IS NULL) OR (NEW.group_grade_id IS NULL) <> (OLD.group_grade_id IS NULL) THEN
            RAISE EXCEPTION 'feedback file % stays a grade''s, or a group grade''s', OLD.id
                USING ERRCODE = 'restrict_violation';
        END IF;
        IF NEW.grade_id IS DISTINCT FROM OLD.grade_id
           AND (SELECT g.submission_id FROM grade g WHERE g.id = NEW.grade_id)
               IS DISTINCT FROM (SELECT g.submission_id FROM grade g WHERE g.id = OLD.grade_id) THEN
            RAISE EXCEPTION 'feedback file % moves only to a grade given on the same submission as its own', OLD.id
                USING ERRCODE = 'restrict_violation';
        END IF;
        IF NEW.group_grade_id IS DISTINCT FROM OLD.group_grade_id
           AND (SELECT gg.submission_id FROM group_grade gg WHERE gg.id = NEW.group_grade_id)
               IS DISTINCT FROM (SELECT gg.submission_id FROM group_grade gg WHERE gg.id = OLD.group_grade_id) THEN
            RAISE EXCEPTION 'feedback file % moves only to a group grade given on the same submission as its own', OLD.id
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

COMMIT;

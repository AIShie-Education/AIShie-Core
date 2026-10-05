-- AIshie Core — migration 0031 (down)
-- Reverts 0031_group_work.up.sql.
--
-- It refuses, changing nothing, while any submission is a group's: the
-- schema before cannot hold a group's work, and a down migration does not
-- delete students' work. Delete the group assignments first
-- (assignment.delete), or restore from a backup taken before 0031.
--
-- Otherwise it drops what 0031 added and puts back 0030's guards, the grade's
-- key to submission (id, student_member_id), one live posted grade per
-- submission, the document's owner check, and a submission's student, never
-- null.
--
-- Lost:
--   * group sets, groups, their memberships and the history of them;
--   * which assignments were group assignments: they become individual,
--     having no work;
--   * drafts' revisions, and who handed each submission in.

BEGIN;

SET LOCAL lock_timeout = '10s';

DO $down$
BEGIN
    IF EXISTS (SELECT 1 FROM submission WHERE group_id IS NOT NULL) THEN
        RAISE EXCEPTION 'group work exists: delete the group assignments (assignment.delete) or restore from a backup taken before 0031'
            USING ERRCODE = 'restrict_violation';
    END IF;
END $down$;

-- ---------------------------------------------------------------------------
-- 6. Documents, as 0030 left them
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION document_check_kept() RETURNS trigger
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

CREATE OR REPLACE FUNCTION document_owner_assignment(doc uuid) RETURNS uuid
LANGUAGE sql STABLE AS $$
    SELECT s.assignment_id
    FROM document d
    JOIN submission s ON s.id = CASE d.kind
                                    WHEN 'submission' THEN d.submission_id
                                    WHEN 'feedback' THEN (SELECT g.submission_id FROM grade g WHERE g.id = d.grade_id)
                                END
    WHERE d.id = doc
$$;

-- No document names a group grade: there is none, with no group's work.
DROP INDEX IF EXISTS document_group_grade_idx;
ALTER TABLE document
    DROP CONSTRAINT IF EXISTS document_owner_one,
    DROP CONSTRAINT IF EXISTS document_group_grade_fk,
    DROP COLUMN IF EXISTS group_grade_id,
    ADD CONSTRAINT document_owner_matches_kind CHECK (
        (submission_id IS NOT NULL) = (kind = 'submission') AND (grade_id IS NOT NULL) = (kind = 'feedback')) NOT VALID;
-- Validated, as 0001 had it: a scan that holds off no write.
ALTER TABLE document VALIDATE CONSTRAINT document_owner_matches_kind;

-- ---------------------------------------------------------------------------
-- 5. Grades, as 0030 left them
-- ---------------------------------------------------------------------------

CREATE UNIQUE INDEX IF NOT EXISTS one_live_submission_grade_down ON grade (submission_id)
    WHERE posted_at IS NOT NULL AND superseded_by IS NULL;
DROP INDEX one_live_submission_grade;
ALTER INDEX one_live_submission_grade_down RENAME TO one_live_submission_grade;

-- Every submission is a student's, so every grade on one names its student.
ALTER TABLE grade DROP CONSTRAINT IF EXISTS grade_submission_member_fk;
ALTER TABLE grade
    ADD CONSTRAINT grade_submission_id_student_member_id_fkey FOREIGN KEY (submission_id, student_member_id)
        REFERENCES submission (id, student_member_id) NOT VALID;
ALTER TABLE grade VALIDATE CONSTRAINT grade_submission_id_student_member_id_fkey;

DROP INDEX IF EXISTS grade_group_grade_idx;
ALTER TABLE grade
    DROP CONSTRAINT IF EXISTS grade_group_grade_fk,
    DROP CONSTRAINT IF EXISTS grade_adjust_by_fk,
    DROP CONSTRAINT IF EXISTS grade_group_grade_of_submission,
    DROP CONSTRAINT IF EXISTS grade_adjust_kind_valid,
    DROP CONSTRAINT IF EXISTS grade_adjust_whole,
    DROP CONSTRAINT IF EXISTS grade_adjust_from_group_grade,
    DROP CONSTRAINT IF EXISTS grade_adjust_said,
    DROP CONSTRAINT IF EXISTS grade_adjust_replace_nonneg,
    DROP COLUMN IF EXISTS group_grade_id,
    DROP COLUMN IF EXISTS adjust_kind,
    DROP COLUMN IF EXISTS adjust_points,
    DROP COLUMN IF EXISTS adjust_reason,
    DROP COLUMN IF EXISTS adjust_by_member_id;

-- Its triggers go with it.
DROP TABLE IF EXISTS group_grade;
DROP FUNCTION IF EXISTS group_grade_check_kept();

-- ---------------------------------------------------------------------------
-- 4. Whose work a submission is
-- ---------------------------------------------------------------------------

DROP FUNCTION IF EXISTS submission_students(uuid);
DROP TRIGGER IF EXISTS submission_member_own ON submission;
DROP FUNCTION IF EXISTS submission_member_write_own();
DROP TABLE IF EXISTS submission_member;
DROP FUNCTION IF EXISTS submission_member_check_guarded();

-- ---------------------------------------------------------------------------
-- 3. Submissions, a student's alone
-- ---------------------------------------------------------------------------

DROP TRIGGER IF EXISTS submission_owner_fixed ON submission;
DROP FUNCTION IF EXISTS submission_check_owner_fixed();
DROP TRIGGER IF EXISTS submission_fits_assignment ON submission;
DROP FUNCTION IF EXISTS submission_check_fits_assignment();
DROP TRIGGER IF EXISTS submission_draft_revised ON submission;
DROP FUNCTION IF EXISTS submission_count_revision();
DROP INDEX IF EXISTS submission_group_attempt_key;
DROP INDEX IF EXISTS submission_group_idx;
ALTER TABLE submission
    DROP CONSTRAINT IF EXISTS submission_one_owner,
    DROP CONSTRAINT IF EXISTS submission_revision_positive,
    DROP CONSTRAINT IF EXISTS submission_group_fk,
    DROP CONSTRAINT IF EXISTS submission_revised_by_fk,
    DROP CONSTRAINT IF EXISTS submission_submitted_by_fk,
    DROP COLUMN IF EXISTS group_id,
    DROP COLUMN IF EXISTS revision,
    DROP COLUMN IF EXISTS revised_at,
    DROP COLUMN IF EXISTS revised_by_member_id,
    DROP COLUMN IF EXISTS submitted_by_member_id,
    ALTER COLUMN student_member_id SET NOT NULL;

-- ---------------------------------------------------------------------------
-- 2. Assignments name no set
-- ---------------------------------------------------------------------------

DROP TRIGGER IF EXISTS assignment_group_set_fixed ON assignment;
DROP FUNCTION IF EXISTS assignment_check_group_set_fixed();
DROP INDEX IF EXISTS assignment_group_set_idx;
ALTER TABLE assignment
    DROP CONSTRAINT IF EXISTS assignment_group_set_fk,
    DROP COLUMN IF EXISTS group_set_id;

-- ---------------------------------------------------------------------------
-- 1. Group sets, groups and memberships
-- ---------------------------------------------------------------------------

DROP FUNCTION IF EXISTS live_group_members(uuid);
DROP TABLE IF EXISTS group_membership;
DROP FUNCTION IF EXISTS group_membership_check_kept();
DROP TABLE IF EXISTS course_group;
DROP FUNCTION IF EXISTS course_group_check_kept();
DROP TABLE IF EXISTS group_set;
DROP FUNCTION IF EXISTS group_set_check_kept();

COMMIT;

-- AIshie Core — after 0031_group_work.down.sql, in `make db-test-sql`
--
-- The schema is 0030's again: no sets, groups or memberships, no group
-- grades, nothing of whose work a submission is but its student; a grade's
-- key to its submission's student, one live posted grade per submission,
-- a document's owner as its kind says, and a submission's student never
-- null. Clinical is an individual assignment, nobody having started on it;
-- Wei's graded essay of tests/up/0031 is as it was.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
DECLARE
    left_over text;
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public'
               AND table_name IN ('group_set', 'course_group', 'group_membership', 'submission_member', 'group_grade')) THEN
        RAISE EXCEPTION 'FAIL  0031 down: a table it added is still there';
    END IF;
    SELECT string_agg(table_name || '.' || column_name, ', ') INTO left_over FROM information_schema.columns
    WHERE table_schema = 'public' AND (table_name, column_name) IN (
        ('assignment', 'group_set_id'), ('submission', 'group_id'), ('submission', 'revision'), ('submission', 'revised_at'),
        ('submission', 'revised_by_member_id'), ('submission', 'submitted_by_member_id'), ('grade', 'group_grade_id'),
        ('grade', 'adjust_kind'), ('grade', 'adjust_points'), ('grade', 'adjust_reason'), ('grade', 'adjust_by_member_id'),
        ('document', 'group_grade_id'));
    IF left_over IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  0031 down: the columns % are still there', left_over;
    END IF;
    SELECT string_agg(proname, ', ') INTO left_over FROM pg_proc
    WHERE proname IN ('live_group_members', 'submission_students', 'group_set_check_kept', 'course_group_check_kept',
                      'group_membership_check_kept', 'assignment_check_group_set_fixed', 'submission_check_owner_fixed',
                      'submission_check_fits_assignment', 'submission_count_revision', 'submission_member_check_guarded',
                      'submission_member_write_own', 'group_grade_check_kept');
    IF left_over IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  0031 down: the functions % are still there', left_over;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'grade_submission_id_student_member_id_fkey' AND convalidated)
       OR NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'document_owner_matches_kind' AND convalidated)
       OR EXISTS (SELECT 1 FROM pg_constraint WHERE conname IN ('grade_submission_member_fk', 'document_owner_one', 'submission_one_owner'))
       OR (SELECT is_nullable FROM information_schema.columns WHERE table_name = 'submission' AND column_name = 'student_member_id') <> 'NO'
       OR (SELECT pg_get_indexdef('one_live_submission_grade'::regclass)) NOT LIKE '%(submission_id)%'
       OR EXISTS (SELECT 1 FROM pg_class WHERE relname = 'one_live_submission_grade_down') THEN
        RAISE EXCEPTION 'FAIL  0031 down: the grade''s key, its one live posted grade, a document''s owner or a submission''s student are not as 0030 left them';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM assignment WHERE id = '00000000-0000-0000-0031-000000000173')
       OR NOT EXISTS (SELECT 1 FROM grade WHERE id = '00000000-0000-0000-0031-0000000000d1' AND posted_at IS NOT NULL) THEN
        RAISE EXCEPTION 'FAIL  0031 down: Clinical, or Wei''s graded essay, is not as it was left';
    END IF;
END $chk$;
\echo 'PASS  0031 down with groups and a group assignment nobody started on: the schema is 0030''s, and the work there was stays'

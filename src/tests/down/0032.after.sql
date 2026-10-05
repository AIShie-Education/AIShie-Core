-- AIshie Core — after 0032_peer_evaluation.down.sql, in `make db-test-sql`
--
-- The schema is 0031's again: no peer forms, sheets or entries, no detail
-- of an adjustment, and an adjustment is replace or delta. Wei's and Kai's
-- peer adjustments of tests/down/0032.before.sql are deltas of the same
-- points, said to be peer evaluation's, by Lin, who graded them, their
-- scores what they were given. Then Handover is deleted for good, as the
-- release before deletes an assignment, so that 0031's down, which refuses
-- while any group's work is left, goes down. Committed.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
DECLARE
    left_over text;
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public'
               AND table_name IN ('peer_form', 'peer_review', 'peer_review_entry')) THEN
        RAISE EXCEPTION 'FAIL  0032 down: a table it added is still there';
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'grade' AND column_name = 'adjust_detail') THEN
        RAISE EXCEPTION 'FAIL  0032 down: grade.adjust_detail is still there';
    END IF;
    SELECT string_agg(proname, ', ') INTO left_over FROM pg_proc
    WHERE proname IN ('peer_criteria_valid', 'peer_form_check_shape_fixed', 'peer_review_check_kept', 'peer_review_entry_check_kept',
                      'grade_peer_adjustment_unsay');
    IF left_over IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  0032 down: the functions % are still there', left_over;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conname IN ('grade_adjust_peer_unsaid', 'grade_adjust_detail_of_peer'))
       OR (SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'grade_adjust_kind_valid') LIKE '%peer%' THEN
        RAISE EXCEPTION 'FAIL  0032 down: an adjustment''s kinds are not 0031''s';
    END IF;
    IF (SELECT count(*) FROM grade
        WHERE id IN ('00000000-0000-0000-0032-0000000001d1', '00000000-0000-0000-0032-0000000001d2') AND adjust_kind = 'delta'
          AND adjust_reason = 'peer evaluation' AND adjust_by_member_id = '00000000-0000-0000-0018-000000000051'
          AND ((id = '00000000-0000-0000-0032-0000000001d1' AND adjust_points = 0.9 AND score = 18.9)
               OR (id = '00000000-0000-0000-0032-0000000001d2' AND adjust_points = -0.9 AND score = 17.1))) <> 2 THEN
        RAISE EXCEPTION 'FAIL  0032 down: Wei''s and Kai''s peer adjustments are not deltas of the same points, by Lin';
    END IF;
END $chk$;

-- Each constraint 0031 left unchecked holds of the grades rewritten.
BEGIN;
ALTER TABLE grade VALIDATE CONSTRAINT grade_adjust_kind_valid;
ALTER TABLE grade VALIDATE CONSTRAINT grade_adjust_said;
ALTER TABLE grade VALIDATE CONSTRAINT grade_adjust_whole;
ROLLBACK;

BEGIN;
INSERT INTO action (id, actor_id, course_id, member_id, action_type, target_type, target_id, payload, payload_hash,
                    idempotency_key, authz_result, status, executed_at)
VALUES ('00000000-0000-0000-0032-0000000001b4', '00000000-0000-0000-0018-000000000031', '00000000-0000-0000-0018-000000000041',
        '00000000-0000-0000-0018-000000000051', 'assignment.delete', 'assignment', '00000000-0000-0000-0032-000000000174', '{}',
        repeat('0', 64), 'down32-b4', 'autonomous', 'executed', now());
INSERT INTO assignment_deletion (assignment_id, course_id, title, was_published, action_id, deleted_by_actor_id,
                                 deleted_by_member_id, deleted_at, submissions, grades, files, proposals, totals)
VALUES ('00000000-0000-0000-0032-000000000174', '00000000-0000-0000-0018-000000000041', 'Handover', true,
        '00000000-0000-0000-0032-0000000001b4', '00000000-0000-0000-0018-000000000031', '00000000-0000-0000-0018-000000000051',
        now(), 1, 2, 0, 0, 0);
DELETE FROM grade WHERE submission_id IN (SELECT id FROM submission WHERE assignment_id = '00000000-0000-0000-0032-000000000174');
DELETE FROM group_grade WHERE submission_id IN (SELECT id FROM submission WHERE assignment_id = '00000000-0000-0000-0032-000000000174');
DELETE FROM submission WHERE assignment_id = '00000000-0000-0000-0032-000000000174';
DELETE FROM assignment WHERE id = '00000000-0000-0000-0032-000000000174';
COMMIT;
\echo 'PASS  0032 down with a peer form, sheets and peer adjustments: the schema is 0031''s, each adjustment a delta of the same points'

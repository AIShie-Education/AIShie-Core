-- AIshie Core — after 0032_peer_evaluation.up.sql, in `make db-test-sql`
--
-- The migration made what peer evaluation needs, and every grade there was
-- holds each constraint it added unchecked: Kai's delta of
-- tests/up/0032.before.sql stays as Lin gave it. Then, in NUR101, this
-- release gives Ward round a peer form, Wei and Kai each write a sheet, and
-- Wei's grade is written again with a peer adjustment; the release before
-- writes it again carrying that on, as a kind it does not know, with an
-- empty reason and the nil member as who made it, which the database takes
-- as none; and deletes Ward round for good, as it deletes an assignment, its
-- peer form and sheets going with it by cascade. Committed: no group's work
-- is left for 0031's down.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
DECLARE
    missing text;
BEGIN
    SELECT string_agg(t, ', ') INTO missing
    FROM unnest(ARRAY['peer_form', 'peer_review', 'peer_review_entry']) AS t
    WHERE NOT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = t);
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  0032 up: no table %', missing;
    END IF;
    SELECT string_agg(t, ', ') INTO missing
    FROM unnest(ARRAY['peer_form_shape_fixed', 'peer_review_kept', 'peer_review_no_truncate', 'peer_review_entry_kept',
                      'peer_review_entry_no_truncate', 'grade_peer_adjustment_unsaid']) AS t
    WHERE NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = t AND NOT tgisinternal);
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  0032 up: no trigger %', missing;
    END IF;
    SELECT string_agg(i, ', ') INTO missing
    FROM unnest(ARRAY['peer_review_one_current', 'peer_review_assignment_idx', 'peer_review_group_idx']) AS i
    WHERE NOT EXISTS (SELECT 1 FROM pg_class c JOIN pg_index x ON x.indexrelid = c.oid WHERE c.relname = i AND x.indisvalid);
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  0032 up: no valid index %', missing;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'grade' AND column_name = 'adjust_detail') THEN
        RAISE EXCEPTION 'FAIL  0032 up: no grade.adjust_detail';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM grade WHERE id = '00000000-0000-0000-0032-0000000000d2' AND adjust_kind = 'delta' AND adjust_points = -2
                   AND adjust_reason = 'Left the round early' AND adjust_detail IS NULL) THEN
        RAISE EXCEPTION 'FAIL  0032 up: Kai''s adjustment is not as Lin gave it';
    END IF;
END $chk$;

-- Each constraint added unchecked holds of the rows there were.
BEGIN;
ALTER TABLE grade VALIDATE CONSTRAINT grade_adjust_kind_valid;
ALTER TABLE grade VALIDATE CONSTRAINT grade_adjust_peer_unsaid;
ALTER TABLE grade VALIDATE CONSTRAINT grade_adjust_detail_of_peer;
ROLLBACK;

BEGIN;
-- This release: Ward round's peer form, Wei's and Kai's sheets (r1, r2),
-- and Wei's grade written again with a peer adjustment (d3). b4 the sheets'
-- action · b5 the deletion
INSERT INTO action (id, actor_id, course_id, member_id, action_type, target_type, target_id, payload, payload_hash,
                    idempotency_key, authz_result, status, executed_at) VALUES
    ('00000000-0000-0000-0032-0000000000b4', '00000000-0000-0000-0018-000000000033', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000053', 'peer_review.submit', 'assignment', '00000000-0000-0000-0032-000000000074', '{}',
     repeat('0', 64), 'up32-b4', 'autonomous', 'executed', now()),
    ('00000000-0000-0000-0032-0000000000b5', '00000000-0000-0000-0018-000000000031', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000051', 'assignment.delete', 'assignment', '00000000-0000-0000-0032-000000000074', '{}',
     repeat('0', 64), 'up32-b5', 'autonomous', 'executed', now());
INSERT INTO peer_form (assignment_id, course_id, kind, opens, closes_at, weight, created_by_member_id, updated_by_member_id)
VALUES ('00000000-0000-0000-0032-000000000074', '00000000-0000-0000-0018-000000000041', 'share', 'on_hand_in', now() + interval '1 day',
        10, '00000000-0000-0000-0018-000000000051', '00000000-0000-0000-0018-000000000051');
INSERT INTO peer_review (id, course_id, assignment_id, group_id, rater_member_id, created_by_action_id) VALUES
    ('00000000-0000-0000-0032-0000000000f1', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0032-000000000074',
     '00000000-0000-0000-0032-0000000000c2', '00000000-0000-0000-0018-000000000053', '00000000-0000-0000-0032-0000000000b4'),
    ('00000000-0000-0000-0032-0000000000f2', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0032-000000000074',
     '00000000-0000-0000-0032-0000000000c2', '00000000-0000-0000-0032-000000000057', '00000000-0000-0000-0032-0000000000b4');
INSERT INTO peer_review_entry (review_id, course_id, ratee_member_id, share) VALUES
    ('00000000-0000-0000-0032-0000000000f1', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0032-000000000057', 100),
    ('00000000-0000-0000-0032-0000000000f2', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0018-000000000053', 100);
UPDATE grade SET superseded_by = '00000000-0000-0000-0032-0000000000d3' WHERE id = '00000000-0000-0000-0032-0000000000d1';
INSERT INTO grade (id, student_member_id, submission_id, origin, score, grader_member_id, created_by_action_id, posted_at,
                   posted_by_member_id, group_grade_id, adjust_kind, adjust_points, adjust_detail)
VALUES ('00000000-0000-0000-0032-0000000000d3', '00000000-0000-0000-0018-000000000053', '00000000-0000-0000-0032-0000000000a1',
        'entered', 18, '00000000-0000-0000-0018-000000000051', '00000000-0000-0000-0032-0000000000b3', now(),
        '00000000-0000-0000-0018-000000000051', '00000000-0000-0000-0032-0000000000e1', 'peer', 0,
        '{"factor": 1, "weight": 10, "raters": 1, "form_version": 1}');
-- The release before writes Wei's grade again, carrying the adjustment on.
UPDATE grade SET superseded_by = '00000000-0000-0000-0032-0000000000d4' WHERE id = '00000000-0000-0000-0032-0000000000d3';
INSERT INTO grade (id, student_member_id, submission_id, origin, score, grader_member_id, created_by_action_id, posted_at,
                   posted_by_member_id, group_grade_id, adjust_kind, adjust_points, adjust_reason, adjust_by_member_id)
VALUES ('00000000-0000-0000-0032-0000000000d4', '00000000-0000-0000-0018-000000000053', '00000000-0000-0000-0032-0000000000a1',
        'entered', 18, '00000000-0000-0000-0018-000000000051', '00000000-0000-0000-0032-0000000000b3', now(),
        '00000000-0000-0000-0018-000000000051', '00000000-0000-0000-0032-0000000000e1', 'peer', 0, '',
        '00000000-0000-0000-0000-000000000000');
SET CONSTRAINTS ALL IMMEDIATE;
SET CONSTRAINTS ALL DEFERRED;
DO $chk$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM grade WHERE id = '00000000-0000-0000-0032-0000000000d4' AND adjust_kind = 'peer'
                   AND adjust_reason IS NULL AND adjust_by_member_id IS NULL AND adjust_detail IS NULL) THEN
        RAISE EXCEPTION 'FAIL  0032 up: the release before''s carried peer adjustment is not taken as by nobody';
    END IF;
END $chk$;
-- And deletes Ward round for good, as it deletes an assignment.
INSERT INTO assignment_deletion (assignment_id, course_id, title, was_published, action_id, deleted_by_actor_id,
                                 deleted_by_member_id, deleted_at, submissions, grades, files, proposals, totals)
VALUES ('00000000-0000-0000-0032-000000000074', '00000000-0000-0000-0018-000000000041', 'Ward round', true,
        '00000000-0000-0000-0032-0000000000b5', '00000000-0000-0000-0018-000000000031', '00000000-0000-0000-0018-000000000051',
        now(), 1, 2, 0, 0, 0);
DELETE FROM grade WHERE submission_id IN (SELECT id FROM submission WHERE assignment_id = '00000000-0000-0000-0032-000000000074');
DELETE FROM group_grade WHERE submission_id IN (SELECT id FROM submission WHERE assignment_id = '00000000-0000-0000-0032-000000000074');
DELETE FROM submission WHERE assignment_id = '00000000-0000-0000-0032-000000000074';
DELETE FROM assignment WHERE id = '00000000-0000-0000-0032-000000000074';
SET CONSTRAINTS ALL IMMEDIATE;
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM peer_form) OR EXISTS (SELECT 1 FROM peer_review) OR EXISTS (SELECT 1 FROM peer_review_entry)
       OR EXISTS (SELECT 1 FROM submission WHERE group_id IS NOT NULL) THEN
        RAISE EXCEPTION 'FAIL  0032 up: Ward round''s peer evaluation, or its work, is still there';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM course_group WHERE id = '00000000-0000-0000-0032-0000000000c2') THEN
        RAISE EXCEPTION 'FAIL  0032 up: Ward A went with it';
    END IF;
END $chk$;
COMMIT;
\echo 'PASS  0032 up holds of the grades there were, takes the release before''s carried peer adjustment as by nobody, and lets it delete an assignment with its peer evaluation'

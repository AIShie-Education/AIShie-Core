-- AIshie Core — after 0031_group_work.up.sql, in `make db-test-sql`
--
-- The migration made what group work needs, put the index of
-- tests/up/0031.before.sql in place of one live posted grade per submission,
-- and made every submission there was its student's (submission_member), so
-- that every grade on one is given to someone whose work it is: each
-- constraint it added unchecked holds of the rows there were. Then the
-- release before works as it did, on this schema, in NUR101: Wei's work on a
-- new quiz is started, handed in, graded and posted, and the quiz deleted
-- for good with it; and, once Lin has made Clinical a group assignment of a
-- set whose one group is Wei's, the release before's due sweep writes
-- nothing for her there, and her own draft of it is refused. Undone after:
-- nothing that was there changes.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
DECLARE
    missing text;
BEGIN
    SELECT string_agg(t, ', ') INTO missing
    FROM unnest(ARRAY['group_set', 'course_group', 'group_membership', 'submission_member', 'group_grade']) AS t
    WHERE NOT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = t);
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  0031 up: no table %', missing;
    END IF;
    SELECT string_agg(t, ', ') INTO missing
    FROM unnest(ARRAY['group_set_kept', 'group_set_no_truncate', 'course_group_kept', 'course_group_no_truncate',
                      'group_membership_kept', 'group_membership_no_truncate', 'assignment_group_set_fixed',
                      'submission_owner_fixed', 'submission_fits_assignment', 'submission_draft_revised',
                      'submission_member_guarded', 'submission_member_no_truncate', 'submission_member_own',
                      'group_grade_kept', 'group_grade_no_truncate']) AS t
    WHERE NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = t AND NOT tgisinternal);
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  0031 up: no trigger %', missing;
    END IF;
    SELECT string_agg(i, ', ') INTO missing
    FROM unnest(ARRAY['one_live_submission_grade', 'group_set_name_key', 'course_group_name_key', 'group_membership_one_live',
                      'group_membership_group_live_idx', 'group_membership_member_idx', 'assignment_group_set_idx',
                      'submission_group_attempt_key', 'submission_group_idx', 'submission_member_member_idx',
                      'submission_member_assignment_idx', 'group_grade_submission_idx', 'grade_group_grade_idx',
                      'document_group_grade_idx']) AS i
    WHERE NOT EXISTS (SELECT 1 FROM pg_class c JOIN pg_index x ON x.indexrelid = c.oid WHERE c.relname = i AND x.indisvalid);
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  0031 up: no valid index %', missing;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_class WHERE relname = 'one_live_submission_member_grade')
       OR (SELECT pg_get_indexdef('one_live_submission_grade'::regclass)) NOT LIKE '%(submission_id, student_member_id)%' THEN
        RAISE EXCEPTION 'FAIL  0031 up: one live posted grade is not one per student on a piece of work: %',
            pg_get_indexdef('one_live_submission_grade'::regclass);
    END IF;
    -- Every submission there was is its student's, and only hers.
    IF (SELECT count(*) FROM submission) <> (SELECT count(*) FROM submission_member)
       OR EXISTS (SELECT 1 FROM submission s
                  WHERE NOT EXISTS (SELECT 1 FROM submission_member x
                                    WHERE x.submission_id = s.id AND x.member_id = s.student_member_id AND x.added_how = 'own'
                                      AND x.assignment_id = s.assignment_id AND x.course_id = s.course_id
                                      AND x.added_at = s.created_at)) THEN
        RAISE EXCEPTION 'FAIL  0031 up: a submission there was is not its student''s, and hers alone';
    END IF;
    IF EXISTS (SELECT 1 FROM submission WHERE group_id IS NOT NULL OR revision <> 1 OR submitted_by_member_id IS NOT NULL)
       OR EXISTS (SELECT 1 FROM assignment WHERE group_set_id IS NOT NULL)
       OR EXISTS (SELECT 1 FROM grade WHERE group_grade_id IS NOT NULL OR adjust_kind IS NOT NULL) THEN
        RAISE EXCEPTION 'FAIL  0031 up: a row there was says something of groups';
    END IF;
END $chk$;

-- Each constraint added unchecked holds of the rows there were.
BEGIN;
ALTER TABLE assignment VALIDATE CONSTRAINT assignment_group_set_fk;
ALTER TABLE submission VALIDATE CONSTRAINT submission_one_owner;
ALTER TABLE submission VALIDATE CONSTRAINT submission_revision_positive;
ALTER TABLE submission VALIDATE CONSTRAINT submission_group_fk;
ALTER TABLE grade VALIDATE CONSTRAINT grade_submission_member_fk;
ALTER TABLE grade VALIDATE CONSTRAINT grade_group_grade_fk;
ALTER TABLE grade VALIDATE CONSTRAINT grade_adjust_whole;
ALTER TABLE grade VALIDATE CONSTRAINT grade_adjust_said;
ALTER TABLE document VALIDATE CONSTRAINT document_owner_one;
ALTER TABLE document VALIDATE CONSTRAINT document_group_grade_fk;
ROLLBACK;

BEGIN;
-- The release before, on this schema. 72 Dosage quiz · a2 Wei's work on it ·
-- b2 Lin's grade · d2 the grade · b3 the quiz's deletion
INSERT INTO assignment (id, course_id, title, points_possible, published_at)
VALUES ('00000000-0000-0000-0031-000000000072', '00000000-0000-0000-0018-000000000041', 'Dosage quiz', 5, now());
INSERT INTO submission (id, assignment_id, course_id, student_member_id, attempt, body, state, created_at)
VALUES ('00000000-0000-0000-0031-0000000000a2', '00000000-0000-0000-0031-000000000072', '00000000-0000-0000-0018-000000000041',
        '00000000-0000-0000-0018-000000000053', 1, 'My answers', 'draft', now());
UPDATE submission SET body = 'My answers, checked' WHERE id = '00000000-0000-0000-0031-0000000000a2' AND state = 'draft';
UPDATE submission SET state = 'submitted', submitted_at = now(), instructions_version_id = NULL
WHERE id = '00000000-0000-0000-0031-0000000000a2' AND state = 'draft';
INSERT INTO action (id, actor_id, course_id, member_id, action_type, target_type, target_id, payload, payload_hash,
                    idempotency_key, authz_result, status, executed_at) VALUES
    ('00000000-0000-0000-0031-0000000000b2', '00000000-0000-0000-0018-000000000031', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000051', 'grade.submit', 'submission', '00000000-0000-0000-0031-0000000000a2', '{"score": 4}',
     repeat('0', 64), 'up31-b2', 'autonomous', 'executed', now()),
    ('00000000-0000-0000-0031-0000000000b3', '00000000-0000-0000-0018-000000000031', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000051', 'assignment.delete', 'assignment', '00000000-0000-0000-0031-000000000072', '{}',
     repeat('0', 64), 'up31-b3', 'autonomous', 'executed', now());
INSERT INTO grade (id, student_member_id, submission_id, origin, score, grader_member_id, created_by_action_id)
VALUES ('00000000-0000-0000-0031-0000000000d2', '00000000-0000-0000-0018-000000000053', '00000000-0000-0000-0031-0000000000a2',
        'entered', 4, '00000000-0000-0000-0018-000000000051', '00000000-0000-0000-0031-0000000000b2');
UPDATE grade SET posted_at = now(), posted_by_member_id = '00000000-0000-0000-0018-000000000051'
WHERE id = '00000000-0000-0000-0031-0000000000d2' AND posted_at IS NULL AND superseded_by IS NULL;
SET CONSTRAINTS ALL IMMEDIATE;
DO $chk$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM submission_member WHERE submission_id = '00000000-0000-0000-0031-0000000000a2'
                   AND member_id = '00000000-0000-0000-0018-000000000053' AND added_how = 'own')
       OR (SELECT revision FROM submission WHERE id = '00000000-0000-0000-0031-0000000000a2') <> 2 THEN
        RAISE EXCEPTION 'FAIL  0031 up: the release before''s submission is not its student''s, or its edit counted no revision';
    END IF;
END $chk$;
SET CONSTRAINTS ALL DEFERRED;
-- Deleted for good, as the release before deletes it.
INSERT INTO assignment_deletion (assignment_id, course_id, title, was_published, action_id, deleted_by_actor_id,
                                 deleted_by_member_id, deleted_at, submissions, grades, files, proposals, totals)
VALUES ('00000000-0000-0000-0031-000000000072', '00000000-0000-0000-0018-000000000041', 'Dosage quiz', true,
        '00000000-0000-0000-0031-0000000000b3', '00000000-0000-0000-0018-000000000031', '00000000-0000-0000-0018-000000000051',
        now(), 1, 1, 0, 0, 0);
DELETE FROM grade WHERE submission_id IN (SELECT id FROM submission WHERE assignment_id = '00000000-0000-0000-0031-000000000072');
DELETE FROM submission WHERE assignment_id = '00000000-0000-0000-0031-000000000072';
DELETE FROM assignment WHERE id = '00000000-0000-0000-0031-000000000072';
SET CONSTRAINTS ALL IMMEDIATE;
SET CONSTRAINTS ALL DEFERRED;

-- This release makes Clinical a group assignment: s1 Clinical groups, g1 Ward
-- A, Wei in it; b4 the placing.
INSERT INTO action (id, actor_id, course_id, member_id, action_type, target_type, payload, payload_hash, idempotency_key,
                    authz_result, status, executed_at)
VALUES ('00000000-0000-0000-0031-0000000000b4', '00000000-0000-0000-0018-000000000031', '00000000-0000-0000-0018-000000000041',
        '00000000-0000-0000-0018-000000000051', 'group.set_members', 'group_set', '{}', repeat('0', 64), 'up31-b4', 'autonomous',
        'executed', now());
INSERT INTO group_set (id, course_id, name, created_by_member_id)
VALUES ('00000000-0000-0000-0031-0000000000c1', '00000000-0000-0000-0018-000000000041', 'Clinical groups',
        '00000000-0000-0000-0018-000000000051');
INSERT INTO course_group (id, course_id, set_id, name, created_by_member_id)
VALUES ('00000000-0000-0000-0031-0000000000c2', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0031-0000000000c1',
        'Ward A', '00000000-0000-0000-0018-000000000051');
INSERT INTO group_membership (course_id, set_id, group_id, member_id, joined_at, joined_by_member_id, joined_how, joined_action_id)
VALUES ('00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0031-0000000000c1', '00000000-0000-0000-0031-0000000000c2',
        '00000000-0000-0000-0018-000000000053', now(), '00000000-0000-0000-0018-000000000051', 'assigned',
        '00000000-0000-0000-0031-0000000000b4');
INSERT INTO assignment (id, course_id, title, points_possible, published_at, due_at, group_set_id)
VALUES ('00000000-0000-0000-0031-000000000073', '00000000-0000-0000-0018-000000000041', 'Clinical', 20, now(),
        now() - interval '1 hour', '00000000-0000-0000-0031-0000000000c1');
DO $chk$
DECLARE
    n int;
    refused boolean := false;
BEGIN
    -- The release before's due sweep (InsertMissingSubmission): passed over.
    INSERT INTO submission (id, assignment_id, course_id, student_member_id, attempt, state, created_at)
    VALUES (gen_random_uuid(), '00000000-0000-0000-0031-000000000073', '00000000-0000-0000-0018-000000000041',
            '00000000-0000-0000-0018-000000000053', 1, 'missing', now())
    ON CONFLICT (assignment_id, student_member_id, attempt) DO NOTHING;
    GET DIAGNOSTICS n = ROW_COUNT;
    IF n <> 0 THEN
        RAISE EXCEPTION 'FAIL  0031 up: the release before recorded a student missing from a group assignment';
    END IF;
    -- Its submission.create (InsertSubmission): refused.
    BEGIN
        INSERT INTO submission (id, assignment_id, course_id, student_member_id, attempt, body, state, created_at)
        VALUES (gen_random_uuid(), '00000000-0000-0000-0031-000000000073', '00000000-0000-0000-0018-000000000041',
                '00000000-0000-0000-0018-000000000053', 1, 'Mine', 'draft', now());
    EXCEPTION WHEN check_violation THEN
        refused := true;
    END;
    IF NOT refused THEN
        RAISE EXCEPTION 'FAIL  0031 up: the release before started a student''s own work on a group assignment';
    END IF;
    -- This release's sweep, under its own key, still records Ward A.
    IF NOT EXISTS (SELECT 1 FROM live_group_members('00000000-0000-0000-0031-0000000000c2') x
                   WHERE x = '00000000-0000-0000-0018-000000000053') THEN
        RAISE EXCEPTION 'FAIL  0031 up: Wei is no member of Ward A';
    END IF;
END $chk$;
ROLLBACK;
\echo 'PASS  0031 up makes every submission there was its student''s, takes the index built first, and keeps the release before working'

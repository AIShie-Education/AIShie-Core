-- AIshie Core — before 0032_peer_evaluation.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with, in NUR101 of tests/up/0018 and
-- with Kai of tests/up/0032: Handover, a group assignment of Ward teams,
-- whose one group, Ward A, Wei and Kai, handed its work in; its peer form,
-- a share form at 10 %, and each one's sheet, with comments; and the group
-- graded 18 of 20, peer evaluation counted, Wei's grade 0.9 above the
-- group's and Kai's 0.9 below, posted. Committed, so that the down
-- migration runs over it.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

-- 1c1 Ward teams · 1c2 Ward A · 174 Handover · 1a1 Ward A's work · 1b1 the
-- placing · 1b2 the grade · 1b3 the sheets · 1e1 the group grade · 1d1 Wei's
-- grade · 1d2 Kai's · 1f1, 1f2 Wei's and Kai's sheets
INSERT INTO action (id, actor_id, course_id, member_id, action_type, target_type, target_id, payload, payload_hash,
                    idempotency_key, authz_result, status, executed_at) VALUES
    ('00000000-0000-0000-0032-0000000001b1', '00000000-0000-0000-0018-000000000031', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000051', 'group.set_members', 'group_set', NULL, '{}', repeat('0', 64), 'down32-b1',
     'autonomous', 'executed', now()),
    ('00000000-0000-0000-0032-0000000001b2', '00000000-0000-0000-0018-000000000031', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000051', 'grade.apply_peer', 'assignment', '00000000-0000-0000-0032-000000000174', '{}',
     repeat('0', 64), 'down32-b2', 'autonomous', 'executed', now()),
    ('00000000-0000-0000-0032-0000000001b3', '00000000-0000-0000-0018-000000000033', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000053', 'peer_review.submit', 'assignment', '00000000-0000-0000-0032-000000000174', '{}',
     repeat('0', 64), 'down32-b3', 'autonomous', 'executed', now());
INSERT INTO group_set (id, course_id, name, created_by_member_id)
VALUES ('00000000-0000-0000-0032-0000000001c1', '00000000-0000-0000-0018-000000000041', 'Ward teams',
        '00000000-0000-0000-0018-000000000051');
INSERT INTO course_group (id, course_id, set_id, name, created_by_member_id)
VALUES ('00000000-0000-0000-0032-0000000001c2', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0032-0000000001c1',
        'Ward A', '00000000-0000-0000-0018-000000000051');
INSERT INTO group_membership (course_id, set_id, group_id, member_id, joined_at, joined_by_member_id, joined_how, joined_action_id) VALUES
    ('00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0032-0000000001c1', '00000000-0000-0000-0032-0000000001c2',
     '00000000-0000-0000-0018-000000000053', now(), '00000000-0000-0000-0018-000000000051', 'assigned', '00000000-0000-0000-0032-0000000001b1'),
    ('00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0032-0000000001c1', '00000000-0000-0000-0032-0000000001c2',
     '00000000-0000-0000-0032-000000000057', now(), '00000000-0000-0000-0018-000000000051', 'assigned', '00000000-0000-0000-0032-0000000001b1');
INSERT INTO assignment (id, course_id, title, points_possible, published_at, group_set_id)
VALUES ('00000000-0000-0000-0032-000000000174', '00000000-0000-0000-0018-000000000041', 'Handover', 20, now(),
        '00000000-0000-0000-0032-0000000001c1');
INSERT INTO submission (id, assignment_id, course_id, group_id, body)
VALUES ('00000000-0000-0000-0032-0000000001a1', '00000000-0000-0000-0032-000000000174', '00000000-0000-0000-0018-000000000041',
        '00000000-0000-0000-0032-0000000001c2', 'Our handover');
UPDATE submission SET state = 'submitted', submitted_at = now(), submitted_by_member_id = '00000000-0000-0000-0032-000000000057'
WHERE id = '00000000-0000-0000-0032-0000000001a1';
INSERT INTO submission_member (submission_id, member_id, course_id, assignment_id, added_at, added_how, added_by_member_id) VALUES
    ('00000000-0000-0000-0032-0000000001a1', '00000000-0000-0000-0018-000000000053', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0032-000000000174', now(), 'hand_in', '00000000-0000-0000-0032-000000000057'),
    ('00000000-0000-0000-0032-0000000001a1', '00000000-0000-0000-0032-000000000057', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0032-000000000174', now(), 'hand_in', '00000000-0000-0000-0032-000000000057');
INSERT INTO peer_form (assignment_id, course_id, kind, self_evaluation, opens, closes_at, weight, created_by_member_id,
                       updated_by_member_id)
VALUES ('00000000-0000-0000-0032-000000000174', '00000000-0000-0000-0018-000000000041', 'share', true, 'on_hand_in',
        now() - interval '1 hour', 10, '00000000-0000-0000-0018-000000000051', '00000000-0000-0000-0018-000000000051');
INSERT INTO peer_review (id, course_id, assignment_id, group_id, rater_member_id, comment, created_by_action_id) VALUES
    ('00000000-0000-0000-0032-0000000001f1', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0032-000000000174',
     '00000000-0000-0000-0032-0000000001c2', '00000000-0000-0000-0018-000000000053', 'Kai was often late.',
     '00000000-0000-0000-0032-0000000001b3'),
    ('00000000-0000-0000-0032-0000000001f2', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0032-000000000174',
     '00000000-0000-0000-0032-0000000001c2', '00000000-0000-0000-0032-000000000057', NULL, '00000000-0000-0000-0032-0000000001b3');
INSERT INTO peer_review_entry (review_id, course_id, ratee_member_id, share, comment) VALUES
    ('00000000-0000-0000-0032-0000000001f1', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0018-000000000053', 60, NULL),
    ('00000000-0000-0000-0032-0000000001f1', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0032-000000000057', 40, 'Late'),
    ('00000000-0000-0000-0032-0000000001f2', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0018-000000000053', 50, NULL),
    ('00000000-0000-0000-0032-0000000001f2', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0032-000000000057', 50, NULL);
INSERT INTO group_grade (id, course_id, submission_id, score, out_of, grader_member_id, created_by_action_id)
VALUES ('00000000-0000-0000-0032-0000000001e1', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0032-0000000001a1',
        18, 20, '00000000-0000-0000-0018-000000000051', '00000000-0000-0000-0032-0000000001b2');
INSERT INTO grade (id, student_member_id, submission_id, origin, score, grader_member_id, created_by_action_id, posted_at,
                   posted_by_member_id, group_grade_id, adjust_kind, adjust_points, adjust_detail) VALUES
    ('00000000-0000-0000-0032-0000000001d1', '00000000-0000-0000-0018-000000000053', '00000000-0000-0000-0032-0000000001a1',
     'entered', 18.9, '00000000-0000-0000-0018-000000000051', '00000000-0000-0000-0032-0000000001b2', now(),
     '00000000-0000-0000-0018-000000000051', '00000000-0000-0000-0032-0000000001e1', 'peer', 0.9,
     '{"factor": 1.1, "weight": 10, "raters": 2, "form_version": 1}'),
    ('00000000-0000-0000-0032-0000000001d2', '00000000-0000-0000-0032-000000000057', '00000000-0000-0000-0032-0000000001a1',
     'entered', 17.1, '00000000-0000-0000-0018-000000000051', '00000000-0000-0000-0032-0000000001b2', now(),
     '00000000-0000-0000-0018-000000000051', '00000000-0000-0000-0032-0000000001e1', 'peer', -0.9,
     '{"factor": 0.9, "weight": 10, "raters": 2, "form_version": 1}');

COMMIT;

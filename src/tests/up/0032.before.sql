-- AIshie Core — before 0032_peer_evaluation.up.sql, in `make db-test-sql`
--
-- What the migration finds, in NUR101 of tests/up/0018: Kai, a second
-- student; Ward teams, a set whose one group, Ward A, is Wei's and Kai's;
-- and Ward round, a group assignment of it, whose work Ward A handed in,
-- graded once for the group and posted, Kai's grade two below the group's
-- for a reason Lin gave, as the release before writes them. Committed, so
-- that the migration runs over it; tests/up/0032.after.sql deletes Ward
-- round for good once it has looked, so that no group's work is left for
-- 0031's down. Kai, the set and its group stay.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

-- 33 Kai · 57 his seat · c1 Ward teams · c2 Ward A · 74 Ward round · a1 Ward
-- A's work · b1 the placing · b2 the hand-in · b3 Lin's grade · e1 the group
-- grade · d1 Wei's grade · d2 Kai's
INSERT INTO actor (id, kind, display_name, created_by_actor_id)
VALUES ('00000000-0000-0000-0032-000000000033', 'human', 'Kai', '00000000-0000-0000-0018-000000000031');
INSERT INTO course_member (id, course_id, actor_id, role, status, added_by_actor_id, student_scope, assignment_scope)
VALUES ('00000000-0000-0000-0032-000000000057', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0032-000000000033',
        'student', 'active', '00000000-0000-0000-0018-000000000031', 'listed', 'all');
INSERT INTO member_student_scope (member_id, student_member_id)
VALUES ('00000000-0000-0000-0032-000000000057', '00000000-0000-0000-0032-000000000057');
INSERT INTO action (id, actor_id, course_id, member_id, action_type, target_type, target_id, payload, payload_hash,
                    idempotency_key, authz_result, status, executed_at) VALUES
    ('00000000-0000-0000-0032-0000000000b1', '00000000-0000-0000-0018-000000000031', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000051', 'group.set_members', 'group_set', NULL, '{}', repeat('0', 64), 'up32-b1',
     'autonomous', 'executed', now()),
    ('00000000-0000-0000-0032-0000000000b2', '00000000-0000-0000-0018-000000000033', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000053', 'submission.submit', 'submission', '00000000-0000-0000-0032-0000000000a1', '{}',
     repeat('0', 64), 'up32-b2', 'autonomous', 'executed', now()),
    ('00000000-0000-0000-0032-0000000000b3', '00000000-0000-0000-0018-000000000031', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000051', 'grade.submit', 'submission', '00000000-0000-0000-0032-0000000000a1', '{}',
     repeat('0', 64), 'up32-b3', 'autonomous', 'executed', now());
INSERT INTO group_set (id, course_id, name, created_by_member_id)
VALUES ('00000000-0000-0000-0032-0000000000c1', '00000000-0000-0000-0018-000000000041', 'Ward teams',
        '00000000-0000-0000-0018-000000000051');
INSERT INTO course_group (id, course_id, set_id, name, created_by_member_id)
VALUES ('00000000-0000-0000-0032-0000000000c2', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0032-0000000000c1',
        'Ward A', '00000000-0000-0000-0018-000000000051');
INSERT INTO group_membership (course_id, set_id, group_id, member_id, joined_at, joined_by_member_id, joined_how, joined_action_id) VALUES
    ('00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0032-0000000000c1', '00000000-0000-0000-0032-0000000000c2',
     '00000000-0000-0000-0018-000000000053', now(), '00000000-0000-0000-0018-000000000051', 'assigned', '00000000-0000-0000-0032-0000000000b1'),
    ('00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0032-0000000000c1', '00000000-0000-0000-0032-0000000000c2',
     '00000000-0000-0000-0032-000000000057', now(), '00000000-0000-0000-0018-000000000051', 'assigned', '00000000-0000-0000-0032-0000000000b1');
INSERT INTO assignment (id, course_id, title, points_possible, published_at, group_set_id)
VALUES ('00000000-0000-0000-0032-000000000074', '00000000-0000-0000-0018-000000000041', 'Ward round', 20, now(),
        '00000000-0000-0000-0032-0000000000c1');
INSERT INTO submission (id, assignment_id, course_id, group_id, body, state, submitted_at, submitted_by_member_id)
VALUES ('00000000-0000-0000-0032-0000000000a1', '00000000-0000-0000-0032-000000000074', '00000000-0000-0000-0018-000000000041',
        '00000000-0000-0000-0032-0000000000c2', 'Our round', 'draft', NULL, NULL);
UPDATE submission SET state = 'submitted', submitted_at = now(), submitted_by_member_id = '00000000-0000-0000-0018-000000000053'
WHERE id = '00000000-0000-0000-0032-0000000000a1';
INSERT INTO submission_member (submission_id, member_id, course_id, assignment_id, added_at, added_how, added_by_member_id) VALUES
    ('00000000-0000-0000-0032-0000000000a1', '00000000-0000-0000-0018-000000000053', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0032-000000000074', now(), 'hand_in', '00000000-0000-0000-0018-000000000053'),
    ('00000000-0000-0000-0032-0000000000a1', '00000000-0000-0000-0032-000000000057', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0032-000000000074', now(), 'hand_in', '00000000-0000-0000-0018-000000000053');
INSERT INTO group_grade (id, course_id, submission_id, score, out_of, grader_member_id, created_by_action_id)
VALUES ('00000000-0000-0000-0032-0000000000e1', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0032-0000000000a1',
        18, 20, '00000000-0000-0000-0018-000000000051', '00000000-0000-0000-0032-0000000000b3');
INSERT INTO grade (id, student_member_id, submission_id, origin, score, grader_member_id, created_by_action_id, posted_at,
                   posted_by_member_id, group_grade_id, adjust_kind, adjust_points, adjust_reason, adjust_by_member_id) VALUES
    ('00000000-0000-0000-0032-0000000000d1', '00000000-0000-0000-0018-000000000053', '00000000-0000-0000-0032-0000000000a1',
     'entered', 18, '00000000-0000-0000-0018-000000000051', '00000000-0000-0000-0032-0000000000b3', now(),
     '00000000-0000-0000-0018-000000000051', '00000000-0000-0000-0032-0000000000e1', NULL, NULL, NULL, NULL),
    ('00000000-0000-0000-0032-0000000000d2', '00000000-0000-0000-0032-000000000057', '00000000-0000-0000-0032-0000000000a1',
     'entered', 16, '00000000-0000-0000-0018-000000000051', '00000000-0000-0000-0032-0000000000b3', now(),
     '00000000-0000-0000-0018-000000000051', '00000000-0000-0000-0032-0000000000e1', 'delta', -2, 'Left the round early',
     '00000000-0000-0000-0018-000000000051');

COMMIT;

-- AIshie Core — before 0031_group_work.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with, in NUR101 of tests/up/0018:
-- Clinical groups, a set of two groups Lin formed, Wei placed in Ward A and
-- then moved to Ward B, and Clinical, a group assignment of the set nobody
-- has started on; beside the individual work of tests/up/0031 and its grade.
-- No group's work: the down refuses while there is any. Committed, so that
-- the down migration runs over it.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

-- b1 the placing · s1 Clinical groups · g1 Ward A, g2 Ward B · 73 Clinical
INSERT INTO action (id, actor_id, course_id, member_id, action_type, target_type, payload, payload_hash, idempotency_key,
                    authz_result, status, executed_at)
VALUES ('00000000-0000-0000-0031-0000000001b1', '00000000-0000-0000-0018-000000000031', '00000000-0000-0000-0018-000000000041',
        '00000000-0000-0000-0018-000000000051', 'group.set_members', 'group_set', '{}', repeat('0', 64), 'down31-b1', 'autonomous',
        'executed', now());
INSERT INTO group_set (id, course_id, name, signup_open, created_by_member_id)
VALUES ('00000000-0000-0000-0031-0000000001c1', '00000000-0000-0000-0018-000000000041', 'Clinical groups', true,
        '00000000-0000-0000-0018-000000000051');
INSERT INTO course_group (id, course_id, set_id, name, capacity, created_by_member_id) VALUES
    ('00000000-0000-0000-0031-0000000001c2', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0031-0000000001c1',
     'Ward A', 4, '00000000-0000-0000-0018-000000000051'),
    ('00000000-0000-0000-0031-0000000001c3', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0031-0000000001c1',
     'Ward B', NULL, '00000000-0000-0000-0018-000000000051');
INSERT INTO group_membership (course_id, set_id, group_id, member_id, joined_at, joined_by_member_id, joined_how, joined_action_id,
                              left_at, left_by_member_id, left_how, left_action_id) VALUES
    ('00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0031-0000000001c1', '00000000-0000-0000-0031-0000000001c2',
     '00000000-0000-0000-0018-000000000053', now() - interval '1 day', '00000000-0000-0000-0018-000000000051', 'assigned',
     '00000000-0000-0000-0031-0000000001b1', now(), '00000000-0000-0000-0018-000000000051', 'moved', '00000000-0000-0000-0031-0000000001b1'),
    ('00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0031-0000000001c1', '00000000-0000-0000-0031-0000000001c3',
     '00000000-0000-0000-0018-000000000053', now(), '00000000-0000-0000-0018-000000000051', 'assigned',
     '00000000-0000-0000-0031-0000000001b1', NULL, NULL, NULL, NULL);
INSERT INTO assignment (id, course_id, title, points_possible, published_at, group_set_id)
VALUES ('00000000-0000-0000-0031-000000000173', '00000000-0000-0000-0018-000000000041', 'Clinical', 20, now(),
        '00000000-0000-0000-0031-0000000001c1');

COMMIT;

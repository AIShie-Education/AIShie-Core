-- AIshie Core — before 0031_group_work.up.sql, in `make db-test-sql`
--
-- What the migration finds: in NUR101 of tests/up/0018, Wei's essay on a
-- reflection, handed in and graded by Lin, the grade posted, as the release
-- before writes them, beside Wei's draft of the Care plan (tests/up/0020).
-- And what a large site does first (docs/deploying.md, Migration 0031): it
-- builds the new index of one live posted grade per student on a piece of
-- work, without holding off writes, under the name the migration gives it,
-- so that the migration takes no lock building it. Committed, so that the
-- migration runs over it; it stays, for the redo and the downs after it.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

-- 71 Reflection · a1 Wei's essay · b1 Lin's grade of it · d1 the grade
INSERT INTO assignment (id, course_id, title, points_possible, published_at)
VALUES ('00000000-0000-0000-0031-000000000071', '00000000-0000-0000-0018-000000000041', 'Reflection', 10, now());
INSERT INTO submission (id, assignment_id, course_id, student_member_id, body, state, submitted_at)
VALUES ('00000000-0000-0000-0031-0000000000a1', '00000000-0000-0000-0031-000000000071', '00000000-0000-0000-0018-000000000041',
        '00000000-0000-0000-0018-000000000053', 'What I learned on the ward.', 'submitted', now());
INSERT INTO action (id, actor_id, course_id, member_id, action_type, target_type, target_id, payload, payload_hash,
                    idempotency_key, authz_result, status, executed_at, result)
VALUES ('00000000-0000-0000-0031-0000000000b1', '00000000-0000-0000-0018-000000000031', '00000000-0000-0000-0018-000000000041',
        '00000000-0000-0000-0018-000000000051', 'grade.submit', 'submission', '00000000-0000-0000-0031-0000000000a1',
        '{"score": 9}', repeat('0', 64), 'up31-b1', 'autonomous', 'executed', now(),
        '{"grade_id": "00000000-0000-0000-0031-0000000000d1"}');
INSERT INTO grade (id, student_member_id, submission_id, origin, score, grader_member_id, created_by_action_id, posted_at,
                   posted_by_member_id)
VALUES ('00000000-0000-0000-0031-0000000000d1', '00000000-0000-0000-0018-000000000053', '00000000-0000-0000-0031-0000000000a1',
        'entered', 9, '00000000-0000-0000-0018-000000000051', '00000000-0000-0000-0031-0000000000b1', now(),
        '00000000-0000-0000-0018-000000000051');

COMMIT;

CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS one_live_submission_member_grade ON grade (submission_id, student_member_id)
    WHERE posted_at IS NOT NULL AND superseded_by IS NULL;

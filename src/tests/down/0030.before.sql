-- AIshie Core — before 0030_assignment_deletion.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with, in NUR101 of tests/up/0018: a
-- quiz Wei had started on, with a file, graded, and its news, deleted for
-- good by Lin as assignment.delete deletes it; the grade's action emptied;
-- the file queued to leave the store and not yet gone. Committed, so that
-- the down migration runs over it.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

-- 72 the quiz · a2 Wei's draft · e1 her file, f1 its version, d1 its file
-- b2 Lin's grade of it · b3 Lin's deletion of the quiz · d2 the grade
INSERT INTO assignment (id, course_id, title, points_possible, published_at)
VALUES ('00000000-0000-0000-0030-000000000072', '00000000-0000-0000-0018-000000000041', 'Dosage quiz', 5, now());
INSERT INTO submission (id, assignment_id, course_id, student_member_id, body)
VALUES ('00000000-0000-0000-0030-0000000000a2', '00000000-0000-0000-0030-000000000072', '00000000-0000-0000-0018-000000000041',
        '00000000-0000-0000-0018-000000000053', 'my answers');
INSERT INTO document (id, course_id, kind, title, submission_id)
VALUES ('00000000-0000-0000-0030-0000000000e1', '00000000-0000-0000-0018-000000000041', 'submission', 'answers.pdf',
        '00000000-0000-0000-0030-0000000000a2');
INSERT INTO document_version (id, document_id, seq, author_member_id)
VALUES ('00000000-0000-0000-0030-0000000000f1', '00000000-0000-0000-0030-0000000000e1', 1, '00000000-0000-0000-0018-000000000053');
INSERT INTO document_version_file (id, version_id, document_id, position, filename, storage_key, content_type, byte_size, created_at)
SELECT '00000000-0000-0000-0030-0000000000d1', v.id, v.document_id, 1, 'answers.pdf', 'documents/down30/d1', 'application/pdf', 10,
       v.created_at
FROM document_version v WHERE v.id = '00000000-0000-0000-0030-0000000000f1';
UPDATE document SET published_version_id = '00000000-0000-0000-0030-0000000000f1' WHERE id = '00000000-0000-0000-0030-0000000000e1';
INSERT INTO action (id, actor_id, course_id, member_id, action_type, target_type, target_id, payload, payload_hash,
                    idempotency_key, authz_result, status, executed_at, result) VALUES
    ('00000000-0000-0000-0030-0000000000b2', '00000000-0000-0000-0018-000000000031', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000051', 'grade.submit', 'submission', '00000000-0000-0000-0030-0000000000a2',
     '{"score": 4, "feedback": "Check the units."}', repeat('0', 64), 'down30-b2', 'autonomous', 'executed', now(),
     '{"grade_id": "00000000-0000-0000-0030-0000000000d2"}'),
    ('00000000-0000-0000-0030-0000000000b3', '00000000-0000-0000-0018-000000000031', '00000000-0000-0000-0018-000000000041',
     '00000000-0000-0000-0018-000000000051', 'assignment.delete', 'assignment', '00000000-0000-0000-0030-000000000072',
     '{"assignment_id": "00000000-0000-0000-0030-000000000072"}', repeat('0', 64), 'down30-b3', 'autonomous', 'executed', now(),
     '{"deleted": true, "title": "Dosage quiz"}');
INSERT INTO grade (id, student_member_id, submission_id, origin, score, grader_member_id, created_by_action_id)
VALUES ('00000000-0000-0000-0030-0000000000d2', '00000000-0000-0000-0018-000000000053', '00000000-0000-0000-0030-0000000000a2',
        'entered', 4, '00000000-0000-0000-0018-000000000051', '00000000-0000-0000-0030-0000000000b2');
INSERT INTO event (type, course_id, action_id, subject_type, subject_id, student_member_id, assignment_id)
VALUES ('grade.created', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0030-0000000000b2', 'grade',
        '00000000-0000-0000-0030-0000000000d2', '00000000-0000-0000-0018-000000000053', '00000000-0000-0000-0030-000000000072');

COMMIT;
BEGIN;

-- Deleted for good.
INSERT INTO assignment_deletion (assignment_id, course_id, title, was_published, action_id, deleted_by_actor_id,
                                 deleted_by_member_id, deleted_at, submissions, grades, files, documents, proposals, totals)
VALUES ('00000000-0000-0000-0030-000000000072', '00000000-0000-0000-0018-000000000041', 'Dosage quiz', true,
        '00000000-0000-0000-0030-0000000000b3', '00000000-0000-0000-0018-000000000031', '00000000-0000-0000-0018-000000000051',
        now(), 1, 1, 1, 0, 0, 0);
INSERT INTO blob_deletion (storage_key, course_id, queued_by_action_id, queued_at, next_try_at)
VALUES ('documents/down30/d1', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0030-0000000000b3', now(), now());
UPDATE action SET payload = '{}', result = NULL,
                  payload_hash = encode(sha256(convert_to(action_type || E'\n{}', 'UTF8')), 'hex'),
                  redacted_by_action_id = '00000000-0000-0000-0030-0000000000b3'
WHERE id = '00000000-0000-0000-0030-0000000000b2';
UPDATE document_version SET purged_at = now(), purged_by_actor_id = '00000000-0000-0000-0018-000000000031',
                            purge_reason = 'assignment_deleted'
WHERE id = '00000000-0000-0000-0030-0000000000f1';
UPDATE document SET published_version_id = NULL WHERE id = '00000000-0000-0000-0030-0000000000e1';
DELETE FROM document_version WHERE id = '00000000-0000-0000-0030-0000000000f1';
DELETE FROM document WHERE id = '00000000-0000-0000-0030-0000000000e1';
DELETE FROM grade WHERE id = '00000000-0000-0000-0030-0000000000d2';
DELETE FROM submission WHERE id = '00000000-0000-0000-0030-0000000000a2';
DELETE FROM event WHERE assignment_id = '00000000-0000-0000-0030-000000000072';
DELETE FROM assignment WHERE id = '00000000-0000-0000-0030-000000000072';

COMMIT;

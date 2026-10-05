-- Deleting an assignment for good (docs/schema.md §2.5, An assignment is
-- deleted for good): what goes with it, read by assignment.delete_preview as
-- the course stands and by assignment.delete under the locks it takes, and
-- the statements that take it. The guards of migration 0030 let these
-- deletes through only in the transaction that has written the
-- assignment's row of assignment_deletion. The only documents they reach are
-- the submitted files of its submissions and the feedback files of the
-- grades given on them: its instructions and rubric, and the course's
-- material, are left as they are.

-- name: LockAssignmentForDelete :one
-- The assignment, held FOR UPDATE for the whole of its deletion: it holds off
-- the KEY SHARE that a new submission's foreign key takes, and an event's or
-- a scope row's, and ShareAssignments; a grader's FOR SHARE
-- (ShareAssignmentForGrading); and an update's or a publish's NO KEY UPDATE.
-- It is the first lock the deletion takes after its caller's seat.
SELECT id, course_id, component_id, title, instructions_document_id, rubric_document_id,
       points_possible, due_at, published_at, group_set_id
FROM assignment
WHERE id = $1 AND course_id = $2
FOR UPDATE;

-- name: LockSubmissionsOfAssignment :many
-- Every submission to the assignment, every attempt and state, held in id
-- order: nothing is added to one (a file, a grade) while it is deleted.
SELECT id FROM submission WHERE assignment_id = $1 ORDER BY id FOR UPDATE;

-- name: LockGradesOfAssignment :many
-- Every grade given on a submission to the assignment, superseded ones
-- included, held in id order, after the submissions.
SELECT g.id
FROM grade g
JOIN submission s ON s.id = g.submission_id
WHERE s.assignment_id = $1
ORDER BY g.id
FOR UPDATE OF g;

-- name: LockGroupGradesOfAssignment :many
-- Every group grade given on a group's submission to the assignment, held in
-- id order, after the grades.
SELECT gg.id
FROM group_grade gg
JOIN submission s ON s.id = gg.submission_id
WHERE s.assignment_id = $1
ORDER BY gg.id
FOR UPDATE OF gg;

-- name: GetAssignmentDeletion :one
-- The record of an assignment of the course deleted for good, if it was.
SELECT * FROM assignment_deletion WHERE assignment_id = $1 AND course_id = $2;

-- name: CountAssignmentWork :one
-- An assignment's submissions, every attempt and state, by state; and the
-- grades given on them that are live, drafts and posted: a superseded grade
-- is the history of the one that replaced it, and is not counted.
SELECT (SELECT count(*) FROM submission s WHERE s.assignment_id = sqlc.arg(assignment_id))::int AS submissions,
       (SELECT count(*) FROM submission s
        WHERE s.assignment_id = sqlc.arg(assignment_id) AND s.state IN ('submitted', 'late'))::int AS handed_in,
       (SELECT count(*) FROM submission s WHERE s.assignment_id = sqlc.arg(assignment_id) AND s.state = 'draft')::int AS drafts,
       (SELECT count(*) FROM submission s WHERE s.assignment_id = sqlc.arg(assignment_id) AND s.state = 'missing')::int AS missing,
       (SELECT count(*) FROM grade g JOIN submission s ON s.id = g.submission_id
        WHERE s.assignment_id = sqlc.arg(assignment_id) AND g.superseded_by IS NULL)::int AS grades,
       (SELECT count(*) FROM grade g JOIN submission s ON s.id = g.submission_id
        WHERE s.assignment_id = sqlc.arg(assignment_id) AND g.superseded_by IS NULL AND g.posted_at IS NOT NULL)::int AS posted;

-- name: ListStudentsWithSubmissionsTo :many
-- The students with a submission row of any kind for the assignment: whose
-- work its deletion takes. A group's work is its students'
-- (submission_students): every member of work handed in or recorded
-- missing, and every member now of a group with a draft.
SELECT DISTINCT st.member_id::uuid AS member_id
FROM submission s, submission_students(s.id) AS st(member_id)
WHERE s.assignment_id = $1
ORDER BY 1;

-- name: ListOwnedDocumentsOfAssignment :many
-- The submitted files of every submission to the assignment, and the
-- feedback files of every grade and group grade given on them, superseded
-- ones included, in id order: deleted with it, and the only documents that
-- are.
SELECT d.id
FROM document d
WHERE (d.kind = 'submission'
       AND d.submission_id IN (SELECT s.id FROM submission s WHERE s.assignment_id = sqlc.arg(assignment_id)))
   OR (d.kind = 'feedback'
       AND d.grade_id IN (SELECT g.id FROM grade g JOIN submission s ON s.id = g.submission_id
                          WHERE s.assignment_id = sqlc.arg(assignment_id)))
   OR (d.kind = 'feedback'
       AND d.group_grade_id IN (SELECT gg.id FROM group_grade gg JOIN submission s ON s.id = gg.submission_id
                                WHERE s.assignment_id = sqlc.arg(assignment_id)))
ORDER BY d.id;

-- name: ListFileKeysOfDocuments :many
-- The keys of the files of every version of the documents, in order.
SELECT DISTINCT f.storage_key FROM document_version_file f
WHERE f.document_id = ANY(sqlc.arg(document_ids)::uuid[])
ORDER BY f.storage_key;

-- name: RenditionKeysOfDocuments :many
-- The PDFs of the files of every version of the documents.
SELECT r.storage_key::text AS storage_key
FROM file_rendition r
JOIN document_version_file f ON f.id = r.file_id
WHERE f.document_id = ANY(sqlc.arg(document_ids)::uuid[]) AND r.storage_key IS NOT NULL
ORDER BY r.storage_key;

-- name: ListActionsAboutAssignment :many
-- The course's actions about the assignment, other than its deletions
-- (assignment.delete), that no deletion has emptied yet: those whose target
-- is the assignment, one of its submissions or of the grades given on them,
-- or one of the submitted and feedback files deleted with it (document_ids,
-- never its instructions or rubric, which stay, with what was done to
-- them); those whose arguments name one of them as assignment_id,
-- submission_id, grade_id or document_id, or whose result does as id,
-- submission_id, grade_id or document_id (what a call made:
-- assignment.create's id, submission.create's submission_id); and, at any
-- remove, the decisions, reviews, withdrawals and expiries of those
-- (target_type action).
WITH RECURSIVE about (id) AS (
    SELECT sqlc.arg(assignment_id)::uuid
  UNION
    SELECT s.id FROM submission s WHERE s.assignment_id = sqlc.arg(assignment_id)::uuid
  UNION
    SELECT g.id FROM grade g JOIN submission s ON s.id = g.submission_id WHERE s.assignment_id = sqlc.arg(assignment_id)::uuid
  UNION
    SELECT gg.id FROM group_grade gg JOIN submission s ON s.id = gg.submission_id WHERE s.assignment_id = sqlc.arg(assignment_id)::uuid
  UNION
    SELECT unnest(sqlc.arg(document_ids)::uuid[])
), named (id) AS (
    SELECT id::text FROM about
), r (id) AS (
    SELECT a.id
    FROM action a
    WHERE a.course_id = sqlc.arg(course_id)::uuid AND a.action_type <> 'assignment.delete'
      AND (a.target_id IN (SELECT id FROM about)
           OR a.payload->>'assignment_id' IN (SELECT id FROM named)
           OR a.payload->>'submission_id' IN (SELECT id FROM named)
           OR a.payload->>'grade_id' IN (SELECT id FROM named)
           OR a.payload->>'document_id' IN (SELECT id FROM named)
           OR a.payload->>'group_grade_id' IN (SELECT id FROM named)
           OR (jsonb_typeof(a.result) = 'object'
               AND (a.result->>'id' IN (SELECT id FROM named)
                    OR a.result->>'submission_id' IN (SELECT id FROM named)
                    OR a.result->>'grade_id' IN (SELECT id FROM named)
                    OR a.result->>'document_id' IN (SELECT id FROM named)
                    OR a.result->>'group_grade_id' IN (SELECT id FROM named))))
  UNION
    SELECT d.id
    FROM action d
    JOIN r ON d.target_id = r.id
    WHERE d.target_type = 'action' AND d.course_id = sqlc.arg(course_id)::uuid
)
SELECT a.id, a.course_id, a.action_type, a.status
FROM action a
WHERE a.id IN (SELECT id FROM r) AND a.redacted_by_action_id IS NULL
ORDER BY a.id;

-- name: InsertAssignmentDeletion :exec
-- The record of the deletion, written before anything is deleted: it opens
-- the guarded path (assignment_being_deleted) for this assignment, in this
-- transaction.
INSERT INTO assignment_deletion (assignment_id, course_id, title, was_published, action_id, deleted_by_actor_id,
                                 deleted_by_member_id, deleted_at, submissions, grades, files, proposals, totals)
VALUES (sqlc.arg(assignment_id), sqlc.arg(course_id), sqlc.arg(title), sqlc.arg(was_published), sqlc.arg(action_id),
        sqlc.arg(deleted_by_actor_id), sqlc.arg(deleted_by_member_id), sqlc.arg(deleted_at), sqlc.arg(submissions),
        sqlc.arg(grades), sqlc.arg(files), sqlc.arg(proposals), sqlc.arg(totals));

-- name: QueueBlobDeletions :execrows
-- Files to delete from the store once this transaction has committed, due
-- at once. A key queued already stays as it is.
INSERT INTO blob_deletion (storage_key, course_id, queued_by_action_id, queued_at, next_try_at)
SELECT k, sqlc.arg(course_id)::uuid, sqlc.arg(action_id)::uuid, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz
FROM unnest(sqlc.arg(storage_keys)::text[]) AS k
ON CONFLICT (storage_key) DO NOTHING;

-- name: RedactActions :execrows
-- Empties actions about an assignment deleted for good, naming the deletion:
-- who did what, when, on what and with what outcome stay; what they were
-- given and returned go, and their hash is an empty call's.
UPDATE action
SET payload = '{}'::jsonb, result = NULL,
    payload_hash = encode(sha256(convert_to(action_type || E'\n{}', 'UTF8')), 'hex'),
    redacted_by_action_id = sqlc.arg(by_action_id)::uuid
WHERE id = ANY(sqlc.arg(ids)::uuid[]) AND redacted_by_action_id IS NULL;

-- name: ClearPublishedVersions :exec
-- Of the submitted and feedback files deleted with it.
UPDATE document SET published_version_id = NULL WHERE id = ANY(sqlc.arg(ids)::uuid[]) AND published_version_id IS NOT NULL;

-- name: DeleteDocumentVersions :execrows
-- Of the submitted and feedback files deleted with it, purged first, so
-- that their files and texts are gone (document_version_guarded).
DELETE FROM document_version WHERE document_id = ANY(sqlc.arg(document_ids)::uuid[]);

-- name: DeleteDocuments :execrows
-- The submitted and feedback files deleted with it (document_kept).
DELETE FROM document WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- name: DeleteGradesOfAssignment :execrows
-- Every grade given on the assignment's submissions, whole chains of
-- superseded grades together: the key from a grade to the one that replaced
-- it is checked at commit.
DELETE FROM grade WHERE submission_id IN (SELECT s.id FROM submission s WHERE s.assignment_id = $1);

-- name: DeleteGroupGradesOfAssignment :execrows
-- Every group grade given on a group's submission to it, once the grades
-- given from them have gone (group_grade_kept).
DELETE FROM group_grade WHERE submission_id IN (SELECT s.id FROM submission s WHERE s.assignment_id = $1);

-- name: DeleteSubmissionsOfAssignment :execrows
-- Whose work each was (submission_member) goes with it, by cascade.
DELETE FROM submission WHERE assignment_id = $1;

-- name: DeleteAssignmentScopes :execrows
-- A seat listed for the assignment alone now reaches no assignment: it
-- fails closed.
DELETE FROM member_assignment_scope WHERE assignment_id = $1;

-- name: DeleteEventsOfAssignment :execrows
DELETE FROM event WHERE course_id = $1 AND assignment_id = $2;

-- name: DeleteAssignment :execrows
DELETE FROM assignment WHERE id = $1 AND course_id = $2;

-- name: RedactTotalsLine :execrows
-- The line of a deleted assignment in the working of the course's totals
-- that still name it, superseded ones: it says the assignment was deleted,
-- and nothing of the grade it was. The total's number is kept: it is what
-- the student was shown.
UPDATE grade g
SET breakdown = jsonb_set(g.breakdown, '{items}', (
        SELECT jsonb_agg(CASE WHEN x.item->>'id' = sqlc.arg(assignment_id)::uuid::text
                              THEN jsonb_build_object('id', x.item->'id', 'kind', 'assignment', 'deleted', true)
                              ELSE x.item END ORDER BY x.n)
        FROM jsonb_array_elements(g.breakdown->'items') WITH ORDINALITY AS x(item, n)))
FROM grade_component c
WHERE c.id = g.component_id AND c.course_id = sqlc.arg(course_id)::uuid AND g.origin = 'computed'
  AND jsonb_typeof(g.breakdown->'items') = 'array'
  AND g.breakdown->'items' @> jsonb_build_array(jsonb_build_object('id', sqlc.arg(assignment_id)::uuid::text));

-- name: ListActionsRedactedAt :many
-- When each of these actions, the deletions that emptied others, ran.
SELECT id, executed_at FROM action WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- ---------------------------------------------------------------------------
-- The queue of files to delete from the store (the job runner)
-- ---------------------------------------------------------------------------

-- name: ListDueBlobDeletions :many
SELECT storage_key, attempts
FROM blob_deletion
WHERE next_try_at <= sqlc.arg(now)
ORDER BY next_try_at, storage_key
LIMIT sqlc.arg(max_rows);

-- name: DeleteBlobDeletion :exec
DELETE FROM blob_deletion WHERE storage_key = $1;

-- name: PostponeBlobDeletion :exec
UPDATE blob_deletion
SET attempts = sqlc.arg(attempts), next_try_at = sqlc.arg(next_try_at), last_error = sqlc.arg(last_error)
WHERE storage_key = sqlc.arg(storage_key);

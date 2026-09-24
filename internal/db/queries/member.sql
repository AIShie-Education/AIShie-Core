-- name: InsertMember :exec
INSERT INTO course_member (
    id, course_id, actor_id, role, status, preset_id, added_by_actor_id, expires_at, student_scope, assignment_scope,
    perm_document_read, perm_document_read_draft, perm_document_write, perm_rubric_read,
    perm_assignment_write, perm_submission_read, perm_submission_write, perm_grade_read,
    perm_grade_submit, perm_grade_post, perm_member_read, perm_member_manage, perm_action_decide, created_at)
VALUES ($1, $2, $3, $4, 'active', $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23);

-- name: GetMemberInCourse :one
SELECT m.*, a.display_name, a.kind AS actor_kind
FROM course_member m
JOIN actor a ON a.id = m.actor_id
WHERE m.id = $1 AND m.course_id = $2;

-- name: GetMemberInCourseForUpdate :one
-- The same row, locked for the rest of the transaction: the management tools
-- read a seat and write it back, and two of them at once must take turns.
SELECT m.*, a.display_name, a.kind AS actor_kind
FROM course_member m
JOIN actor a ON a.id = m.actor_id
WHERE m.id = $1 AND m.course_id = $2
FOR UPDATE OF m;

-- name: GetLiveMembership :one
-- Not locked: a live seat found here is only refused, and locking it would
-- wait for its member's calls in flight, and could deadlock with them, to say
-- no. A seat past its expiry is locked by id before it is removed.
SELECT id, status, expires_at FROM course_member WHERE course_id = $1 AND actor_id = $2 AND status <> 'removed';

-- name: ListMembers :many
SELECT m.*, a.display_name, a.kind AS actor_kind
FROM course_member m
JOIN actor a ON a.id = m.actor_id
WHERE m.course_id = $1 AND m.id > sqlc.arg(after)
  AND (sqlc.narg(role)::text IS NULL OR m.role = sqlc.narg(role))
  AND (sqlc.arg(include_removed)::bool OR m.status <> 'removed')
ORDER BY m.id
LIMIT sqlc.arg(max_rows);

-- name: ListStudentScope :many
SELECT student_member_id FROM member_student_scope WHERE member_id = $1 ORDER BY student_member_id;

-- name: ListAssignmentScope :many
SELECT assignment_id FROM member_assignment_scope WHERE member_id = $1 ORDER BY assignment_id;

-- name: SetMemberStatus :execrows
UPDATE course_member SET status = $2 WHERE id = $1 AND status = sqlc.arg(from_status);

-- name: SetMemberPerms :exec
UPDATE course_member SET
    perm_document_read = $2, perm_document_read_draft = $3, perm_document_write = $4, perm_rubric_read = $5,
    perm_assignment_write = $6, perm_submission_read = $7, perm_submission_write = $8, perm_grade_read = $9,
    perm_grade_submit = $10, perm_grade_post = $11, perm_member_read = $12, perm_member_manage = $13,
    perm_action_decide = $14
WHERE id = $1;

-- name: SetMemberScopeKinds :exec
UPDATE course_member SET student_scope = $2, assignment_scope = $3 WHERE id = $1;

-- name: SetMemberExpiry :exec
UPDATE course_member SET expires_at = $2 WHERE id = $1;

-- name: ClearStudentScope :exec
DELETE FROM member_student_scope WHERE member_id = $1;

-- name: ClearAssignmentScope :exec
DELETE FROM member_assignment_scope WHERE member_id = $1;

-- name: AddStudentScope :exec
INSERT INTO member_student_scope (member_id, student_member_id) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: AddAssignmentScope :exec
INSERT INTO member_assignment_scope (member_id, assignment_id) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: CountStudentsOfCourse :one
-- How many of the given member ids are current students of this course.
SELECT count(*) FROM course_member
WHERE course_id = $1 AND role = 'student' AND status <> 'removed' AND id = ANY(sqlc.arg(member_ids)::uuid[]);

-- name: CountAssignmentsOfCourse :one
SELECT count(*) FROM assignment WHERE course_id = $1 AND id = ANY(sqlc.arg(assignment_ids)::uuid[]);

-- name: ListProposedActionIDsByMember :many
SELECT id FROM action WHERE member_id = $1 AND status = 'proposed' ORDER BY id FOR UPDATE;

-- name: CancelProposal :execrows
UPDATE action SET status = 'cancelled', result = $2 WHERE id = $1 AND status = 'proposed';

-- name: ShareSeats :exec
-- KEY SHARE on the given seats, in id order: what taking them before some
-- other lock looks like, where that lock would otherwise be held while one of
-- them is waited for.
SELECT 1 FROM course_member WHERE id = ANY(sqlc.arg(ids)::uuid[]) ORDER BY id FOR KEY SHARE;

-- Everything authorize() reads. Two columns are deliberately never selected
-- here: the actor's type and the member's roster role. Authorization does not
-- branch on either, and a test fails if this file ever names them.

-- name: GetActorForAuthz :one
SELECT id, display_name, status, platform_role
FROM actor
WHERE id = $1;

-- name: GetCourseForAuthz :one
SELECT id, status
FROM course
WHERE id = $1;

-- name: GetLiveMemberForAuthz :one
-- The partial unique index allows at most one row per (course, actor) that is
-- not removed. A paused row is returned so the caller can say why it denied.
SELECT id, course_id, actor_id, status, expires_at, student_scope, assignment_scope,
       perm_document_read, perm_document_read_draft, perm_document_write, perm_rubric_read,
       perm_assignment_write, perm_submission_read, perm_submission_write, perm_grade_read,
       perm_grade_submit, perm_grade_post, perm_member_read, perm_member_manage, perm_action_decide
FROM course_member
WHERE course_id = $1 AND actor_id = $2 AND status <> 'removed';

-- name: LockLiveMemberForAuthz :one
-- The same, for a call that writes, and the first row that call locks: the
-- caller's own seat, KEY SHARE, to the end of the call. It blocks only what
-- locks the seat FOR UPDATE — a change to it, its removal, the expiry sweep —
-- which then waits for the call, or the call waits for it and sees what it
-- did. Taking the seat before anything else keeps one order for every write,
-- the seat first: the order the action row's foreign key to it always had.
SELECT id, course_id, actor_id, status, expires_at, student_scope, assignment_scope,
       perm_document_read, perm_document_read_draft, perm_document_write, perm_rubric_read,
       perm_assignment_write, perm_submission_read, perm_submission_write, perm_grade_read,
       perm_grade_submit, perm_grade_post, perm_member_read, perm_member_manage, perm_action_decide
FROM course_member
WHERE course_id = $1 AND actor_id = $2 AND status <> 'removed'
FOR KEY SHARE;

-- name: GetMemberForAuthz :one
-- By id, removed rows included: re-authorizing a proposal checks the very
-- membership it was made under, not whatever row the actor holds today.
SELECT id, course_id, actor_id, status, expires_at, student_scope, assignment_scope,
       perm_document_read, perm_document_read_draft, perm_document_write, perm_rubric_read,
       perm_assignment_write, perm_submission_read, perm_submission_write, perm_grade_read,
       perm_grade_submit, perm_grade_post, perm_member_read, perm_member_manage, perm_action_decide
FROM course_member
WHERE id = $1;

-- name: CountStudentsInScope :one
SELECT count(*)
FROM member_student_scope
WHERE member_id = $1 AND student_member_id = ANY(sqlc.arg(student_member_ids)::uuid[]);

-- name: CountAssignmentsInScope :one
SELECT count(*)
FROM member_assignment_scope
WHERE member_id = $1 AND assignment_id = ANY(sqlc.arg(assignment_ids)::uuid[]);

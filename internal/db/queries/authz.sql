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

-- A seat comes with its principal, when it is a delegate's: the principal's
-- standing, scope and levels cap the delegate's (domain.Member). The three
-- queries below select the same columns, in the same order. owner_matches
-- says the seat is what its actor's ownership says it must be: no principal
-- for an actor nobody owns, the owner's seat for one somebody does. An owner
-- can change after a seat was taken, and the seat then stops counting.

-- name: GetLiveMemberForAuthz :one
-- The partial unique index allows at most one row per (course, actor) that is
-- not removed. A paused row is returned so the caller can say why it denied.
SELECT m.id, m.course_id, m.actor_id, m.status, m.expires_at, m.student_scope, m.assignment_scope,
       m.perm_document_read, m.perm_document_read_draft, m.perm_document_write, m.perm_rubric_read,
       m.perm_assignment_write, m.perm_submission_read, m.perm_submission_write, m.perm_grade_read,
       m.perm_grade_submit, m.perm_grade_post, m.perm_member_read, m.perm_member_manage,
       m.perm_action_decide, m.perm_agent_delegate, m.perm_conversation_ask, m.perm_conversation_answer,
       m.principal_member_id, m.answers_course,
       (CASE WHEN m.principal_member_id IS NULL THEN a.owner_actor_id IS NULL
             ELSE a.owner_actor_id IS NOT DISTINCT FROM p.actor_id END)::bool AS owner_matches,
       p.actor_id AS principal_actor_id, p.status AS principal_status, p.expires_at AS principal_expires_at,
       p.student_scope AS principal_student_scope, p.assignment_scope AS principal_assignment_scope,
       p.perm_document_read AS principal_perm_document_read, p.perm_document_read_draft AS principal_perm_document_read_draft, p.perm_document_write AS principal_perm_document_write,
       p.perm_rubric_read AS principal_perm_rubric_read, p.perm_assignment_write AS principal_perm_assignment_write, p.perm_submission_read AS principal_perm_submission_read,
       p.perm_submission_write AS principal_perm_submission_write, p.perm_grade_read AS principal_perm_grade_read, p.perm_grade_submit AS principal_perm_grade_submit,
       p.perm_grade_post AS principal_perm_grade_post, p.perm_member_read AS principal_perm_member_read, p.perm_member_manage AS principal_perm_member_manage,
       p.perm_action_decide AS principal_perm_action_decide, p.perm_agent_delegate AS principal_perm_agent_delegate, p.perm_conversation_ask AS principal_perm_conversation_ask,
       p.perm_conversation_answer AS principal_perm_conversation_answer,
       pa.status AS principal_actor_status
FROM course_member m
JOIN actor a ON a.id = m.actor_id
LEFT JOIN course_member p ON p.id = m.principal_member_id
LEFT JOIN actor pa ON pa.id = p.actor_id
WHERE m.course_id = $1 AND m.actor_id = $2 AND m.status <> 'removed';

-- name: LockLiveMemberForAuthz :one
-- The same, for a call that writes, and the first row that call locks: the
-- caller's own seat, KEY SHARE, to the end of the call. It blocks only what
-- locks the seat FOR UPDATE — a change to it, its removal, the expiry sweep —
-- which then waits for the call, or the call waits for it and sees what it
-- did. Taking the seat before anything else keeps one order for every write,
-- the seat first: the order the action row's foreign key to it always had.
--
-- Only the caller's own seat is locked here. A delegate's principal is
-- locked next, by LockPrincipalForAuthz, and read again as it then stands:
-- a delegate's seat, then its principal's, is the order everything that
-- takes both takes them in (pipeline.Decide included).
SELECT m.id, m.course_id, m.actor_id, m.status, m.expires_at, m.student_scope, m.assignment_scope,
       m.perm_document_read, m.perm_document_read_draft, m.perm_document_write, m.perm_rubric_read,
       m.perm_assignment_write, m.perm_submission_read, m.perm_submission_write, m.perm_grade_read,
       m.perm_grade_submit, m.perm_grade_post, m.perm_member_read, m.perm_member_manage,
       m.perm_action_decide, m.perm_agent_delegate, m.perm_conversation_ask, m.perm_conversation_answer,
       m.principal_member_id, m.answers_course,
       (CASE WHEN m.principal_member_id IS NULL THEN a.owner_actor_id IS NULL
             ELSE a.owner_actor_id IS NOT DISTINCT FROM p.actor_id END)::bool AS owner_matches,
       p.actor_id AS principal_actor_id, p.status AS principal_status, p.expires_at AS principal_expires_at,
       p.student_scope AS principal_student_scope, p.assignment_scope AS principal_assignment_scope,
       p.perm_document_read AS principal_perm_document_read, p.perm_document_read_draft AS principal_perm_document_read_draft, p.perm_document_write AS principal_perm_document_write,
       p.perm_rubric_read AS principal_perm_rubric_read, p.perm_assignment_write AS principal_perm_assignment_write, p.perm_submission_read AS principal_perm_submission_read,
       p.perm_submission_write AS principal_perm_submission_write, p.perm_grade_read AS principal_perm_grade_read, p.perm_grade_submit AS principal_perm_grade_submit,
       p.perm_grade_post AS principal_perm_grade_post, p.perm_member_read AS principal_perm_member_read, p.perm_member_manage AS principal_perm_member_manage,
       p.perm_action_decide AS principal_perm_action_decide, p.perm_agent_delegate AS principal_perm_agent_delegate, p.perm_conversation_ask AS principal_perm_conversation_ask,
       p.perm_conversation_answer AS principal_perm_conversation_answer,
       pa.status AS principal_actor_status
FROM course_member m
JOIN actor a ON a.id = m.actor_id
LEFT JOIN course_member p ON p.id = m.principal_member_id
LEFT JOIN actor pa ON pa.id = p.actor_id
WHERE m.course_id = $1 AND m.actor_id = $2 AND m.status <> 'removed'
FOR KEY SHARE OF m;

-- name: GetMemberForAuthz :one
-- By id, removed rows included: re-authorizing a proposal checks the very
-- membership it was made under, not whatever row the actor holds today.
SELECT m.id, m.course_id, m.actor_id, m.status, m.expires_at, m.student_scope, m.assignment_scope,
       m.perm_document_read, m.perm_document_read_draft, m.perm_document_write, m.perm_rubric_read,
       m.perm_assignment_write, m.perm_submission_read, m.perm_submission_write, m.perm_grade_read,
       m.perm_grade_submit, m.perm_grade_post, m.perm_member_read, m.perm_member_manage,
       m.perm_action_decide, m.perm_agent_delegate, m.perm_conversation_ask, m.perm_conversation_answer,
       m.principal_member_id, m.answers_course,
       (CASE WHEN m.principal_member_id IS NULL THEN a.owner_actor_id IS NULL
             ELSE a.owner_actor_id IS NOT DISTINCT FROM p.actor_id END)::bool AS owner_matches,
       p.actor_id AS principal_actor_id, p.status AS principal_status, p.expires_at AS principal_expires_at,
       p.student_scope AS principal_student_scope, p.assignment_scope AS principal_assignment_scope,
       p.perm_document_read AS principal_perm_document_read, p.perm_document_read_draft AS principal_perm_document_read_draft, p.perm_document_write AS principal_perm_document_write,
       p.perm_rubric_read AS principal_perm_rubric_read, p.perm_assignment_write AS principal_perm_assignment_write, p.perm_submission_read AS principal_perm_submission_read,
       p.perm_submission_write AS principal_perm_submission_write, p.perm_grade_read AS principal_perm_grade_read, p.perm_grade_submit AS principal_perm_grade_submit,
       p.perm_grade_post AS principal_perm_grade_post, p.perm_member_read AS principal_perm_member_read, p.perm_member_manage AS principal_perm_member_manage,
       p.perm_action_decide AS principal_perm_action_decide, p.perm_agent_delegate AS principal_perm_agent_delegate, p.perm_conversation_ask AS principal_perm_conversation_ask,
       p.perm_conversation_answer AS principal_perm_conversation_answer,
       pa.status AS principal_actor_status
FROM course_member m
JOIN actor a ON a.id = m.actor_id
LEFT JOIN course_member p ON p.id = m.principal_member_id
LEFT JOIN actor pa ON pa.id = p.actor_id
WHERE m.id = $1;

-- name: GetMembersForAuthz :many
-- The same for several seats at once, by id: what a list shows of what each
-- of its seats may do (a conversation's respondent), for a list's worth of
-- seats in one statement. Locks nothing.
SELECT m.id, m.course_id, m.actor_id, m.status, m.expires_at, m.student_scope, m.assignment_scope,
       m.perm_document_read, m.perm_document_read_draft, m.perm_document_write, m.perm_rubric_read,
       m.perm_assignment_write, m.perm_submission_read, m.perm_submission_write, m.perm_grade_read,
       m.perm_grade_submit, m.perm_grade_post, m.perm_member_read, m.perm_member_manage,
       m.perm_action_decide, m.perm_agent_delegate, m.perm_conversation_ask, m.perm_conversation_answer,
       m.principal_member_id, m.answers_course,
       (CASE WHEN m.principal_member_id IS NULL THEN a.owner_actor_id IS NULL
             ELSE a.owner_actor_id IS NOT DISTINCT FROM p.actor_id END)::bool AS owner_matches,
       p.actor_id AS principal_actor_id, p.status AS principal_status, p.expires_at AS principal_expires_at,
       p.student_scope AS principal_student_scope, p.assignment_scope AS principal_assignment_scope,
       p.perm_document_read AS principal_perm_document_read, p.perm_document_read_draft AS principal_perm_document_read_draft, p.perm_document_write AS principal_perm_document_write,
       p.perm_rubric_read AS principal_perm_rubric_read, p.perm_assignment_write AS principal_perm_assignment_write, p.perm_submission_read AS principal_perm_submission_read,
       p.perm_submission_write AS principal_perm_submission_write, p.perm_grade_read AS principal_perm_grade_read, p.perm_grade_submit AS principal_perm_grade_submit,
       p.perm_grade_post AS principal_perm_grade_post, p.perm_member_read AS principal_perm_member_read, p.perm_member_manage AS principal_perm_member_manage,
       p.perm_action_decide AS principal_perm_action_decide, p.perm_agent_delegate AS principal_perm_agent_delegate, p.perm_conversation_ask AS principal_perm_conversation_ask,
       p.perm_conversation_answer AS principal_perm_conversation_answer,
       pa.status AS principal_actor_status
FROM course_member m
JOIN actor a ON a.id = m.actor_id
LEFT JOIN course_member p ON p.id = m.principal_member_id
LEFT JOIN actor pa ON pa.id = p.actor_id
WHERE m.id = ANY(sqlc.arg(ids)::uuid[]);

-- name: LockPrincipalForAuthz :one
-- A delegate's principal, KEY SHARE, to the end of a call the delegate
-- writes: removing, pausing or narrowing the principal waits for the call,
-- or the call waits for it and, reading the row again here, sees what it
-- did.
SELECT p.id, p.actor_id, p.status, p.expires_at, p.student_scope, p.assignment_scope,
       p.perm_document_read, p.perm_document_read_draft, p.perm_document_write, p.perm_rubric_read,
       p.perm_assignment_write, p.perm_submission_read, p.perm_submission_write, p.perm_grade_read,
       p.perm_grade_submit, p.perm_grade_post, p.perm_member_read, p.perm_member_manage,
       p.perm_action_decide, p.perm_agent_delegate, p.perm_conversation_ask, p.perm_conversation_answer,
       pa.status AS actor_status
FROM course_member p
JOIN actor pa ON pa.id = p.actor_id
WHERE p.id = $1
FOR KEY SHARE OF p;

-- name: CountStudentsInScope :one
SELECT count(*)
FROM member_student_scope
WHERE member_id = $1 AND student_member_id = ANY(sqlc.arg(student_member_ids)::uuid[]);

-- name: CountAssignmentsInScope :one
SELECT count(*)
FROM member_assignment_scope
WHERE member_id = $1 AND assignment_id = ANY(sqlc.arg(assignment_ids)::uuid[]);

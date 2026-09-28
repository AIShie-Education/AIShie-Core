-- name: InsertMember :exec
INSERT INTO course_member (
    id, course_id, actor_id, role, status, preset_id, added_by_actor_id, expires_at, student_scope, assignment_scope,
    perm_document_read, perm_document_read_draft, perm_document_write, perm_rubric_read,
    perm_assignment_write, perm_submission_read, perm_submission_write, perm_grade_read,
    perm_grade_submit, perm_grade_post, perm_member_read, perm_member_manage, perm_action_decide,
    perm_agent_delegate, perm_conversation_ask, perm_conversation_answer, perm_member_invite,
    created_at, principal_member_id, answers_course)
VALUES ($1, $2, $3, $4, 'active', $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22,
        $23, $24, $25, $26, $27, sqlc.narg(principal_member_id), sqlc.arg(answers_course));

-- name: GetMemberInCourse :one
-- The seat, with whom it is and, for an agent someone owns, whose.
SELECT m.*, a.display_name, a.kind AS actor_kind, a.owner_actor_id, o.display_name AS owner_name
FROM course_member m
JOIN actor a ON a.id = m.actor_id
LEFT JOIN actor o ON o.id = a.owner_actor_id
WHERE m.id = $1 AND m.course_id = $2;

-- name: GetMemberInCourseForUpdate :one
-- The same row, locked for the rest of the transaction: the management tools
-- read a seat and write it back, and two of them at once must take turns.
SELECT m.*, a.display_name, a.kind AS actor_kind, a.owner_actor_id, o.display_name AS owner_name
FROM course_member m
JOIN actor a ON a.id = m.actor_id
LEFT JOIN actor o ON o.id = a.owner_actor_id
WHERE m.id = $1 AND m.course_id = $2
FOR UPDATE OF m;

-- name: GetLiveMembership :one
-- Not locked: a live seat found here is only refused, and locking it would
-- wait for its member's calls in flight, and could deadlock with them, to say
-- no. A seat past its expiry, or orphaned (SeatOrphaned), is locked by id
-- before it is removed.
SELECT id, status, expires_at FROM course_member WHERE course_id = $1 AND actor_id = $2 AND status <> 'removed';

-- name: SeatOrphaned :one
-- Whether a seat counts for nothing for good, whatever becomes of it: a
-- delegate's whose principal is removed or past its expiry, or one that does
-- not match its actor's ownership (an owned agent's seat with no principal,
-- or with one that is not its owner's). Neither a removed seat nor an expired
-- one comes back, and an owner is not changed while the agent has a seat in
-- a course that is not archived (actor.set_owner), where only this could
-- find it. With no clock (now null), a principal's expiry is not judged:
-- whoever asks leaves it to seat(), which has one. ListOrphanedSeats is the
-- same rule for every seat, and the authorization queries' owner_matches
-- its other half: a change to one is a change to all three.
SELECT (CASE WHEN m.principal_member_id IS NULL THEN a.owner_actor_id IS NOT NULL
             ELSE p.status = 'removed' OR coalesce(p.expires_at <= sqlc.arg(now), false)
                  OR a.owner_actor_id IS DISTINCT FROM p.actor_id END)::bool AS orphaned
FROM course_member m
JOIN actor a ON a.id = m.actor_id
LEFT JOIN course_member p ON p.id = m.principal_member_id
WHERE m.id = sqlc.arg(member_id);

-- name: ListMembers :many
SELECT m.*, a.display_name, a.kind AS actor_kind, a.owner_actor_id, o.display_name AS owner_name
FROM course_member m
JOIN actor a ON a.id = m.actor_id
LEFT JOIN actor o ON o.id = a.owner_actor_id
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
    perm_action_decide = $14, perm_agent_delegate = $15, perm_conversation_ask = $16, perm_conversation_answer = $17,
    perm_member_invite = $18
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
-- them is waited for. A delegate's seat and its principal's are not taken
-- together in id order but in two calls, the delegate's first: that is the
-- order its own calls take them in (LockLiveMemberForAuthz). Taking the
-- principal's alone, before a delegate's seat is locked FOR UPDATE, is
-- tools.holdPrincipalOf.
SELECT 1 FROM course_member WHERE id = ANY(sqlc.arg(ids)::uuid[]) ORDER BY id FOR KEY SHARE;

-- name: LookupActorForSeating :one
-- The actor a whole email address, or an id, belongs to, for someone seating
-- them, with their seat in this course if they have a live one, and their
-- owner if they are an agent someone owns. The email must match whole, in
-- any case: this finds a person whose address one already has, and lists
-- nobody.
SELECT a.id, a.kind, a.display_name, a.status, m.id AS member_id, a.owner_actor_id, o.display_name AS owner_name
FROM actor a
LEFT JOIN course_member m ON m.actor_id = a.id AND m.course_id = sqlc.arg(course_id) AND m.status <> 'removed'
LEFT JOIN actor o ON o.id = a.owner_actor_id
WHERE a.kind <> 'system'
  AND (a.id = sqlc.narg(actor_id) OR lower(a.email) = lower(sqlc.narg(email)));

-- name: ListLiveDelegatesOf :many
-- The seats of a principal's delegates that are not removed, whatever their
-- status: what its removal removes with it. Read, not locked, by whoever
-- holds the principal FOR UPDATE, before the removal: no delegate is seated
-- by it meanwhile, since that takes its seat, and none is removed by anyone
-- else, since that takes its seat's KEY SHARE first. The removal itself is
-- the database's (course_member_delegates_follow), in the statement that
-- removes the principal; the delegates' rows are only updated there, which
-- the KEY SHARE a delegate's call in flight holds on its own seat does not
-- wait for: the call waits instead for the principal, and then finds it
-- removed.
SELECT id FROM course_member
WHERE principal_member_id = $1 AND status <> 'removed'
ORDER BY id;

-- name: GetSeatPrincipal :one
-- Which seat a seat is a delegate of, if any. Whose delegate a seat is never
-- changes, so it may be read before anything is locked.
SELECT principal_member_id FROM course_member WHERE id = $1;

-- name: PrincipalsSeatsHaveRole :one
-- Whether a principal's own seat, or the seat of another of its delegates
-- than except, is among the seats of one roster role that are not removed or
-- past their expiry: what a delegate's member.update_perms_bulk would reach,
-- and must not (tools.notYourPrincipals). Asked before anything is locked.
SELECT EXISTS (
    SELECT 1 FROM course_member
    WHERE course_id = $1 AND role = sqlc.arg(role) AND status <> 'removed'
      AND (expires_at IS NULL OR expires_at > sqlc.arg(now)) AND id <> sqlc.arg(except_member_id)
      AND (id = sqlc.arg(principal_member_id) OR principal_member_id = sqlc.arg(principal_member_id))
)::bool;

-- name: LockLiveSeatsByRole :many
-- Every seat of one roster role that is not removed or past its expiry,
-- except one, locked in id order: member.update_perms_bulk changes them all
-- or none. Choosing seats by role is what the manager asked for; it is not
-- authorization, which each change goes through on its own.
SELECT id FROM course_member
WHERE course_id = $1 AND role = sqlc.arg(role) AND status <> 'removed'
  AND (expires_at IS NULL OR expires_at > sqlc.arg(now)) AND id <> sqlc.arg(except_member_id)
ORDER BY id
FOR UPDATE;

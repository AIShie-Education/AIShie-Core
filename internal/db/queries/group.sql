-- Group sets, their groups and memberships (docs/schema.md §2.5a, Groups).
-- Who counts as a group's member is live_group_members(), migration 0031:
-- a stay not ended, of a seat that is a student's, not removed and not past
-- its expiry. Every count, list and check here goes through it.

-- Sets -----------------------------------------------------------------------

-- name: InsertGroupSet :exec
INSERT INTO group_set (id, course_id, name, description, signup_open, signup_closes_at, created_by_member_id,
                       created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(course_id), sqlc.arg(name), sqlc.narg(description), sqlc.arg(signup_open),
        sqlc.narg(signup_closes_at), sqlc.arg(created_by_member_id), sqlc.arg(created_at), sqlc.arg(created_at));

-- name: GetGroupSet :one
SELECT * FROM group_set WHERE id = $1 AND course_id = $2;

-- name: ShareGroupSet :one
-- A set, held FOR SHARE to the end of the call: placing students and signing
-- up take it so, and then the groups they touch FOR UPDATE, so that a split
-- or a change of the set's sign-up (FOR UPDATE) is one after the other with
-- them, never interleaved.
SELECT * FROM group_set WHERE id = $1 AND course_id = $2 FOR SHARE;

-- name: LockGroupSet :one
-- A set, held FOR UPDATE: a split, and group_set.update.
SELECT * FROM group_set WHERE id = $1 AND course_id = $2 FOR UPDATE;

-- name: UpdateGroupSet :exec
UPDATE group_set
SET name = sqlc.arg(name), description = sqlc.narg(description), signup_open = sqlc.arg(signup_open),
    signup_closes_at = sqlc.narg(signup_closes_at), archived_at = sqlc.narg(archived_at), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id);

-- name: GroupSetNameTaken :one
-- Another set of the course, not archived, by the same name in any case.
SELECT EXISTS (
    SELECT 1 FROM group_set
    WHERE course_id = sqlc.arg(course_id) AND lower(name) = lower(sqlc.arg(name)::text) AND archived_at IS NULL
      AND id <> sqlc.arg(except_id)
);

-- name: ListGroupSets :many
SELECT * FROM group_set
WHERE course_id = $1 AND (sqlc.arg(include_archived)::bool OR archived_at IS NULL)
ORDER BY created_at, id;

-- Groups ---------------------------------------------------------------------

-- name: InsertGroup :exec
INSERT INTO course_group (id, course_id, set_id, name, capacity, created_by_member_id, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: GetGroup :one
-- A group of the course, with its size now.
SELECT g.*, (SELECT count(*) FROM live_group_members(g.id))::int AS size
FROM course_group g
WHERE g.id = $1 AND g.course_id = $2;

-- name: LockGroups :many
-- Groups of a set, held FOR UPDATE in id order, with their sizes as they
-- stand once held: placing students, signing up, splitting, archiving.
SELECT g.id, g.name, g.capacity, g.archived_at, g.created_at
FROM course_group g
WHERE g.set_id = sqlc.arg(set_id) AND g.id = ANY(sqlc.arg(ids)::uuid[])
ORDER BY g.id
FOR UPDATE;

-- name: ShareGroup :one
-- The group whose work is handed in, held FOR SHARE: a move of one of its
-- members (FOR UPDATE) and a hand-in are one after the other.
SELECT id, set_id, name, archived_at FROM course_group WHERE id = $1 AND course_id = $2 FOR SHARE;

-- name: UpdateGroup :exec
UPDATE course_group SET name = sqlc.arg(name), capacity = sqlc.narg(capacity), archived_at = sqlc.narg(archived_at)
WHERE id = sqlc.arg(id);

-- name: GroupNameTaken :one
-- Another group of the set, not archived, by the same name in any case.
SELECT EXISTS (
    SELECT 1 FROM course_group
    WHERE set_id = sqlc.arg(set_id) AND lower(name) = lower(sqlc.arg(name)::text) AND archived_at IS NULL
      AND id <> sqlc.arg(except_id)
);

-- name: ListGroupsOfSets :many
-- The groups of the sets, with their sizes now, the oldest first.
SELECT g.*, (SELECT count(*) FROM live_group_members(g.id))::int AS size
FROM course_group g
WHERE g.set_id = ANY(sqlc.arg(set_ids)::uuid[])
ORDER BY g.set_id, g.created_at, g.id;

-- Members --------------------------------------------------------------------

-- name: ListLiveMembersOfGroups :many
-- Who is in each of the groups now, with their names, and when and how they
-- joined: those the reader's student scope reaches (a delegate's
-- principal's too), "all" for a reader who is shown them all.
SELECT gm.group_id, gm.member_id, a.display_name, gm.joined_at, gm.joined_how
FROM group_membership gm
JOIN course_member m ON m.id = gm.member_id
JOIN actor a ON a.id = m.actor_id
WHERE gm.group_id = ANY(sqlc.arg(group_ids)::uuid[]) AND gm.left_at IS NULL
  AND gm.member_id IN (SELECT live_group_members(gm.group_id))
  AND (sqlc.arg(student_all)::bool OR EXISTS (
        SELECT 1 FROM member_student_scope y WHERE y.member_id = sqlc.arg(member_id) AND y.student_member_id = gm.member_id))
  AND (sqlc.arg(principal_student_all)::bool OR EXISTS (
        SELECT 1 FROM member_student_scope py WHERE py.member_id = sqlc.arg(principal_id) AND py.student_member_id = gm.member_id))
ORDER BY gm.group_id, a.display_name, gm.member_id;

-- name: LiveMembersOf :many
-- A group's members now, in id order.
SELECT lm.member_id::uuid AS member_id FROM live_group_members(sqlc.arg(group_id)::uuid) AS lm(member_id) ORDER BY 1;

-- name: CurrentMembership :one
-- A student's stay in a group of the set that has not ended, whether or not
-- they count as a member now.
SELECT id, group_id FROM group_membership WHERE set_id = $1 AND member_id = $2 AND left_at IS NULL;

-- name: ListCurrentMemberships :many
-- The stays of these students in groups of the set that have not ended.
SELECT id, group_id, member_id FROM group_membership
WHERE set_id = sqlc.arg(set_id) AND member_id = ANY(sqlc.arg(member_ids)::uuid[]) AND left_at IS NULL;

-- name: ListLiveMembershipsOfSet :many
-- Every stay in a group of the set that counts now: a live member's.
SELECT gm.id, gm.group_id, gm.member_id
FROM group_membership gm
WHERE gm.set_id = $1 AND gm.left_at IS NULL AND gm.member_id IN (SELECT live_group_members(gm.group_id))
ORDER BY gm.member_id;

-- name: CurrentGroupOf :one
-- The group of the set a student counts as a member of now, if any.
SELECT g.id, g.name
FROM group_membership gm
JOIN course_group g ON g.id = gm.group_id
WHERE gm.set_id = sqlc.arg(set_id) AND gm.member_id = sqlc.arg(member_id) AND gm.left_at IS NULL
  AND gm.member_id IN (SELECT live_group_members(gm.group_id));

-- name: InsertMembership :exec
INSERT INTO group_membership (id, course_id, set_id, group_id, member_id, joined_at, joined_by_member_id, joined_how,
                              joined_action_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: EndMembership :execrows
UPDATE group_membership
SET left_at = sqlc.arg(left_at), left_by_member_id = sqlc.arg(left_by_member_id), left_how = sqlc.arg(left_how),
    left_action_id = sqlc.arg(left_action_id)
WHERE id = sqlc.arg(id) AND left_at IS NULL;

-- name: ListMembershipHistory :many
-- Every stay in a group of the set, ended or not, the newest first, with the
-- student's name.
SELECT gm.*, a.display_name
FROM group_membership gm
JOIN course_member m ON m.id = gm.member_id
JOIN actor a ON a.id = m.actor_id
WHERE gm.set_id = $1
ORDER BY gm.joined_at DESC, gm.id DESC;

-- name: ListLiveStudents :many
-- The course's students now: a seat whose roster role is student, not
-- removed and not past its expiry, as live_group_members counts them, with
-- their names, in id order (UUID v7: seat order).
SELECT m.id, a.display_name
FROM course_member m
JOIN actor a ON a.id = m.actor_id
WHERE m.course_id = $1 AND m.role = 'student' AND m.status <> 'removed'
  AND (m.expires_at IS NULL OR m.expires_at > now())
ORDER BY m.id;

-- name: ListStudentSeats :many
-- What the roster says of these seats of the course: placing and signing up
-- take a student's seat, live, alone.
SELECT m.id, m.role, m.status, (m.expires_at IS NULL OR m.expires_at > now())::bool AS unexpired
FROM course_member m
WHERE m.course_id = sqlc.arg(course_id) AND m.id = ANY(sqlc.arg(ids)::uuid[]);

-- Work -----------------------------------------------------------------------

-- name: ListWorkOfGroups :many
-- Every submission of the groups to an assignment of their set, any state,
-- a draft included, the latest attempt first for each assignment.
SELECT s.group_id::uuid AS group_id, s.assignment_id, a.title, s.id AS submission_id, s.attempt, s.state
FROM submission s
JOIN assignment a ON a.id = s.assignment_id
WHERE s.group_id = ANY(sqlc.arg(group_ids)::uuid[])
ORDER BY s.group_id, s.assignment_id, s.attempt DESC;

-- name: ListReadableWorkOfGroups :many
-- ListWorkOfGroups, of the submissions the reader may read: one of whose
-- students (submission_students) the reader's student scope reaches, and
-- their principal's, as authorize() reaches a group's work. A member reads
-- the group's draft and the attempts they are part of, not one handed in
-- before they joined.
SELECT s.group_id::uuid AS group_id, s.assignment_id, a.title, s.id AS submission_id, s.attempt, s.state
FROM submission s
JOIN assignment a ON a.id = s.assignment_id
WHERE s.group_id = ANY(sqlc.arg(group_ids)::uuid[])
  AND (sqlc.arg(student_all)::bool OR EXISTS (
        SELECT 1 FROM submission_students(s.id) AS st(member_id)
        JOIN member_student_scope y ON y.student_member_id = st.member_id
        WHERE y.member_id = sqlc.arg(member_id)))
  AND (sqlc.arg(principal_student_all)::bool OR EXISTS (
        SELECT 1 FROM submission_students(s.id) AS st(member_id)
        JOIN member_student_scope py ON py.student_member_id = st.member_id
        WHERE py.member_id = sqlc.arg(principal_id)))
ORDER BY s.group_id, s.assignment_id, s.attempt DESC;

-- name: ListAssignmentsOfSets :many
-- The assignments using each of the sets, within the caller's assignment
-- scope; one not published only for whoever may see it.
SELECT a.id, a.title, a.group_set_id::uuid AS group_set_id, a.published_at
FROM assignment a
WHERE a.group_set_id = ANY(sqlc.arg(set_ids)::uuid[])
  AND (sqlc.arg(include_unpublished)::bool OR a.published_at IS NOT NULL)
  AND (sqlc.arg(assignment_all)::bool OR EXISTS (
        SELECT 1 FROM member_assignment_scope y WHERE y.member_id = sqlc.arg(member_id) AND y.assignment_id = a.id))
  AND (sqlc.arg(principal_assignment_all)::bool OR EXISTS (
        SELECT 1 FROM member_assignment_scope py WHERE py.member_id = sqlc.arg(principal_id) AND py.assignment_id = a.id))
ORDER BY a.group_set_id, a.created_at, a.id;

-- name: SetHasAssignments :one
SELECT EXISTS (SELECT 1 FROM assignment WHERE group_set_id = $1);

-- name: ListUnassignedStudents :many
-- The course's students now in no group of the set, within the caller's
-- student scope (a delegate's principal's too), with their names.
SELECT m.id, a.display_name
FROM course_member m
JOIN actor a ON a.id = m.actor_id
WHERE m.course_id = sqlc.arg(course_id) AND m.role = 'student' AND m.status <> 'removed'
  AND (m.expires_at IS NULL OR m.expires_at > now())
  AND NOT EXISTS (SELECT 1 FROM group_membership gm WHERE gm.set_id = sqlc.arg(set_id) AND gm.member_id = m.id AND gm.left_at IS NULL)
  AND (sqlc.arg(student_all)::bool OR EXISTS (
        SELECT 1 FROM member_student_scope y WHERE y.member_id = sqlc.arg(member_id) AND y.student_member_id = m.id))
  AND (sqlc.arg(principal_student_all)::bool OR EXISTS (
        SELECT 1 FROM member_student_scope py WHERE py.member_id = sqlc.arg(principal_id) AND py.student_member_id = m.id))
ORDER BY a.display_name, m.id;

-- name: StudentsInListedScope :many
-- Of these students, those a seat's list of students names.
SELECT student_member_id FROM member_student_scope
WHERE member_id = sqlc.arg(member_id) AND student_member_id = ANY(sqlc.arg(ids)::uuid[]);

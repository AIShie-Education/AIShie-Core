-- name: InsertSubmission :exec
-- A student's draft. Its row of submission_member, its student, is written
-- with it by the database (submission_member_own).
INSERT INTO submission (id, assignment_id, course_id, student_member_id, attempt, body, state, created_at)
VALUES ($1, $2, $3, sqlc.arg(student_member_id)::uuid, $4, $5, 'draft', $6);

-- name: InsertGroupSubmission :exec
-- A group's draft: whose work it is is the group's members now, until it is
-- handed in (submission_students).
INSERT INTO submission (id, assignment_id, course_id, group_id, attempt, body, state, created_at)
VALUES ($1, $2, $3, sqlc.arg(group_id)::uuid, $4, $5, 'draft', $6);

-- name: GetSubmissionFull :one
SELECT id, assignment_id, course_id, student_member_id, group_id, attempt, body, instructions_version_id,
       state, submitted_at, created_at, revision, revised_at, revised_by_member_id, submitted_by_member_id
FROM submission WHERE id = $1 AND course_id = $2;

-- name: GetSubmissionFullForUpdate :one
-- GetSubmissionFull, locked until the transaction ends, so that what
-- submission.submit checks is what it hands in: an edit to the draft, or a
-- file added to it or archived from it, meanwhile waits, and then finds it
-- handed in.
SELECT id, assignment_id, course_id, student_member_id, group_id, attempt, body, instructions_version_id,
       state, submitted_at, created_at, revision, revised_at, revised_by_member_id, submitted_by_member_id
FROM submission WHERE id = $1 AND course_id = $2
FOR UPDATE;

-- name: LockSubmissionsOf :many
-- All of one student's attempts at one assignment, locked, newest first.
SELECT id, attempt, state FROM submission
WHERE assignment_id = $1 AND student_member_id = sqlc.arg(student_member_id)::uuid
ORDER BY attempt DESC
FOR UPDATE;

-- name: ListSubmissionsOf :many
-- LockSubmissionsOf, not locked: what a tool's Validate reads of a
-- student's attempts before a proposal is queued, which the tool reads again
-- under the lock when it is carried out.
SELECT id, attempt, state FROM submission
WHERE assignment_id = $1 AND student_member_id = sqlc.arg(student_member_id)::uuid
ORDER BY attempt DESC;

-- name: LockGroupSubmissionsOf :many
-- All of one group's attempts at one assignment, locked, newest first.
SELECT id, attempt, state FROM submission
WHERE assignment_id = $1 AND group_id = sqlc.arg(group_id)::uuid
ORDER BY attempt DESC
FOR UPDATE;

-- name: ListGroupSubmissionsOf :many
-- LockGroupSubmissionsOf, not locked.
SELECT id, attempt, state FROM submission
WHERE assignment_id = $1 AND group_id = sqlc.arg(group_id)::uuid
ORDER BY attempt DESC;

-- name: ReopenMissingSubmission :exec
-- A 'missing' row is a placeholder written when the due date passed with
-- nothing handed in. Late work takes it over rather than sitting beside it.
UPDATE submission SET state = 'draft', body = $2 WHERE id = $1 AND state = 'missing';

-- name: UpdateSubmissionDraft :one
-- A draft's new text, written by whom and when. The database counts the
-- revision (submission_draft_revised); none comes back when it is no
-- longer a draft.
UPDATE submission SET body = $2, revised_by_member_id = sqlc.arg(revised_by_member_id), revised_at = sqlc.arg(revised_at)
WHERE id = $1 AND state = 'draft'
RETURNING revision;

-- name: SubmitSubmission :execrows
UPDATE submission
SET state = $2, submitted_at = $3, instructions_version_id = $4, submitted_by_member_id = sqlc.narg(submitted_by_member_id)
WHERE id = $1 AND state = 'draft';

-- name: SetSubmissionLateness :execrows
UPDATE submission SET state = $2 WHERE id = $1 AND state IN ('submitted', 'late') AND state <> $2;

-- name: ListSubmissions :many
-- Scope in SQL: a submission is within it when the seat reaches one of its
-- students (submission_students), and, for a delegate, its principal one of
-- them too. student_member_id finds the work a student is part of: their
-- own, or a group's, which for a group's draft is its members now.
SELECT s.id, s.assignment_id, s.course_id, s.student_member_id, s.group_id, g.name AS group_name, s.attempt, s.state,
       s.submitted_at, s.created_at, s.revision, s.revised_at, s.revised_by_member_id, s.submitted_by_member_id
FROM submission s
LEFT JOIN course_group g ON g.id = s.group_id
WHERE s.course_id = $1 AND s.id > sqlc.arg(after)
  AND (sqlc.narg(assignment_id)::uuid IS NULL OR s.assignment_id = sqlc.narg(assignment_id))
  AND (sqlc.narg(group_id)::uuid IS NULL OR s.group_id = sqlc.narg(group_id))
  AND (sqlc.narg(student_member_id)::uuid IS NULL OR sqlc.narg(student_member_id)::uuid IN (SELECT submission_students(s.id)))
  AND (sqlc.arg(student_all)::bool OR EXISTS (
        SELECT 1 FROM submission_students(s.id) st(member_id)
        JOIN member_student_scope x ON x.student_member_id = st.member_id
        WHERE x.member_id = sqlc.arg(member_id)))
  AND (sqlc.arg(assignment_all)::bool OR EXISTS (
        SELECT 1 FROM member_assignment_scope y WHERE y.member_id = sqlc.arg(member_id) AND y.assignment_id = s.assignment_id))
  -- A delegate's principal's scope, the same way; "all" for any other seat.
  AND (sqlc.arg(principal_student_all)::bool OR EXISTS (
        SELECT 1 FROM submission_students(s.id) pt(member_id)
        JOIN member_student_scope px ON px.student_member_id = pt.member_id
        WHERE px.member_id = sqlc.arg(principal_id)))
  AND (sqlc.arg(principal_assignment_all)::bool OR EXISTS (
        SELECT 1 FROM member_assignment_scope py WHERE py.member_id = sqlc.arg(principal_id) AND py.assignment_id = s.assignment_id))
ORDER BY s.id
LIMIT sqlc.arg(max_rows);

-- name: ListAssignmentRoster :many
-- Every current student of the course whom the caller's student scope
-- reaches, with their latest attempt at one assignment, if any: the students
-- who have not started are rows too, with no submission. The caller's
-- assignment scope is checked on the target, before this runs. A delegate
-- reaches only the students its principal reaches too.
SELECT m.id AS student_member_id, a.display_name, m.status AS member_status,
       s.id AS submission_id, s.attempt, s.state, s.submitted_at
FROM course_member m
JOIN actor a ON a.id = m.actor_id
LEFT JOIN submission s ON s.assignment_id = sqlc.arg(assignment_id) AND s.student_member_id = m.id
    AND s.attempt = (SELECT max(z.attempt) FROM submission z
                     WHERE z.assignment_id = sqlc.arg(assignment_id) AND z.student_member_id = m.id)
WHERE m.course_id = sqlc.arg(course_id) AND m.role = 'student' AND m.status <> 'removed'
  AND m.id > sqlc.arg(after)
  AND (sqlc.arg(student_all)::bool OR EXISTS (
        SELECT 1 FROM member_student_scope y WHERE y.member_id = sqlc.arg(member_id) AND y.student_member_id = m.id))
  AND (sqlc.arg(principal_student_all)::bool OR EXISTS (
        SELECT 1 FROM member_student_scope py WHERE py.member_id = sqlc.arg(principal_id) AND py.student_member_id = m.id))
ORDER BY m.id
LIMIT sqlc.arg(max_rows);

-- name: ListGroupAssignmentRoster :many
-- ListAssignmentRoster for a group assignment: each student with their
-- group in its set now, if any; the latest attempt of the work they are part
-- of (a group's handed in or recorded missing, which names them); and their
-- group's open draft, if it has one.
SELECT m.id AS student_member_id, a.display_name, m.status AS member_status,
       gm.group_id AS group_id,
       p.id AS part_submission_id, p.attempt AS part_attempt, p.state AS part_state, p.submitted_at AS part_submitted_at,
       p.group_id AS part_group_id,
       d.id AS draft_submission_id, d.attempt AS draft_attempt
FROM course_member m
JOIN actor a ON a.id = m.actor_id
LEFT JOIN group_membership gm ON gm.set_id = sqlc.arg(set_id) AND gm.member_id = m.id AND gm.left_at IS NULL
LEFT JOIN submission p ON p.id = (
    SELECT z.id
    FROM submission_member x JOIN submission z ON z.id = x.submission_id
    WHERE x.assignment_id = sqlc.arg(assignment_id) AND x.member_id = m.id
    ORDER BY z.attempt DESC LIMIT 1)
LEFT JOIN submission d ON d.id = (
    SELECT z.id
    FROM submission z
    WHERE z.assignment_id = sqlc.arg(assignment_id) AND z.group_id = gm.group_id AND z.state = 'draft'
    ORDER BY z.attempt DESC LIMIT 1)
WHERE m.course_id = sqlc.arg(course_id) AND m.role = 'student' AND m.status <> 'removed'
  AND m.id > sqlc.arg(after)
  AND (sqlc.arg(student_all)::bool OR EXISTS (
        SELECT 1 FROM member_student_scope y WHERE y.member_id = sqlc.arg(member_id) AND y.student_member_id = m.id))
  AND (sqlc.arg(principal_student_all)::bool OR EXISTS (
        SELECT 1 FROM member_student_scope py WHERE py.member_id = sqlc.arg(principal_id) AND py.student_member_id = m.id))
ORDER BY m.id
LIMIT sqlc.arg(max_rows);

-- name: ListGroupsWithLatestWork :many
-- The set's groups not archived, each with its latest attempt at the
-- assignment, if any, the oldest group first.
SELECT g.id, g.name, s.id AS submission_id, s.attempt, s.state, s.submitted_at, s.submitted_by_member_id
FROM course_group g
LEFT JOIN submission s ON s.id = (
    SELECT z.id
    FROM submission z
    WHERE z.assignment_id = sqlc.arg(assignment_id) AND z.group_id = g.id
    ORDER BY z.attempt DESC LIMIT 1)
WHERE g.set_id = sqlc.arg(set_id) AND g.archived_at IS NULL
ORDER BY g.created_at, g.id;

-- Whose work a submission is ---------------------------------------------------

-- name: LockWorkMembersOfAssignment :exec
-- Every write of whose work a group's submission is takes it — a hand-in, a
-- 'missing' record, a correction, the due sweep — so that one student comes
-- to be part of one group's work for an assignment: 1095324503 is "AISW".
SELECT pg_advisory_xact_lock(1095324503, hashtext((sqlc.arg(assignment_id)::uuid)::text));

-- name: InsertSubmissionMembers :exec
-- Rows of whose work a group's submission is, as it was handed in, recorded
-- missing or corrected. course_id, assignment_id and group_id are the
-- submission's, which submission_member_guarded writes.
INSERT INTO submission_member (submission_id, member_id, course_id, assignment_id, added_at, added_how, added_by_member_id)
SELECT sqlc.arg(submission_id)::uuid, x, sqlc.arg(course_id)::uuid, sqlc.arg(assignment_id)::uuid, sqlc.arg(added_at)::timestamptz,
       sqlc.arg(added_how)::text, sqlc.narg(added_by_member_id)::uuid
FROM unnest(sqlc.arg(member_ids)::uuid[]) AS x;

-- name: DeleteSubmissionMembers :execrows
-- Of a group's submission: its rows while it is a draft again, or those a
-- correction takes off (BeginMemberCorrection).
DELETE FROM submission_member WHERE submission_id = sqlc.arg(submission_id) AND member_id = ANY(sqlc.arg(member_ids)::uuid[]);

-- name: DeleteAllSubmissionMembers :exec
-- A group's 'missing' row taken over by late work, a draft again: whose it
-- is is its group's members now, until it is handed in.
DELETE FROM submission_member WHERE submission_id = $1;

-- name: BeginMemberCorrection :exec
-- Says, for this transaction, that whose work the submission is is being
-- corrected (submission.set_members): submission_member_guarded lets its
-- rows go, which the grade's foreign key still holds while a grade names one.
SELECT set_config('aishie.correcting_submission', (sqlc.arg(submission_id)::uuid)::text, true);

-- name: EndMemberCorrection :exec
SELECT set_config('aishie.correcting_submission', '', true);

-- name: ListSubmissionMembers :many
-- Whose work a submission is, as its rows say, with their names and how each
-- came to be part of it.
SELECT x.member_id, a.display_name, x.added_at, x.added_how, x.added_by_member_id
FROM submission_member x
JOIN course_member m ON m.id = x.member_id
JOIN actor a ON a.id = m.actor_id
WHERE x.submission_id = $1
ORDER BY a.display_name, x.member_id;

-- name: SubmissionStudents :many
-- The students of a submission (submission_students): its student; a
-- group's draft's members now; a group's work's rows.
SELECT st.member_id::uuid AS member_id FROM submission_students(sqlc.arg(submission_id)::uuid) AS st(member_id) ORDER BY 1;

-- name: NamesOfMembers :many
SELECT m.id, a.display_name FROM course_member m JOIN actor a ON a.id = m.actor_id
WHERE m.id = ANY(sqlc.arg(ids)::uuid[]) ORDER BY a.display_name, m.id;

-- name: OtherWorkNaming :many
-- Of these students, those another group's work for the assignment names,
-- with that work: a student is part of one group's work for an assignment.
SELECT DISTINCT ON (x.member_id) x.member_id, x.submission_id
FROM submission_member x
WHERE x.assignment_id = sqlc.arg(assignment_id) AND x.member_id = ANY(sqlc.arg(member_ids)::uuid[])
  AND x.group_id IS DISTINCT FROM sqlc.narg(group_id)::uuid
ORDER BY x.member_id, x.submission_id;

-- name: MemberGradedOnSubmission :many
-- Of these students, those a grade on the submission names, any state.
SELECT DISTINCT g.student_member_id FROM grade g
WHERE g.submission_id = sqlc.arg(submission_id) AND g.student_member_id = ANY(sqlc.arg(member_ids)::uuid[])
ORDER BY 1;

-- name: GradeProposedOnSubmission :one
-- A grade proposed for the submission and not yet decided.
SELECT EXISTS (
    SELECT 1 FROM action
    WHERE target_type = 'submission' AND target_id = $1 AND action_type IN ('grade.submit', 'grade.regrade', 'grade.adjust')
      AND status = 'proposed'
);

-- name: GroupsWithHandedInWork :many
-- Of these groups, those with work handed in (not a draft) for an assignment
-- of their set, with it: sign-up never changes who did handed-in work.
SELECT DISTINCT ON (s.group_id) s.group_id::uuid AS group_id, s.assignment_id, s.id AS submission_id, s.state
FROM submission s
WHERE s.group_id = ANY(sqlc.arg(group_ids)::uuid[]) AND s.state <> 'draft'
ORDER BY s.group_id, s.assignment_id, s.attempt DESC;

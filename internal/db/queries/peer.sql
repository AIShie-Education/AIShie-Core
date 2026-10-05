-- Peer evaluation within a group (docs/schema.md §2.5b, Peer evaluation):
-- an assignment's peer form, its raters' sheets and what they gave each
-- member. Who is in a group's circle is worked out in the tool, from the
-- group's work and live_group_members (migration 0031).

-- The form -------------------------------------------------------------------

-- name: GetPeerForm :one
SELECT * FROM peer_form WHERE assignment_id = $1 AND course_id = $2;

-- name: SharePeerForm :one
-- The form, held FOR SHARE to the end of the call: a sheet written, or a
-- grade worked out from the sheets, against the form as it stands, which a
-- change of it (FOR UPDATE) waits for or is waited for by.
SELECT * FROM peer_form WHERE assignment_id = $1 AND course_id = $2 FOR SHARE;

-- name: LockPeerForm :one
-- The form, held FOR UPDATE: peer_form.set.
SELECT * FROM peer_form WHERE assignment_id = $1 AND course_id = $2 FOR UPDATE;

-- name: InsertPeerForm :exec
INSERT INTO peer_form (assignment_id, course_id, enabled, kind, criteria, scale_min, scale_max, self_evaluation, opens,
                       opens_at, closes_at, weight, share_with_students, created_by_member_id, created_at,
                       updated_by_member_id, updated_at)
VALUES (sqlc.arg(assignment_id), sqlc.arg(course_id), sqlc.arg(enabled), sqlc.arg(kind), sqlc.narg(criteria),
        sqlc.narg(scale_min), sqlc.narg(scale_max), sqlc.arg(self_evaluation), sqlc.arg(opens), sqlc.narg(opens_at),
        sqlc.arg(closes_at), sqlc.arg(weight), sqlc.arg(share_with_students), sqlc.arg(member_id), sqlc.arg(now),
        sqlc.arg(member_id), sqlc.arg(now));

-- name: UpdatePeerForm :one
-- The form as a change leaves it, its version counted.
UPDATE peer_form
SET enabled = sqlc.arg(enabled), kind = sqlc.arg(kind), criteria = sqlc.narg(criteria), scale_min = sqlc.narg(scale_min),
    scale_max = sqlc.narg(scale_max), self_evaluation = sqlc.arg(self_evaluation), opens = sqlc.arg(opens),
    opens_at = sqlc.narg(opens_at), closes_at = sqlc.arg(closes_at), weight = sqlc.arg(weight),
    share_with_students = sqlc.arg(share_with_students), version = version + 1,
    updated_by_member_id = sqlc.arg(member_id), updated_at = sqlc.arg(now)
WHERE assignment_id = sqlc.arg(assignment_id)
RETURNING version;

-- name: PeerFormInUse :one
-- Whether a sheet names the form: its shape no longer changes.
SELECT EXISTS (SELECT 1 FROM peer_review WHERE assignment_id = $1);

-- name: PeerFormEnabled :one
SELECT EXISTS (SELECT 1 FROM peer_form WHERE assignment_id = $1 AND enabled);

-- Sheets ---------------------------------------------------------------------

-- name: LockPeerSheet :exec
-- One writer of a rater's sheet for an assignment at a time: the current
-- sheet is replaced, or the first written, by one call while another waits.
SELECT pg_advisory_xact_lock(hashtextextended(
    'peer-sheet:' || (sqlc.arg(assignment_id)::uuid)::text || ':' || (sqlc.arg(rater_member_id)::uuid)::text, 0));

-- name: SupersedePeerSheet :one
-- The rater's current sheet, superseded by the new one about to be written
-- (a deferred key): the id of the one replaced.
UPDATE peer_review SET superseded_by = sqlc.arg(new_id)
WHERE assignment_id = sqlc.arg(assignment_id) AND rater_member_id = sqlc.arg(rater_member_id) AND superseded_by IS NULL
RETURNING id;

-- name: InsertPeerReview :exec
INSERT INTO peer_review (id, course_id, assignment_id, group_id, rater_member_id, comment, created_by_action_id, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: InsertPeerReviewEntry :exec
-- Written with its sheet (peer_review_entry_kept), its course the sheet's.
INSERT INTO peer_review_entry (review_id, course_id, ratee_member_id, ratings, share, comment)
VALUES (sqlc.arg(review_id), sqlc.arg(course_id), sqlc.arg(ratee_member_id), sqlc.narg(ratings), sqlc.narg(share),
        sqlc.narg(comment));

-- name: ListCurrentPeerSheets :many
-- The current sheets of these raters for the assignment, entry by entry,
-- with each rater's name.
SELECT r.id, r.rater_member_id, r.group_id, r.comment, r.created_at, e.ratee_member_id, e.ratings, e.share,
       e.comment AS entry_comment, a.display_name AS rater_name
FROM peer_review r
JOIN peer_review_entry e ON e.review_id = r.id
JOIN course_member m ON m.id = r.rater_member_id
JOIN actor a ON a.id = m.actor_id
WHERE r.assignment_id = sqlc.arg(assignment_id) AND r.superseded_by IS NULL
  AND r.rater_member_id = ANY(sqlc.arg(rater_ids)::uuid[])
ORDER BY r.rater_member_id, e.ratee_member_id;

-- name: CountCurrentPeerSheets :one
-- The current sheets of the assignment: what deleting it counts.
SELECT count(*)::int FROM peer_review WHERE assignment_id = $1 AND superseded_by IS NULL;

-- name: ListPeerRaters :many
-- Everyone who wrote a sheet for the assignment, current or not: whose
-- sheets go when it is deleted.
SELECT DISTINCT rater_member_id FROM peer_review WHERE assignment_id = $1 ORDER BY 1;

-- Circles --------------------------------------------------------------------

-- name: LatestHandedWorkOfGroup :one
-- The group's latest work for the assignment that is not a draft: handed in
-- or recorded missing. Its members are the group's circle.
SELECT id, attempt, state, submitted_at
FROM submission
WHERE assignment_id = sqlc.arg(assignment_id) AND group_id = sqlc.arg(group_id)::uuid AND state <> 'draft'
ORDER BY attempt DESC
LIMIT 1;

-- name: GroupHasHandedIn :one
-- Whether the group has handed work in for the assignment, on time or late:
-- what opens a form that opens on hand-in, for the group.
SELECT EXISTS (
    SELECT 1 FROM submission
    WHERE assignment_id = sqlc.arg(assignment_id) AND group_id = sqlc.arg(group_id)::uuid AND state IN ('submitted', 'late')
);

-- name: WorkGroupsNaming :many
-- The groups whose work for the assignment, handed in or recorded missing,
-- names the student.
SELECT DISTINCT group_id::uuid AS group_id
FROM submission_member
WHERE assignment_id = sqlc.arg(assignment_id) AND member_id = sqlc.arg(member_id) AND group_id IS NOT NULL
ORDER BY 1;

-- Grades ---------------------------------------------------------------------

-- name: LiveGroupGradeOfWork :one
-- The newest group grade on the work that a live grade is given from, a
-- draft or posted: the group's score as it stands.
SELECT gg.*
FROM group_grade gg
WHERE gg.submission_id = $1
  AND EXISTS (SELECT 1 FROM grade g WHERE g.group_grade_id = gg.id AND g.superseded_by IS NULL)
ORDER BY gg.created_at DESC, gg.id DESC
LIMIT 1;

-- name: ListLiveGradesFromGroupGrades :many
-- Every live grade of the assignment given from a group grade, a draft or
-- posted, with the group grade's score and whether it allows extra, and the
-- work's group: what counting peer evaluation writes again.
SELECT g.id, g.student_member_id, g.submission_id, g.score, g.feedback, g.breakdown, g.rubric_version_id, g.posted_at,
       g.group_grade_id::uuid AS group_grade_id, g.adjust_kind, g.adjust_points, g.adjust_reason, g.adjust_by_member_id,
       g.adjust_detail, s.group_id::uuid AS group_id, gg.score AS group_score, gg.allow_extra
FROM grade g
JOIN submission s ON s.id = g.submission_id
JOIN group_grade gg ON gg.id = g.group_grade_id
WHERE s.assignment_id = $1 AND g.origin = 'entered' AND g.superseded_by IS NULL
ORDER BY g.id;

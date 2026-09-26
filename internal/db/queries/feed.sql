-- name: ListGrades :many
-- Scope in SQL. Two more rules ride along:
--   * an unposted or superseded grade is shown only to a member who grades
--     (include_drafts); everyone else sees live posted grades and nothing else;
--   * a grade on a component belongs to no single assignment, so a member
--     limited to listed assignments does not see it at all.
SELECT g.id, g.student_member_id, g.submission_id, g.component_id, s.assignment_id, g.origin, g.score,
       g.feedback, g.breakdown, g.rubric_version_id, g.grader_member_id, g.created_by_action_id,
       g.posted_at, g.posted_by_member_id, g.superseded_by, g.created_at
FROM grade g
JOIN course_member sm ON sm.id = g.student_member_id
LEFT JOIN submission s ON s.id = g.submission_id
WHERE sm.course_id = $1 AND g.id > sqlc.arg(after)
  AND (sqlc.narg(student_member_id)::uuid IS NULL OR g.student_member_id = sqlc.narg(student_member_id))
  AND (sqlc.narg(assignment_id)::uuid IS NULL OR s.assignment_id = sqlc.narg(assignment_id))
  AND (sqlc.arg(include_drafts)::bool OR (g.posted_at IS NOT NULL AND g.superseded_by IS NULL))
  AND (sqlc.arg(student_all)::bool OR EXISTS (
        SELECT 1 FROM member_student_scope x WHERE x.member_id = sqlc.arg(member_id) AND x.student_member_id = g.student_member_id))
  AND (sqlc.arg(assignment_all)::bool OR (s.assignment_id IS NOT NULL AND EXISTS (
        SELECT 1 FROM member_assignment_scope y WHERE y.member_id = sqlc.arg(member_id) AND y.assignment_id = s.assignment_id)))
  -- A delegate's principal's scope, the same way; "all" for any other seat.
  AND (sqlc.arg(principal_student_all)::bool OR EXISTS (
        SELECT 1 FROM member_student_scope px WHERE px.member_id = sqlc.arg(principal_id) AND px.student_member_id = g.student_member_id))
  AND (sqlc.arg(principal_assignment_all)::bool OR (s.assignment_id IS NOT NULL AND EXISTS (
        SELECT 1 FROM member_assignment_scope py WHERE py.member_id = sqlc.arg(principal_id) AND py.assignment_id = s.assignment_id)))
ORDER BY g.id
LIMIT sqlc.arg(max_rows);

-- name: GetGradeFull :one
SELECT g.id, g.student_member_id, g.submission_id, g.component_id, s.assignment_id, g.origin, g.score,
       g.feedback, g.breakdown, g.rubric_version_id, g.grader_member_id, g.created_by_action_id,
       g.posted_at, g.posted_by_member_id, g.superseded_by, g.created_at
FROM grade g
JOIN course_member sm ON sm.id = g.student_member_id
LEFT JOIN submission s ON s.id = g.submission_id
WHERE g.id = $1 AND sm.course_id = $2;

-- name: ListEvents :many
-- The feed, from a cursor. Three filters, all here rather than afterwards:
--   * type: what kinds of event this member's permissions let it see, worked
--     out by the caller from the member row — plus, always, the events of
--     its own actions, which is how an agent learns what became of a proposal;
--     news of a conversation instead goes to its two participants and nobody
--     else, whoever caused it (a removal that closed it, say);
--   * student scope, exactly as authorize() step 4;
--   * assignment scope as step 5, including its extra case: an event that
--     names a student but no assignment (a total, a component grade) spans
--     assignments and is for members whose assignment scope is the whole course;
--   * for a delegate, both again with its principal's scope ("all" for any
--     other seat).
SELECT e.seq, e.type, e.course_id, e.action_id, e.subject_type, e.subject_id,
       e.student_member_id, e.assignment_id, e.payload, e.occurred_at
FROM event e
WHERE e.course_id = $1 AND e.seq > sqlc.arg(since_seq)
  AND ( (e.subject_type <> 'conversation'
          AND ( e.type = ANY(sqlc.arg(visible_types)::text[])
                OR EXISTS (SELECT 1 FROM action a WHERE a.id = e.action_id AND a.member_id = sqlc.arg(member_id)) ))
        OR (e.subject_type = 'conversation' AND EXISTS (
              SELECT 1 FROM conversation c
              WHERE c.id = e.subject_id
                AND (c.opener_member_id = sqlc.arg(member_id) OR c.respondent_member_id = sqlc.arg(member_id)))) )
  AND ( e.student_member_id IS NULL OR sqlc.arg(student_all)::bool OR EXISTS (
        SELECT 1 FROM member_student_scope x WHERE x.member_id = sqlc.arg(member_id) AND x.student_member_id = e.student_member_id) )
  AND ( sqlc.arg(assignment_all)::bool
        OR (e.assignment_id IS NULL AND e.student_member_id IS NULL)
        OR (e.assignment_id IS NOT NULL AND EXISTS (
              SELECT 1 FROM member_assignment_scope y WHERE y.member_id = sqlc.arg(member_id) AND y.assignment_id = e.assignment_id)) )
  AND ( e.student_member_id IS NULL OR sqlc.arg(principal_student_all)::bool OR EXISTS (
        SELECT 1 FROM member_student_scope px WHERE px.member_id = sqlc.arg(principal_id) AND px.student_member_id = e.student_member_id) )
  AND ( sqlc.arg(principal_assignment_all)::bool
        OR (e.assignment_id IS NULL AND e.student_member_id IS NULL)
        OR (e.assignment_id IS NOT NULL AND EXISTS (
              SELECT 1 FROM member_assignment_scope py WHERE py.member_id = sqlc.arg(principal_id) AND py.assignment_id = e.assignment_id)) )
ORDER BY e.seq
LIMIT sqlc.arg(max_rows);

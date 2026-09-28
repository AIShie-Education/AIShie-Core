-- name: InsertCourse :exec
INSERT INTO course (id, dept_id, term_id, code, section, title, description, status, created_by_actor_id, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'draft', $8, $9);

-- name: GetCourse :one
SELECT id, dept_id, term_id, code, section, title, description, status, created_at
FROM course WHERE id = $1;

-- name: CourseCodeTaken :one
SELECT EXISTS (SELECT 1 FROM course WHERE term_id = $1 AND code = $2 AND section = $3);

-- name: UpdateCourse :exec
UPDATE course SET title = $2, description = $3 WHERE id = $1;

-- name: LockCourseDetails :one
-- A course's title and description, held until a change to them is written:
-- NO KEY UPDATE, so that two changes take turns and neither puts back what
-- the other changed, while calls in the course, which take the row KEY
-- SHARE through their foreign keys, do not wait.
SELECT title, description FROM course WHERE id = $1 FOR NO KEY UPDATE;

-- name: SetCourseStatus :execrows
UPDATE course SET status = $2 WHERE id = $1 AND status <> $2;

-- name: ListCourses :many
-- Courses as their administrators see them. platform lists every course;
-- otherwise, those in the departments actor administers and beneath them.
-- dept_id is one department; within_dept_id a department and everything
-- beneath it.
WITH RECURSIVE mine (id) AS (
    SELECT da.dept_id FROM department_admin da
    WHERE da.actor_id = sqlc.arg(actor_id)::uuid AND da.removed_at IS NULL AND NOT sqlc.arg(platform)::bool
  UNION
    SELECT d.id FROM department d JOIN mine ON d.parent_id = mine.id
), within (id) AS (
    SELECT sqlc.narg(within_dept_id)::uuid WHERE sqlc.narg(within_dept_id)::uuid IS NOT NULL
  UNION
    SELECT d.id FROM department d JOIN within ON d.parent_id = within.id
)
SELECT c.id, c.dept_id, c.term_id, c.code, c.section, c.title, c.description, c.status, c.created_at
FROM course c
WHERE c.id > sqlc.arg(after)
  AND (sqlc.arg(platform)::bool OR c.dept_id IN (SELECT id FROM mine))
  AND (sqlc.narg(term_id)::uuid IS NULL OR c.term_id = sqlc.narg(term_id))
  AND (sqlc.narg(dept_id)::uuid IS NULL OR c.dept_id = sqlc.narg(dept_id))
  AND (sqlc.narg(within_dept_id)::uuid IS NULL OR c.dept_id IN (SELECT id FROM within))
ORDER BY c.id
LIMIT sqlc.arg(max_rows);

-- name: LockCourseDept :one
-- The department a course is in, held until the end of a move: NO KEY
-- UPDATE, as the move's own UPDATE takes it, so that two moves of one course
-- take turns and each sees where the other left it. A call in the course
-- takes KEY SHARE on the row through a foreign key, and does not wait.
SELECT dept_id FROM course WHERE id = $1 FOR NO KEY UPDATE;

-- name: SetCourseDept :exec
UPDATE course SET dept_id = $2 WHERE id = $1;

-- name: InsertComponent :exec
INSERT INTO grade_component (id, course_id, parent_id, name, weight, drop_lowest, points_possible, sort_order, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: UpdateComponent :exec
UPDATE grade_component
SET name = $2, weight = $3, drop_lowest = $4, points_possible = $5, sort_order = $6
WHERE id = $1;

-- name: SetComponentParent :exec
UPDATE grade_component SET parent_id = $2 WHERE id = $1;

-- name: GetComponentParent :one
SELECT parent_id FROM grade_component WHERE id = $1;

-- name: LockCourseComponents :exec
-- Taken before changing the tree's shape, so that two moves cannot each
-- check for a cycle and then create one between them.
SELECT pg_advisory_xact_lock(hashtextextended('component-tree:' || (sqlc.arg(course_id)::uuid)::text, 0));

-- name: ComponentHasGrades :one
SELECT EXISTS (SELECT 1 FROM grade WHERE component_id = $1 AND origin = 'entered');

-- name: ComponentHasLiveGrades :one
SELECT EXISTS (SELECT 1 FROM grade WHERE component_id = $1 AND origin = 'entered' AND superseded_by IS NULL);

-- name: ComponentSubtreeHasLiveGrades :one
-- Any entered grade, live, on the component, on a component beneath it, or
-- on a submission to an assignment beneath it.
WITH RECURSIVE sub(component_id) AS (
    SELECT gc.id FROM grade_component gc WHERE gc.id = sqlc.arg(component_id)
    UNION ALL
    SELECT c.id FROM grade_component c JOIN sub ON c.parent_id = sub.component_id
)
SELECT EXISTS (
    SELECT 1 FROM grade g
    WHERE g.origin = 'entered' AND g.superseded_by IS NULL
      AND (g.component_id IN (SELECT sub.component_id FROM sub)
           OR g.submission_id IN (SELECT s.id FROM submission s JOIN assignment a ON a.id = s.assignment_id
                                  WHERE a.component_id IN (SELECT sub.component_id FROM sub)))
);

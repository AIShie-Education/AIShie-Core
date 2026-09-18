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

-- name: SetCourseStatus :execrows
UPDATE course SET status = $2 WHERE id = $1 AND status <> $2;

-- name: ListCourses :many
SELECT id, dept_id, term_id, code, section, title, description, status, created_at
FROM course
WHERE id > sqlc.arg(after)
  AND (sqlc.narg(term_id)::uuid IS NULL OR term_id = sqlc.narg(term_id))
  AND (sqlc.narg(dept_id)::uuid IS NULL OR dept_id = sqlc.narg(dept_id))
ORDER BY id
LIMIT sqlc.arg(max_rows);

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

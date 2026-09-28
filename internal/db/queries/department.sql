-- The department tree and its administrators (docs/schema.md §2.9).
--
-- The tree is an adjacency list, walked with recursive CTEs. Every walk
-- stops at 16 levels, twice what the trigger department_tree_valid allows,
-- as a guard: a deeper tree cannot be written.

-- name: LockDepartmentTree :exec
-- The tree lock: taken before any change to the tree's shape (a department
-- created, or moved), so that two changes never each check the tree and then
-- together make a cycle or a tree too deep. The first key is "AIST"; the
-- event streams' is "AISE".
SELECT pg_advisory_xact_lock(1095324500, 0);

-- name: GetDepartment :one
SELECT id, name, parent_id, created_at FROM department WHERE id = $1;

-- name: DepartmentDepth :one
-- How many levels the department is down the tree: 1 at the top.
WITH RECURSIVE up (id, parent_id, n) AS (
    SELECT d.id, d.parent_id, 1 FROM department d WHERE d.id = sqlc.arg(dept_id)::uuid
  UNION ALL
    SELECT d.id, d.parent_id, up.n + 1
    FROM department d JOIN up ON d.id = up.parent_id
    WHERE up.n < 16
)
SELECT count(*)::int AS depth FROM up;

-- name: SubtreeHeight :one
-- How many levels the department and what is beneath it take up: 1 for one
-- with nothing beneath it.
WITH RECURSIVE down (id, n) AS (
    SELECT d.id, 1 FROM department d WHERE d.id = sqlc.arg(dept_id)::uuid
  UNION ALL
    SELECT d.id, down.n + 1 FROM department d JOIN down ON d.parent_id = down.id WHERE down.n < 16
)
SELECT coalesce(max(n), 0)::int AS height FROM down;

-- name: InSubtree :one
-- Whether other is the department itself or beneath it.
WITH RECURSIVE down (id, n) AS (
    SELECT d.id, 1 FROM department d WHERE d.id = sqlc.arg(dept_id)::uuid
  UNION ALL
    SELECT d.id, down.n + 1 FROM department d JOIN down ON d.parent_id = down.id WHERE down.n < 16
)
SELECT coalesce(bool_or(id = sqlc.arg(other_id)::uuid), false)::bool AS inside FROM down;

-- name: SiblingNameTaken :one
-- Whether another department under the same parent (at the top, for null)
-- has the name, in any case. id is the department being named, left out of
-- the comparison; a new one's id is not there yet.
SELECT EXISTS (
    SELECT 1 FROM department
    WHERE parent_id IS NOT DISTINCT FROM sqlc.narg(parent_id)::uuid
      AND lower(name) = lower(sqlc.arg(name)) AND id <> sqlc.arg(id)
);

-- name: RenameDepartment :exec
UPDATE department SET name = $2 WHERE id = $1;

-- name: SetDepartmentParent :exec
UPDATE department SET parent_id = $2 WHERE id = $1;

-- name: DepartmentTree :many
-- The departments as a tree: each before those beneath it, siblings by name.
-- depth is 1 at the top. With root_id, only that department and what is
-- beneath it, still at their depth in the whole tree.
WITH RECURSIVE tree (id, name, parent_id, depth, path, inside) AS (
    SELECT d.id, d.name, d.parent_id, 1, ARRAY[lower(d.name), d.id::text],
           sqlc.narg(root_id)::uuid IS NULL OR d.id = sqlc.narg(root_id)::uuid
    FROM department d WHERE d.parent_id IS NULL
  UNION ALL
    SELECT d.id, d.name, d.parent_id, tree.depth + 1, tree.path || ARRAY[lower(d.name), d.id::text],
           tree.inside OR d.id = sqlc.narg(root_id)::uuid
    FROM department d JOIN tree ON d.parent_id = tree.id
    WHERE tree.depth < 16
)
SELECT id, name, parent_id, depth::int AS depth
FROM tree
WHERE inside
ORDER BY path;

-- name: CourseCountsByDept :many
-- Courses directly in each department, archived ones included.
SELECT dept_id, count(*) AS courses FROM course GROUP BY dept_id;

-- name: AdminCountsByDept :many
-- Live appointments at each department itself.
SELECT dept_id, count(*) AS admins FROM department_admin WHERE removed_at IS NULL GROUP BY dept_id;

-- name: LiveAppointment :one
SELECT * FROM department_admin WHERE dept_id = $1 AND actor_id = $2 AND removed_at IS NULL;

-- name: InsertAppointment :exec
-- The partial unique index department_admin_one_live refuses a second live
-- appointment of one person at one department, two made at once included.
INSERT INTO department_admin (id, dept_id, actor_id, appointed_by_actor_id, appointed_at)
VALUES ($1, $2, $3, $4, $5);

-- name: EndAppointment :execrows
-- Zero rows: it had ended already. An Admin-gated write by the appointee
-- holds the appointment FOR SHARE (LockDepartmentAuthority), so this waits
-- for any such call in flight, and every call after it finds the appointment
-- ended.
UPDATE department_admin
SET removed_at = sqlc.arg(removed_at)::timestamptz, removed_by_actor_id = sqlc.arg(removed_by_actor_id)::uuid
WHERE id = sqlc.arg(id) AND removed_at IS NULL;

-- name: ListAppointments :many
-- The appointments at a department and, with inherited, at every department
-- above it, whose administrators administer it too: nearest first, then by
-- name. Ended ones only with include_removed. With the names of who holds
-- each, who made it and who ended it.
WITH RECURSIVE up (id, parent_id, depth) AS (
    SELECT d.id, d.parent_id, 0 FROM department d WHERE d.id = sqlc.arg(dept_id)::uuid
  UNION ALL
    SELECT d.id, d.parent_id, up.depth + 1
    FROM department d JOIN up ON d.id = up.parent_id
    WHERE sqlc.arg(inherited)::bool AND up.depth < 16
)
SELECT da.id, da.dept_id, dp.name AS dept_name, da.actor_id, a.display_name,
       da.appointed_by_actor_id, ab.display_name AS appointed_by_name, da.appointed_at,
       da.removed_at, da.removed_by_actor_id, rb.display_name AS removed_by_name
FROM up
JOIN department_admin da ON da.dept_id = up.id
JOIN department dp ON dp.id = da.dept_id
JOIN actor a ON a.id = da.actor_id
JOIN actor ab ON ab.id = da.appointed_by_actor_id
LEFT JOIN actor rb ON rb.id = da.removed_by_actor_id
WHERE sqlc.arg(include_removed)::bool OR da.removed_at IS NULL
ORDER BY up.depth, a.display_name, da.appointed_at, da.id;

-- name: MyAppointments :many
-- The departments an actor is appointed to administer now.
SELECT da.id, da.dept_id, d.name, da.appointed_at
FROM department_admin da
JOIN department d ON d.id = da.dept_id
WHERE da.actor_id = $1 AND da.removed_at IS NULL
ORDER BY d.name, d.id;

-- name: DepartmentAuthority :one
-- The appointment through which actor administers dept: one at dept, or the
-- nearest above it. No row: they do not.
WITH RECURSIVE up (id, parent_id, depth) AS (
    SELECT d.id, d.parent_id, 0 FROM department d WHERE d.id = sqlc.arg(dept_id)::uuid
  UNION ALL
    SELECT d.id, d.parent_id, up.depth + 1
    FROM department d JOIN up ON d.id = up.parent_id
    WHERE up.depth < 16
)
SELECT da.id AS appointment_id, da.dept_id
FROM up
JOIN department_admin da
  ON da.dept_id = up.id AND da.actor_id = sqlc.arg(actor_id)::uuid AND da.removed_at IS NULL
ORDER BY up.depth
LIMIT 1;

-- name: LockDepartmentAuthority :one
-- The same, for a write made by that authority. The appointment is held
-- FOR SHARE to the end of the call. Ending it is an UPDATE, which waits for
-- the call, or the call waits for it and then, reading the row again, finds
-- it ended. A department row is never locked here.
WITH RECURSIVE up (id, parent_id, depth) AS (
    SELECT d.id, d.parent_id, 0 FROM department d WHERE d.id = sqlc.arg(dept_id)::uuid
  UNION ALL
    SELECT d.id, d.parent_id, up.depth + 1
    FROM department d JOIN up ON d.id = up.parent_id
    WHERE up.depth < 16
)
SELECT da.id AS appointment_id, da.dept_id
FROM up
JOIN department_admin da
  ON da.dept_id = up.id AND da.actor_id = sqlc.arg(actor_id)::uuid AND da.removed_at IS NULL
ORDER BY up.depth
LIMIT 1
FOR SHARE OF da;

-- name: AdministeredDepartments :many
-- Every department actor covers. strictly: an appointment of theirs is
-- above it, so they may reshape it and staff it. appointed: one is at it.
WITH RECURSIVE down (id, below) AS (
    SELECT da.dept_id, 0 FROM department_admin da
    WHERE da.actor_id = sqlc.arg(actor_id)::uuid AND da.removed_at IS NULL
  UNION ALL
    SELECT d.id, down.below + 1 FROM department d JOIN down ON d.parent_id = down.id WHERE down.below < 16
)
SELECT id, bool_or(below > 0)::bool AS strictly, bool_or(below = 0)::bool AS appointed
FROM down GROUP BY id;

-- name: CourseAdministrators :many
-- Every live appointment that reaches the course: at its department or
-- above, nearest first, with the person's seat here if they hold one, which
-- they were given as anyone is: an appointment gives nobody a seat.
WITH RECURSIVE up (id, parent_id, depth) AS (
    SELECT d.id, d.parent_id, 0
    FROM course c JOIN department d ON d.id = c.dept_id WHERE c.id = sqlc.arg(course_id)::uuid
  UNION ALL
    SELECT d.id, d.parent_id, up.depth + 1 FROM department d JOIN up ON d.id = up.parent_id WHERE up.depth < 16
)
SELECT da.id AS appointment_id, da.actor_id, a.display_name, da.dept_id, dp.name AS dept_name, up.depth::int AS depth,
       da.appointed_at, m.id AS member_id
FROM up
JOIN department_admin da ON da.dept_id = up.id AND da.removed_at IS NULL
JOIN department dp ON dp.id = da.dept_id
JOIN actor a ON a.id = da.actor_id
LEFT JOIN course_member m
  ON m.course_id = sqlc.arg(course_id)::uuid AND m.actor_id = da.actor_id AND m.status <> 'removed'
ORDER BY up.depth, a.display_name, da.id;

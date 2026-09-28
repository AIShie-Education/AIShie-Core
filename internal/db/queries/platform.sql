-- name: InsertTerm :exec
INSERT INTO term (id, name, starts_on, ends_on) VALUES ($1, $2, $3, $4);

-- name: ListTerms :many
SELECT id, name, starts_on, ends_on FROM term ORDER BY starts_on DESC, id;

-- name: TermExists :one
SELECT EXISTS (SELECT 1 FROM term WHERE id = $1);

-- name: InsertDepartment :exec
-- A null parent_id is a department at the top of the tree. The trigger
-- department_tree_valid refuses one that would be too deep.
INSERT INTO department (id, name, parent_id, created_at) VALUES ($1, $2, $3, $4);

-- name: ListDepartments :many
SELECT id, name, parent_id, created_at FROM department ORDER BY name, id;

-- name: DepartmentExists :one
SELECT EXISTS (SELECT 1 FROM department WHERE id = $1);

-- name: SuspendActor :execrows
-- An administrator's suspension. It is made over an active actor, or over
-- one its owner has suspended, which it then takes over: from then on it is
-- the administrator's to lift, not the owner's.
UPDATE actor SET status = 'suspended', suspended_by_actor_id = sqlc.arg(suspended_by_actor_id)
WHERE id = sqlc.arg(id)
  AND (status = 'active'
       OR (status = 'suspended' AND suspended_by_actor_id IS NOT NULL AND suspended_by_actor_id = owner_actor_id));

-- name: ReactivateActor :execrows
UPDATE actor SET status = 'active', suspended_by_actor_id = NULL WHERE id = $1 AND status = 'suspended';

-- name: EmailTaken :one
SELECT EXISTS (SELECT 1 FROM actor WHERE lower(email) = lower($1));

-- name: EmailTakenByAnother :one
SELECT EXISTS (SELECT 1 FROM actor WHERE lower(email) = lower(sqlc.arg(email)) AND id <> sqlc.arg(id));

-- name: UpdateActor :exec
-- A null leaves the value as it is.
UPDATE actor
SET display_name = coalesce(sqlc.narg(display_name), display_name),
    email = coalesce(sqlc.narg(email), email)
WHERE id = sqlc.arg(id);

-- name: GetActorView :one
-- One actor as an administrator sees it: the row, and whether they can sign
-- in. At most one invitation is live (credential_one_live_invite), so the
-- join adds no row; it may have expired unused.
SELECT a.id, a.kind, a.display_name, a.email, a.status, a.platform_role, a.created_by_actor_id, a.created_at,
       EXISTS (SELECT 1 FROM credential p WHERE p.actor_id = a.id AND p.kind = 'password' AND p.revoked_at IS NULL) AS has_password,
       EXISTS (SELECT 1 FROM credential s WHERE s.actor_id = a.id AND s.kind = 'sso' AND s.revoked_at IS NULL) AS has_sso,
       i.expires_at AS invite_expires_at, a.owner_actor_id, o.display_name AS owner_name, a.suspended_by_actor_id
FROM actor a
LEFT JOIN credential i ON i.actor_id = a.id AND i.kind = 'invite' AND i.revoked_at IS NULL
LEFT JOIN actor o ON o.id = a.owner_actor_id
WHERE a.id = $1;

-- name: ListActors :many
-- Everyone registered, as GetActorView sees them: people and agents, not the
-- system actor, which nobody registers or manages. The search is a piece of
-- the name or of the email, in any case, taken as it is: strpos has no
-- wildcards to escape.
SELECT a.id, a.kind, a.display_name, a.email, a.status, a.platform_role, a.created_by_actor_id, a.created_at,
       EXISTS (SELECT 1 FROM credential p WHERE p.actor_id = a.id AND p.kind = 'password' AND p.revoked_at IS NULL) AS has_password,
       EXISTS (SELECT 1 FROM credential s WHERE s.actor_id = a.id AND s.kind = 'sso' AND s.revoked_at IS NULL) AS has_sso,
       i.expires_at AS invite_expires_at, a.owner_actor_id, o.display_name AS owner_name, a.suspended_by_actor_id
FROM actor a
LEFT JOIN credential i ON i.actor_id = a.id AND i.kind = 'invite' AND i.revoked_at IS NULL
LEFT JOIN actor o ON o.id = a.owner_actor_id
WHERE a.id > sqlc.arg(after) AND a.kind <> 'system'
  AND (sqlc.narg(kind)::text IS NULL OR a.kind = sqlc.narg(kind))
  AND (sqlc.narg(status)::text IS NULL OR a.status = sqlc.narg(status))
  AND (sqlc.narg(owner_actor_id)::uuid IS NULL OR a.owner_actor_id = sqlc.narg(owner_actor_id))
  AND (sqlc.narg(search)::text IS NULL
       OR strpos(lower(a.display_name), lower(sqlc.narg(search))) > 0
       OR strpos(lower(coalesce(a.email, '')), lower(sqlc.narg(search))) > 0)
ORDER BY a.id
LIMIT sqlc.arg(max_rows);

-- name: GetPreset :one
SELECT * FROM permission_preset WHERE id = $1;

-- name: GetBuiltinPresetByName :one
SELECT * FROM permission_preset WHERE name = $1 AND dept_id IS NULL;

-- name: GetDeptPresetByName :one
SELECT * FROM permission_preset WHERE name = $1 AND dept_id = $2;

-- name: ListPresets :many
-- Built-ins, plus one department's own when a department is named.
SELECT * FROM permission_preset
WHERE dept_id IS NULL OR dept_id = sqlc.narg(dept_id)
ORDER BY dept_id NULLS FIRST, name;

-- name: InsertPreset :exec
INSERT INTO permission_preset (
    id, dept_id, name, description, role, student_scope, assignment_scope,
    perm_document_read, perm_document_read_draft, perm_document_write, perm_rubric_read,
    perm_assignment_write, perm_submission_read, perm_submission_write, perm_grade_read,
    perm_grade_submit, perm_grade_post, perm_member_read, perm_member_manage, perm_action_decide,
    perm_agent_delegate, perm_conversation_ask, perm_conversation_answer,
    created_by_actor_id, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25);

-- name: UpdatePreset :execrows
-- Built-ins (dept_id null) are policy shipped with the system; only a
-- department's own presets are edited here.
UPDATE permission_preset SET
    description = $2, role = $3, student_scope = $4, assignment_scope = $5,
    perm_document_read = $6, perm_document_read_draft = $7, perm_document_write = $8, perm_rubric_read = $9,
    perm_assignment_write = $10, perm_submission_read = $11, perm_submission_write = $12, perm_grade_read = $13,
    perm_grade_submit = $14, perm_grade_post = $15, perm_member_read = $16, perm_member_manage = $17,
    perm_action_decide = $18, perm_agent_delegate = $19, perm_conversation_ask = $20, perm_conversation_answer = $21
WHERE id = $1 AND dept_id IS NOT NULL;

-- name: CountBuiltinPresets :one
SELECT count(*) FROM permission_preset WHERE dept_id IS NULL;

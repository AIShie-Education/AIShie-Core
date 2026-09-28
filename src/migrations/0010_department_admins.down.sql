-- AIshiteru Core — migration 0010 (down)
-- Reverts 0010_department_admins.up.sql.
--
-- Lost: every appointment, the shape of the tree (every department is at the
-- top again, so two departments of one name under different parents become
-- namesakes at the top), and the capacity recorded on actions
-- (action.authority). Kept: the departments, and the actions and events
-- themselves.

BEGIN;

SET LOCAL lock_timeout = '10s';

ALTER TABLE action
    DROP CONSTRAINT IF EXISTS action_authority_dept_fk,
    DROP CONSTRAINT IF EXISTS action_authority_valid,
    DROP COLUMN IF EXISTS authority_dept_id,
    DROP COLUMN IF EXISTS authority;

DROP TABLE IF EXISTS department_admin;
DROP FUNCTION IF EXISTS department_admin_check();

DROP TRIGGER IF EXISTS department_tree_valid ON department;
DROP FUNCTION IF EXISTS department_check_tree();
DROP INDEX IF EXISTS department_parent_idx;
ALTER TABLE department DROP CONSTRAINT IF EXISTS department_not_own_parent, DROP COLUMN IF EXISTS parent_id;

COMMIT;

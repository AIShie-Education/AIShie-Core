-- AIshie Core — after 0010_department_admins.down.sql, in `make db-test-sql`
--
-- The appointments, the tree and the capacity on actions are gone; the
-- departments stay, each at the top now, and so does the action, as history.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'department_admin') THEN
        RAISE EXCEPTION 'FAIL  0010 down: department_admin is still there';
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public'
               AND (table_name, column_name) IN (('department', 'parent_id'), ('action', 'authority'), ('action', 'authority_dept_id'))) THEN
        RAISE EXCEPTION 'FAIL  0010 down: a column it added is still there';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
               WHERE n.nspname = 'public' AND p.proname IN ('department_check_tree', 'department_admin_check')) THEN
        RAISE EXCEPTION 'FAIL  0010 down: a function it added is still there';
    END IF;
    IF (SELECT count(*) FROM department WHERE id IN ('00000000-0000-0000-0010-000000000021',
            '00000000-0000-0000-0010-000000000022', '00000000-0000-0000-0010-000000000023')) <> 3 THEN
        RAISE EXCEPTION 'FAIL  0010 down: a department of the tree did not stay';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM action WHERE id = '00000000-0000-0000-0010-0000000000b1' AND status = 'executed') THEN
        RAISE EXCEPTION 'FAIL  0010 down: the action made by a department''s administrator did not stay';
    END IF;
END $chk$;
\echo 'PASS  0010 down with a tree and its administrators present'

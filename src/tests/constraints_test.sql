-- AIshiteru Core — database-enforced rule tests
--
-- Run against a throwaway database that has the migrations applied. Every
-- statement runs inside one transaction that is rolled back at the end, but
-- the fixtures use fixed ids, so do not point this at a database with data.
--
--   createdb aishiteru_test
--   psql -v ON_ERROR_STOP=1 -d aishiteru_test -f migrations/0001_init.up.sql
--   psql -X -d aishiteru_test -f tests/constraints_test.sql
--   dropdb aishiteru_test
--
-- Each check prints PASS; the first failure stops the run with FAIL.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

CREATE FUNCTION pg_temp.ok(label text, stmt text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    EXECUTE stmt;
    RAISE NOTICE 'PASS  %', label;
END $$;

CREATE FUNCTION pg_temp.fails(label text, expected text, stmt text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    got text;
    msg text;
BEGIN
    BEGIN
        EXECUTE stmt;
        -- Force deferred checks so a violation cannot hide until COMMIT.
        SET CONSTRAINTS ALL IMMEDIATE;
        SET CONSTRAINTS ALL DEFERRED;
    EXCEPTION WHEN OTHERS THEN
        got := SQLSTATE;
        msg := SQLERRM;
    END;
    IF got IS NULL THEN
        RAISE EXCEPTION 'FAIL  % : expected SQLSTATE % but the statement succeeded', label, expected;
    ELSIF got <> expected THEN
        RAISE EXCEPTION 'FAIL  % : expected SQLSTATE % but got % (%)', label, expected, got, msg;
    END IF;
    RAISE NOTICE 'PASS  % [% %]', label, got, msg;
END $$;

\o /dev/null
-- Fixtures ------------------------------------------------------------------
-- ids: 11 term · 21 dept · 3x actors · 4x courses · 5x members · 6x components
--      7x assignments · ex documents · fx versions · ax submissions
--      bx actions · dx grades
INSERT INTO term (id, name, starts_on, ends_on)
VALUES ('00000000-0000-0000-0000-000000000011', '2026 Autumn', '2026-09-01', '2026-12-20');
INSERT INTO department (id, name) VALUES ('00000000-0000-0000-0000-000000000021', 'Computing');
INSERT INTO actor (id, kind, display_name, platform_role, created_by_actor_id) VALUES
    ('00000000-0000-0000-0000-000000000031', 'human',  'root',   'root',  NULL),
    ('00000000-0000-0000-0000-000000000032', 'human',  'admin',  'admin', '00000000-0000-0000-0000-000000000031'),
    ('00000000-0000-0000-0000-000000000033', 'system', 'system', NULL,    '00000000-0000-0000-0000-000000000031'),
    ('00000000-0000-0000-0000-000000000034', 'human',  'Sato',   NULL,    '00000000-0000-0000-0000-000000000033'),
    ('00000000-0000-0000-0000-000000000035', 'human',  'Yuki',   NULL,    '00000000-0000-0000-0000-000000000033'),
    ('00000000-0000-0000-0000-000000000036', 'agent',  'grader', NULL,    '00000000-0000-0000-0000-000000000034'),
    ('00000000-0000-0000-0000-000000000037', 'human',  'Ken',    NULL,    '00000000-0000-0000-0000-000000000033');
-- 91 built-in preset (name chosen not to collide with src/seed/presets.sql)
INSERT INTO permission_preset (id, name, role, student_scope, assignment_scope, perm_document_read, perm_grade_submit)
VALUES ('00000000-0000-0000-0000-000000000091', 'test-grader', 'assistant', 'all', 'listed', 'autonomous', 'confirm_required');
INSERT INTO course (id, dept_id, term_id, code, section, title, created_by_actor_id) VALUES
    ('00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000021', '00000000-0000-0000-0000-000000000011', 'CS101', 'A', 'Intro', '00000000-0000-0000-0000-000000000032'),
    ('00000000-0000-0000-0000-000000000042', '00000000-0000-0000-0000-000000000021', '00000000-0000-0000-0000-000000000011', 'CS101', 'B', 'Intro', '00000000-0000-0000-0000-000000000032');
-- 51 Sato instructor@A · 52 Yuki student@A · 53 grader agent@A · 54 Ken student@B
-- 55 Sato instructor@B · 58 Ken student@A
INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope, perm_grade_submit, perm_action_decide) VALUES
    ('00000000-0000-0000-0000-000000000051', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000034', 'instructor', '00000000-0000-0000-0000-000000000032', 'all',    'all',    'autonomous',       'autonomous'),
    ('00000000-0000-0000-0000-000000000052', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000035', 'student',    '00000000-0000-0000-0000-000000000034', 'listed', 'all',    'denied',           'denied'),
    ('00000000-0000-0000-0000-000000000053', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000036', 'assistant',  '00000000-0000-0000-0000-000000000034', 'all',    'listed', 'confirm_required', 'denied'),
    ('00000000-0000-0000-0000-000000000054', '00000000-0000-0000-0000-000000000042', '00000000-0000-0000-0000-000000000037', 'student',    '00000000-0000-0000-0000-000000000034', 'listed', 'all',    'denied',           'denied'),
    ('00000000-0000-0000-0000-000000000055', '00000000-0000-0000-0000-000000000042', '00000000-0000-0000-0000-000000000034', 'instructor', '00000000-0000-0000-0000-000000000032', 'all',    'all',    'autonomous',       'autonomous'),
    ('00000000-0000-0000-0000-000000000058', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000037', 'student',    '00000000-0000-0000-0000-000000000034', 'listed', 'all',    'denied',           'denied');
INSERT INTO member_student_scope VALUES
    ('00000000-0000-0000-0000-000000000052', '00000000-0000-0000-0000-000000000052'),
    ('00000000-0000-0000-0000-000000000058', '00000000-0000-0000-0000-000000000058');
-- 61 Total(A) · 62 Assignments(A) · 64 Midterm(A) · 63 Total(B)
INSERT INTO grade_component (id, course_id, parent_id, name, weight, points_possible) VALUES
    ('00000000-0000-0000-0000-000000000061', '00000000-0000-0000-0000-000000000041', NULL, 'Total', 1, NULL),
    ('00000000-0000-0000-0000-000000000062', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000061', 'Assignments', 40, NULL),
    ('00000000-0000-0000-0000-000000000064', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000061', 'Midterm', 30, 100),
    ('00000000-0000-0000-0000-000000000063', '00000000-0000-0000-0000-000000000042', NULL, 'Total', 1, NULL);
INSERT INTO assignment (id, course_id, component_id, title, points_possible) VALUES
    ('00000000-0000-0000-0000-000000000071', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000062', 'HW1', 10),
    ('00000000-0000-0000-0000-000000000072', '00000000-0000-0000-0000-000000000042', NULL, 'HW1', 10);
INSERT INTO document (id, course_id, kind, title) VALUES
    ('00000000-0000-0000-0000-0000000000e1', '00000000-0000-0000-0000-000000000041', 'material', 'Lecture 1'),
    ('00000000-0000-0000-0000-0000000000e2', '00000000-0000-0000-0000-000000000041', 'material', 'Lecture 2');

-- Actors and credentials -----------------------------------------------------
SELECT pg_temp.fails('platform_role must be root or admin', '23514', $q$
    INSERT INTO actor (kind, display_name, platform_role, created_by_actor_id)
    VALUES ('human', 'x', 'superuser', '00000000-0000-0000-0000-000000000031') $q$);
SELECT pg_temp.fails('email is unique regardless of case', '23505', $q$
    INSERT INTO actor (kind, display_name, email, created_by_actor_id)
    VALUES ('human', 'a', 'Yuki@example.edu', '00000000-0000-0000-0000-000000000033');
    INSERT INTO actor (kind, display_name, email, created_by_actor_id)
    VALUES ('human', 'b', 'yuki@EXAMPLE.edu', '00000000-0000-0000-0000-000000000033') $q$);
SELECT pg_temp.ok('sso credential with provider and subject', $q$
    INSERT INTO credential (actor_id, kind, provider, subject)
    VALUES ('00000000-0000-0000-0000-000000000035', 'sso', 'polyu-adfs', 'yuki@connect.polyu.hk') $q$);
SELECT pg_temp.fails('sso credential needs provider and subject', '23514', $q$
    INSERT INTO credential (actor_id, kind) VALUES ('00000000-0000-0000-0000-000000000035', 'sso') $q$);
SELECT pg_temp.fails('one sso identity cannot map to two actors', '23505', $q$
    INSERT INTO credential (actor_id, kind, provider, subject)
    VALUES ('00000000-0000-0000-0000-000000000037', 'sso', 'polyu-adfs', 'yuki@connect.polyu.hk') $q$);
SELECT pg_temp.fails('password credential needs a hash', '23514', $q$
    INSERT INTO credential (actor_id, kind) VALUES ('00000000-0000-0000-0000-000000000034', 'password') $q$);
SELECT pg_temp.fails('api token needs a lookup prefix', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash) VALUES ('00000000-0000-0000-0000-000000000036', 'api_token', 'h') $q$);

-- Courses and membership -----------------------------------------------------
SELECT pg_temp.fails('same term, code and section twice', '23505', $q$
    INSERT INTO course (dept_id, term_id, code, section, title, created_by_actor_id)
    VALUES ('00000000-0000-0000-0000-000000000021', '00000000-0000-0000-0000-000000000011', 'CS101', 'A', 'dup', '00000000-0000-0000-0000-000000000032') $q$);
SELECT pg_temp.ok('a second course gives the same actor a second membership', $q$
    INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope)
    VALUES ('00000000-0000-0000-0000-000000000056', '00000000-0000-0000-0000-000000000042',
            '00000000-0000-0000-0000-000000000035', 'student', '00000000-0000-0000-0000-000000000034', 'listed', 'all') $q$);
SELECT pg_temp.fails('one live membership per actor per course', '23505', $q$
    INSERT INTO course_member (course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope)
    VALUES ('00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000035', 'student',
            '00000000-0000-0000-0000-000000000034', 'listed', 'all') $q$);
SELECT pg_temp.ok('a removed membership can be replaced by a new row', $q$
    UPDATE course_member SET status = 'removed' WHERE id = '00000000-0000-0000-0000-000000000056';
    INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope)
    VALUES ('00000000-0000-0000-0000-000000000057', '00000000-0000-0000-0000-000000000042',
            '00000000-0000-0000-0000-000000000035', 'student', '00000000-0000-0000-0000-000000000034', 'listed', 'all') $q$);
SELECT pg_temp.fails('scope must be all or listed', '23514', $q$
    INSERT INTO course_member (course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope)
    VALUES ('00000000-0000-0000-0000-000000000042', '00000000-0000-0000-0000-000000000036', 'assistant',
            '00000000-0000-0000-0000-000000000034', 'some', 'all') $q$);
SELECT pg_temp.fails('unknown role is rejected', '23514', $q$
    INSERT INTO course_member (course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope)
    VALUES ('00000000-0000-0000-0000-000000000042', '00000000-0000-0000-0000-000000000036', 'admin',
            '00000000-0000-0000-0000-000000000034', 'all', 'all') $q$);
SELECT pg_temp.fails('permissions only take autonomy levels', '22P02', $q$
    UPDATE course_member SET perm_grade_submit = 'maybe' WHERE id = '00000000-0000-0000-0000-000000000053' $q$);

-- Presets --------------------------------------------------------------------
SELECT pg_temp.ok('preset and member permission columns are identical', $q$
    DO $chk$
    DECLARE a text[]; b text[];
    BEGIN
        SELECT array_agg(column_name::text || ' ' || udt_name ORDER BY column_name) INTO a
        FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'course_member' AND column_name LIKE 'perm\_%';
        SELECT array_agg(column_name::text || ' ' || udt_name ORDER BY column_name) INTO b
        FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'permission_preset' AND column_name LIKE 'perm\_%';
        IF a IS NULL OR a IS DISTINCT FROM b THEN
            RAISE EXCEPTION 'perm_* columns differ: course_member % vs permission_preset %', a, b;
        END IF;
    END $chk$ $q$);
SELECT pg_temp.fails('built-in preset names are unique', '23505', $q$
    INSERT INTO permission_preset (name, role, student_scope, assignment_scope)
    VALUES ('test-grader', 'assistant', 'all', 'all') $q$);
SELECT pg_temp.ok('a department may define its own preset under the same name', $q$
    INSERT INTO permission_preset (id, dept_id, name, role, student_scope, assignment_scope, created_by_actor_id)
    VALUES ('00000000-0000-0000-0000-000000000092', '00000000-0000-0000-0000-000000000021', 'test-grader',
            'assistant', 'all', 'all', '00000000-0000-0000-0000-000000000032') $q$);
SELECT pg_temp.fails('department preset names are unique within the department', '23505', $q$
    INSERT INTO permission_preset (dept_id, name, role, student_scope, assignment_scope)
    VALUES ('00000000-0000-0000-0000-000000000021', 'test-grader', 'assistant', 'all', 'all') $q$);
SELECT pg_temp.fails('preset role must be valid', '23514', $q$
    INSERT INTO permission_preset (name, role, student_scope, assignment_scope)
    VALUES ('bad', 'admin', 'all', 'all') $q$);
SELECT pg_temp.ok('applying a preset copies role, scope and every permission', $q$
    INSERT INTO course_member (id, course_id, actor_id, added_by_actor_id, preset_id,
                               role, student_scope, assignment_scope,
                               perm_document_read, perm_document_read_draft, perm_document_write, perm_rubric_read,
                               perm_assignment_write, perm_submission_read, perm_submission_write, perm_grade_read,
                               perm_grade_submit, perm_grade_post, perm_member_read, perm_member_manage, perm_action_decide)
    SELECT '00000000-0000-0000-0000-000000000059', '00000000-0000-0000-0000-000000000042',
           '00000000-0000-0000-0000-000000000036', '00000000-0000-0000-0000-000000000034', id,
           role, student_scope, assignment_scope,
           perm_document_read, perm_document_read_draft, perm_document_write, perm_rubric_read,
           perm_assignment_write, perm_submission_read, perm_submission_write, perm_grade_read,
           perm_grade_submit, perm_grade_post, perm_member_read, perm_member_manage, perm_action_decide
    FROM permission_preset WHERE id = '00000000-0000-0000-0000-000000000091';
    DO $chk$
    BEGIN
        IF NOT EXISTS (
            SELECT 1 FROM course_member
            WHERE id = '00000000-0000-0000-0000-000000000059'
              AND role = 'assistant' AND assignment_scope = 'listed'
              AND perm_grade_submit = 'confirm_required'
              AND perm_document_read = 'autonomous' AND perm_grade_post = 'denied')
        THEN RAISE EXCEPTION 'preset values were not copied';
        END IF;
    END $chk$ $q$);
SELECT pg_temp.fails('preset_id must name an existing preset', '23503', $q$
    INSERT INTO course_member (course_id, actor_id, added_by_actor_id, preset_id, role, student_scope, assignment_scope)
    VALUES ('00000000-0000-0000-0000-000000000042', '00000000-0000-0000-0000-000000000032',
            '00000000-0000-0000-0000-000000000034', '00000000-0000-0000-0000-000000000099', 'student', 'listed', 'all') $q$);

-- Grading scheme -------------------------------------------------------------
SELECT pg_temp.fails('one root component per course', '23505', $q$
    INSERT INTO grade_component (course_id, name) VALUES ('00000000-0000-0000-0000-000000000041', 'Another total') $q$);
SELECT pg_temp.fails('component cannot be its own parent', '23514', $q$
    UPDATE grade_component SET parent_id = id WHERE id = '00000000-0000-0000-0000-000000000064' $q$);

-- Documents and versions -----------------------------------------------------
SELECT pg_temp.ok('text-only version', $q$
    INSERT INTO document_version (id, document_id, seq, body_md, author_member_id)
    VALUES ('00000000-0000-0000-0000-0000000000f1', '00000000-0000-0000-0000-0000000000e1', 1, 'Lecture text',
            '00000000-0000-0000-0000-000000000051') $q$);
SELECT pg_temp.ok('file-only version', $q$
    INSERT INTO document_version (id, document_id, seq, storage_key, content_type, byte_size, author_member_id)
    VALUES ('00000000-0000-0000-0000-0000000000f2', '00000000-0000-0000-0000-0000000000e2', 1, 'k/lecture2.pdf',
            'application/pdf', 1024, '00000000-0000-0000-0000-000000000051') $q$);
SELECT pg_temp.fails('version needs text or a file', '23514', $q$
    INSERT INTO document_version (document_id, seq, author_member_id)
    VALUES ('00000000-0000-0000-0000-0000000000e1', 2, '00000000-0000-0000-0000-000000000051') $q$);
SELECT pg_temp.fails('file needs content type and size', '23514', $q$
    INSERT INTO document_version (document_id, seq, storage_key, author_member_id)
    VALUES ('00000000-0000-0000-0000-0000000000e1', 2, 'k/x', '00000000-0000-0000-0000-000000000051') $q$);
SELECT pg_temp.ok('published pointer moves to own version', $q$
    UPDATE document SET published_version_id = '00000000-0000-0000-0000-0000000000f1'
    WHERE id = '00000000-0000-0000-0000-0000000000e1' $q$);
SELECT pg_temp.fails('published pointer cannot name another document''s version', '23503', $q$
    UPDATE document SET published_version_id = '00000000-0000-0000-0000-0000000000f2'
    WHERE id = '00000000-0000-0000-0000-0000000000e1' $q$);
SELECT pg_temp.fails('versions are append-only', '23001', $q$
    UPDATE document_version SET body_md = 'rewritten' WHERE id = '00000000-0000-0000-0000-0000000000f1' $q$);
SELECT pg_temp.fails('submission document needs its submission', '23514', $q$
    INSERT INTO document (course_id, kind, title) VALUES ('00000000-0000-0000-0000-000000000041', 'submission', 'essay.pdf') $q$);

-- Submissions ----------------------------------------------------------------
SELECT pg_temp.ok('student submits to own course', $q$
    INSERT INTO submission (id, assignment_id, course_id, student_member_id, body)
    VALUES ('00000000-0000-0000-0000-0000000000a1', '00000000-0000-0000-0000-000000000071',
            '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000052', 'my essay') $q$);
SELECT pg_temp.fails('member of another course cannot submit here', '23503', $q$
    INSERT INTO submission (assignment_id, course_id, student_member_id, body)
    VALUES ('00000000-0000-0000-0000-000000000071', '00000000-0000-0000-0000-000000000041',
            '00000000-0000-0000-0000-000000000054', 'x') $q$);
SELECT pg_temp.fails('assignment from another course cannot be used', '23503', $q$
    INSERT INTO submission (assignment_id, course_id, student_member_id, body)
    VALUES ('00000000-0000-0000-0000-000000000072', '00000000-0000-0000-0000-000000000041',
            '00000000-0000-0000-0000-000000000052', 'x') $q$);
SELECT pg_temp.ok('submitted file is a document owned by the submission', $q$
    INSERT INTO document (course_id, kind, title, submission_id)
    VALUES ('00000000-0000-0000-0000-000000000041', 'submission', 'essay.pdf', '00000000-0000-0000-0000-0000000000a1') $q$);
SELECT pg_temp.fails('material cannot carry a submission owner', '23514', $q$
    INSERT INTO document (course_id, kind, title, submission_id)
    VALUES ('00000000-0000-0000-0000-000000000041', 'material', 'x', '00000000-0000-0000-0000-0000000000a1') $q$);

-- Submission freeze ----------------------------------------------------------
SELECT pg_temp.ok('draft body can be edited', $q$
    UPDATE submission SET body = 'my essay, revised' WHERE id = '00000000-0000-0000-0000-0000000000a1' $q$);
SELECT pg_temp.ok('draft can be submitted', $q$
    UPDATE submission SET state = 'submitted', submitted_at = now() WHERE id = '00000000-0000-0000-0000-0000000000a1' $q$);
SELECT pg_temp.fails('submitted body cannot be edited', '23001', $q$
    UPDATE submission SET body = 'sneaky edit' WHERE id = '00000000-0000-0000-0000-0000000000a1' $q$);
SELECT pg_temp.fails('submitted_at cannot be moved', '23001', $q$
    UPDATE submission SET submitted_at = submitted_at - interval '1 day' WHERE id = '00000000-0000-0000-0000-0000000000a1' $q$);
SELECT pg_temp.ok('submitted can be corrected to late', $q$
    UPDATE submission SET state = 'late' WHERE id = '00000000-0000-0000-0000-0000000000a1' $q$);
SELECT pg_temp.ok('late can be corrected back to submitted', $q$
    UPDATE submission SET state = 'submitted' WHERE id = '00000000-0000-0000-0000-0000000000a1' $q$);
SELECT pg_temp.ok('no-op update of a submitted row is allowed', $q$
    UPDATE submission SET state = state WHERE id = '00000000-0000-0000-0000-0000000000a1' $q$);
SELECT pg_temp.fails('submitted cannot go back to draft', '23001', $q$
    UPDATE submission SET state = 'draft' WHERE id = '00000000-0000-0000-0000-0000000000a1' $q$);
SELECT pg_temp.fails('submitted cannot be deleted', '23001', $q$
    DELETE FROM submission WHERE id = '00000000-0000-0000-0000-0000000000a1' $q$);
SELECT pg_temp.ok('submission with NULL fields can still flip to late', $q$
    INSERT INTO submission (id, assignment_id, course_id, student_member_id, attempt, state, submitted_at)
    VALUES ('00000000-0000-0000-0000-0000000000a2', '00000000-0000-0000-0000-000000000071',
            '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000052', 2, 'submitted', now());
    UPDATE submission SET state = 'late' WHERE id = '00000000-0000-0000-0000-0000000000a2' $q$);
SELECT pg_temp.ok('draft can be deleted', $q$
    INSERT INTO submission (id, assignment_id, course_id, student_member_id, attempt)
    VALUES ('00000000-0000-0000-0000-0000000000a3', '00000000-0000-0000-0000-000000000071',
            '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000052', 3);
    DELETE FROM submission WHERE id = '00000000-0000-0000-0000-0000000000a3' $q$);

-- Actions --------------------------------------------------------------------
SELECT pg_temp.fails('member cannot approve own proposal', '23514', $q$
    INSERT INTO action (actor_id, course_id, member_id, action_type, target_type, target_id, idempotency_key,
                        authz_result, status, decided_by_member_id, decided_at)
    VALUES ('00000000-0000-0000-0000-000000000036', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000053',
            'grade.submit', 'submission', '00000000-0000-0000-0000-0000000000a1', 'k-self', 'confirm_required', 'approved',
            '00000000-0000-0000-0000-000000000053', now()) $q$);
SELECT pg_temp.ok('instructor approves and the action executes', $q$
    INSERT INTO action (id, actor_id, course_id, member_id, action_type, target_type, target_id, idempotency_key,
                        authz_result, status, decided_by_member_id, decided_at, executed_at)
    VALUES ('00000000-0000-0000-0000-0000000000b1', '00000000-0000-0000-0000-000000000036', '00000000-0000-0000-0000-000000000041',
            '00000000-0000-0000-0000-000000000053', 'grade.submit', 'submission', '00000000-0000-0000-0000-0000000000a1', 'k-1',
            'confirm_required', 'executed', '00000000-0000-0000-0000-000000000051', now(), now()) $q$);
SELECT pg_temp.fails('member cannot review own action', '23514', $q$
    INSERT INTO action (actor_id, course_id, member_id, action_type, target_type, idempotency_key, authz_result, status,
                        executed_at, review_state, reviewed_by_member_id, reviewed_at)
    VALUES ('00000000-0000-0000-0000-000000000036', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000053',
            'grade.submit', 'submission', 'k-review', 'pending_review', 'executed', now(), 'reviewed',
            '00000000-0000-0000-0000-000000000053', now()) $q$);
SELECT pg_temp.fails('idempotency key cannot repeat for one actor', '23505', $q$
    INSERT INTO action (actor_id, action_type, target_type, idempotency_key, authz_result, status)
    VALUES ('00000000-0000-0000-0000-000000000036', 'grade.submit', 'submission', 'k-1', 'denied', 'denied') $q$);
SELECT pg_temp.ok('same key is fine for a different actor', $q$
    INSERT INTO action (actor_id, action_type, target_type, idempotency_key, authz_result, status)
    VALUES ('00000000-0000-0000-0000-000000000034', 'grade.submit', 'submission', 'k-1', 'denied', 'denied') $q$);
SELECT pg_temp.fails('unknown status is rejected', '23514', $q$
    INSERT INTO action (actor_id, action_type, target_type, idempotency_key, authz_result, status)
    VALUES ('00000000-0000-0000-0000-000000000034', 'grade.submit', 'submission', 'k-2', 'autonomous', 'done') $q$);

-- Grades ---------------------------------------------------------------------
SELECT pg_temp.fails('grade requires an action', '23502', $q$
    INSERT INTO grade (student_member_id, submission_id, origin, score, grader_member_id)
    VALUES ('00000000-0000-0000-0000-000000000052', '00000000-0000-0000-0000-0000000000a1', 'entered', 8,
            '00000000-0000-0000-0000-000000000053') $q$);
SELECT pg_temp.fails('grade needs a target', '23514', $q$
    INSERT INTO grade (student_member_id, origin, score, grader_member_id, created_by_action_id)
    VALUES ('00000000-0000-0000-0000-000000000052', 'entered', 8, '00000000-0000-0000-0000-000000000053',
            '00000000-0000-0000-0000-0000000000b1') $q$);
SELECT pg_temp.fails('grade cannot target both a submission and a component', '23514', $q$
    INSERT INTO grade (student_member_id, submission_id, component_id, origin, score, grader_member_id, created_by_action_id)
    VALUES ('00000000-0000-0000-0000-000000000052', '00000000-0000-0000-0000-0000000000a1', '00000000-0000-0000-0000-000000000064',
            'entered', 8, '00000000-0000-0000-0000-000000000053', '00000000-0000-0000-0000-0000000000b1') $q$);
SELECT pg_temp.fails('submission grade must be for the submitting student', '23503', $q$
    INSERT INTO grade (student_member_id, submission_id, origin, score, grader_member_id, created_by_action_id)
    VALUES ('00000000-0000-0000-0000-000000000058', '00000000-0000-0000-0000-0000000000a1', 'entered', 8,
            '00000000-0000-0000-0000-000000000053', '00000000-0000-0000-0000-0000000000b1') $q$);
SELECT pg_temp.fails('posted_at needs posted_by', '23514', $q$
    INSERT INTO grade (student_member_id, submission_id, origin, score, grader_member_id, created_by_action_id, posted_at)
    VALUES ('00000000-0000-0000-0000-000000000052', '00000000-0000-0000-0000-0000000000a1', 'entered', 8,
            '00000000-0000-0000-0000-000000000053', '00000000-0000-0000-0000-0000000000b1', now()) $q$);
SELECT pg_temp.fails('origin must be entered or computed', '23514', $q$
    INSERT INTO grade (student_member_id, submission_id, origin, score, grader_member_id, created_by_action_id)
    VALUES ('00000000-0000-0000-0000-000000000052', '00000000-0000-0000-0000-0000000000a1', 'guessed', 8,
            '00000000-0000-0000-0000-000000000053', '00000000-0000-0000-0000-0000000000b1') $q$);
SELECT pg_temp.ok('posted submission grade', $q$
    INSERT INTO grade (id, student_member_id, submission_id, origin, score, grader_member_id, created_by_action_id,
                       posted_at, posted_by_member_id)
    VALUES ('00000000-0000-0000-0000-0000000000d1', '00000000-0000-0000-0000-000000000052', '00000000-0000-0000-0000-0000000000a1',
            'entered', 8, '00000000-0000-0000-0000-000000000053', '00000000-0000-0000-0000-0000000000b1', now(),
            '00000000-0000-0000-0000-000000000051') $q$);
SELECT pg_temp.fails('second live grade for the same submission', '23505', $q$
    INSERT INTO grade (student_member_id, submission_id, origin, score, grader_member_id, created_by_action_id,
                       posted_at, posted_by_member_id)
    VALUES ('00000000-0000-0000-0000-000000000052', '00000000-0000-0000-0000-0000000000a1', 'entered', 9,
            '00000000-0000-0000-0000-000000000053', '00000000-0000-0000-0000-0000000000b1', now(),
            '00000000-0000-0000-0000-000000000051') $q$);
SELECT pg_temp.ok('regrade in documented order passes the deferred check', $q$
    UPDATE grade SET superseded_by = '00000000-0000-0000-0000-0000000000d2' WHERE id = '00000000-0000-0000-0000-0000000000d1';
    INSERT INTO grade (id, student_member_id, submission_id, origin, score, grader_member_id, created_by_action_id,
                       posted_at, posted_by_member_id)
    VALUES ('00000000-0000-0000-0000-0000000000d2', '00000000-0000-0000-0000-000000000052', '00000000-0000-0000-0000-0000000000a1',
            'entered', 9, '00000000-0000-0000-0000-000000000053', '00000000-0000-0000-0000-0000000000b1', now(),
            '00000000-0000-0000-0000-000000000051');
    SET CONSTRAINTS ALL IMMEDIATE;
    SET CONSTRAINTS ALL DEFERRED $q$);
SELECT pg_temp.fails('superseded_by must exist by commit', '23503', $q$
    UPDATE grade SET superseded_by = '00000000-0000-0000-0000-0000000000ff' WHERE id = '00000000-0000-0000-0000-0000000000d2' $q$);
SELECT pg_temp.ok('component grade entered directly', $q$
    INSERT INTO grade (id, student_member_id, component_id, origin, score, grader_member_id, created_by_action_id,
                       posted_at, posted_by_member_id)
    VALUES ('00000000-0000-0000-0000-0000000000d3', '00000000-0000-0000-0000-000000000052', '00000000-0000-0000-0000-000000000064',
            'entered', 78, '00000000-0000-0000-0000-000000000051', '00000000-0000-0000-0000-0000000000b1', now(),
            '00000000-0000-0000-0000-000000000051') $q$);
SELECT pg_temp.fails('second live grade for the same component and student', '23505', $q$
    INSERT INTO grade (student_member_id, component_id, origin, score, grader_member_id, created_by_action_id,
                       posted_at, posted_by_member_id)
    VALUES ('00000000-0000-0000-0000-000000000052', '00000000-0000-0000-0000-000000000064', 'entered', 80,
            '00000000-0000-0000-0000-000000000051', '00000000-0000-0000-0000-0000000000b1', now(),
            '00000000-0000-0000-0000-000000000051') $q$);
SELECT pg_temp.ok('same component for another student is fine', $q$
    INSERT INTO grade (student_member_id, component_id, origin, score, grader_member_id, created_by_action_id,
                       posted_at, posted_by_member_id)
    VALUES ('00000000-0000-0000-0000-000000000058', '00000000-0000-0000-0000-000000000064', 'entered', 65,
            '00000000-0000-0000-0000-000000000051', '00000000-0000-0000-0000-0000000000b1', now(),
            '00000000-0000-0000-0000-000000000051') $q$);
SELECT pg_temp.ok('posted course total is a computed snapshot', $q$
    INSERT INTO grade (student_member_id, component_id, origin, score, grader_member_id, created_by_action_id,
                       posted_at, posted_by_member_id)
    VALUES ('00000000-0000-0000-0000-000000000052', '00000000-0000-0000-0000-000000000061', 'computed', 71.5,
            '00000000-0000-0000-0000-000000000051', '00000000-0000-0000-0000-0000000000b1', now(),
            '00000000-0000-0000-0000-000000000051') $q$);
SELECT pg_temp.ok('feedback file is a document owned by the grade', $q$
    INSERT INTO document (course_id, kind, title, grade_id)
    VALUES ('00000000-0000-0000-0000-000000000041', 'feedback', 'comments.pdf', '00000000-0000-0000-0000-0000000000d2') $q$);

-- Events ---------------------------------------------------------------------
SELECT pg_temp.ok('event without an action', $q$
    INSERT INTO event (type, course_id, subject_type, subject_id)
    VALUES ('assignment.due_passed', '00000000-0000-0000-0000-000000000041', 'assignment', '00000000-0000-0000-0000-000000000071') $q$);
SELECT pg_temp.ok('event from an action', $q$
    INSERT INTO event (type, course_id, action_id, subject_type, subject_id)
    VALUES ('grade.posted', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-0000000000b1', 'grade',
            '00000000-0000-0000-0000-0000000000d2') $q$);
SELECT pg_temp.fails('events are append-only', '23001', $q$
    DELETE FROM event $q$);

\o
ROLLBACK;
\echo 'All checks passed.'

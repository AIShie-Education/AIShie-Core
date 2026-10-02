-- AIshie Core — database-enforced rule tests
--
-- Run against a throwaway database that has the migrations applied. Every
-- statement runs inside one transaction that is rolled back at the end, but
-- the fixtures use fixed ids, so do not point this at a database with data.
--
--   createdb aishie_test
--   for f in migrations/*.up.sql; do psql -v ON_ERROR_STOP=1 -d aishie_test -f "$f"; done
--   psql -X -d aishie_test -f tests/constraints_test.sql
--   dropdb aishie_test
--
-- or `make db-test-sql` at the repository root, which does all of it.
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
-- ids: 11 term · 21 dept · 2xx the department tree · dax appointments
--      3x actors · 4x courses · 5x members · 6x components
--      7x assignments · ex documents · fx versions · ax submissions
--      bx actions · dx grades · cx conversations · cxx their messages
--      1cx credentials · 1ax join links · cax attachments
--      22xx files of versions · 25xx agents' hosting · 26xx renditions
--      29xx what answers relied on
INSERT INTO term (id, name, starts_on, ends_on)
VALUES ('00000000-0000-0000-0000-000000000011', '2026 Autumn', '2026-09-01', '2026-12-20');
INSERT INTO department (id, name) VALUES ('00000000-0000-0000-0000-000000000021', 'Computing');
INSERT INTO actor (id, kind, display_name, platform_role, created_by_actor_id, hosting) VALUES
    ('00000000-0000-0000-0000-000000000031', 'human',  'root',   'root',  NULL,                                   NULL),
    ('00000000-0000-0000-0000-000000000032', 'human',  'admin',  'admin', '00000000-0000-0000-0000-000000000031', NULL),
    ('00000000-0000-0000-0000-000000000033', 'system', 'system', NULL,    '00000000-0000-0000-0000-000000000031', NULL),
    ('00000000-0000-0000-0000-000000000034', 'human',  'Sato',   NULL,    '00000000-0000-0000-0000-000000000033', NULL),
    ('00000000-0000-0000-0000-000000000035', 'human',  'Yuki',   NULL,    '00000000-0000-0000-0000-000000000033', NULL),
    ('00000000-0000-0000-0000-000000000036', 'agent',  'grader', NULL,    '00000000-0000-0000-0000-000000000034', 'mcp'),
    ('00000000-0000-0000-0000-000000000037', 'human',  'Ken',    NULL,    '00000000-0000-0000-0000-000000000033', NULL);
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
    VALUES ('00000000-0000-0000-0000-000000000035', 'sso', 'school-adfs', 'yuki@students.example.edu') $q$);
SELECT pg_temp.fails('sso credential needs provider and subject', '23514', $q$
    INSERT INTO credential (actor_id, kind) VALUES ('00000000-0000-0000-0000-000000000035', 'sso') $q$);
SELECT pg_temp.fails('one sso identity cannot map to two actors', '23505', $q$
    INSERT INTO credential (actor_id, kind, provider, subject)
    VALUES ('00000000-0000-0000-0000-000000000037', 'sso', 'school-adfs', 'yuki@students.example.edu') $q$);
SELECT pg_temp.fails('password credential needs a hash', '23514', $q$
    INSERT INTO credential (actor_id, kind) VALUES ('00000000-0000-0000-0000-000000000034', 'password') $q$);
SELECT pg_temp.fails('api token needs a lookup prefix', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash) VALUES ('00000000-0000-0000-0000-000000000036', 'api_token', 'h') $q$);
SELECT pg_temp.ok('login session with a prefix and an expiry', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix, expires_at)
    VALUES ('00000000-0000-0000-0000-000000000034', 'session', 'h', 'sess-1', now() + interval '12 hours') $q$);
SELECT pg_temp.fails('session needs a lookup prefix', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, expires_at)
    VALUES ('00000000-0000-0000-0000-000000000034', 'session', 'h', now() + interval '12 hours') $q$);
SELECT pg_temp.fails('session must expire', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix)
    VALUES ('00000000-0000-0000-0000-000000000034', 'session', 'h', 'sess-2') $q$);
SELECT pg_temp.ok('invitation with a prefix and an expiry', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix, expires_at)
    VALUES ('00000000-0000-0000-0000-000000000037', 'invite', 'h', 'inv-1', now() + interval '7 days') $q$);
SELECT pg_temp.fails('invitation needs a lookup prefix', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, expires_at)
    VALUES ('00000000-0000-0000-0000-000000000034', 'invite', 'h', now() + interval '7 days') $q$);
SELECT pg_temp.fails('invitation must expire', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix)
    VALUES ('00000000-0000-0000-0000-000000000034', 'invite', 'h', 'inv-2') $q$);
SELECT pg_temp.fails('one live invitation per actor', '23505', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix, expires_at)
    VALUES ('00000000-0000-0000-0000-000000000037', 'invite', 'h', 'inv-3', now() + interval '7 days') $q$);
SELECT pg_temp.ok('a new invitation once the last is revoked', $q$
    UPDATE credential SET revoked_at = now() WHERE token_prefix = 'inv-1';
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix, expires_at)
    VALUES ('00000000-0000-0000-0000-000000000037', 'invite', 'h', 'inv-4', now() + interval '7 days') $q$);
SELECT pg_temp.fails('unknown credential kind is rejected', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash) VALUES ('00000000-0000-0000-0000-000000000034', 'passkey', 'h') $q$);
SELECT pg_temp.fails('the system actor holds no credential', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix)
    VALUES ('00000000-0000-0000-0000-000000000033', 'api_token', 'h', 'sys-1') $q$);
SELECT pg_temp.fails('a credential is not moved to the system actor', '23514', $q$
    UPDATE credential SET actor_id = '00000000-0000-0000-0000-000000000033' WHERE token_prefix = 'sess-1' $q$);

-- API tokens are for agents; signing in is for people.
SELECT pg_temp.ok('an agent holds an API token', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix, label)
    VALUES ('00000000-0000-0000-0000-000000000036', 'api_token', 'h', 'tok-agent-1', 'grader') $q$);
SELECT pg_temp.fails('a person holds no API token', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix)
    VALUES ('00000000-0000-0000-0000-000000000034', 'api_token', 'h', 'tok-person-1') $q$);
SELECT pg_temp.fails('root neither, as bootstrap once gave it', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix, label)
    VALUES ('00000000-0000-0000-0000-000000000031', 'api_token', 'h', 'tok-root-1', 'bootstrap') $q$);
SELECT pg_temp.fails('not even one written revoked', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix, revoked_at)
    VALUES ('00000000-0000-0000-0000-000000000034', 'api_token', 'h', 'tok-person-2', now()) $q$);
SELECT pg_temp.fails('nor is an agent''s token moved to a person', '23514', $q$
    UPDATE credential SET actor_id = '00000000-0000-0000-0000-000000000034' WHERE token_prefix = 'tok-agent-1' $q$);
SELECT pg_temp.fails('nor a person''s session made into one', '23514', $q$
    UPDATE credential SET kind = 'api_token', expires_at = NULL WHERE token_prefix = 'sess-1' $q$);
SELECT pg_temp.ok('a person''s token from before the rule is revoked, as the migration revokes it', $q$
    ALTER TABLE credential DISABLE TRIGGER credential_fits_actor_kind;
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix)
    VALUES ('00000000-0000-0000-0000-000000000034', 'api_token', 'h', 'tok-person-old');
    ALTER TABLE credential ENABLE TRIGGER credential_fits_actor_kind;
    UPDATE credential SET revoked_at = now() WHERE token_prefix = 'tok-person-old' $q$);
SELECT pg_temp.fails('and is not brought back', '23514', $q$
    UPDATE credential SET revoked_at = NULL WHERE token_prefix = 'tok-person-old' $q$);
SELECT pg_temp.ok('while it stays revoked, whatever else is done to it passes', $q$
    UPDATE credential SET revoked_at = revoked_at - interval '1 second', label = 'from before 0017' WHERE token_prefix = 'tok-person-old' $q$);
SELECT pg_temp.fails('an agent holds no password', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash) VALUES ('00000000-0000-0000-0000-000000000036', 'password', 'h') $q$);
SELECT pg_temp.fails('no invitation to choose one', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix, expires_at)
    VALUES ('00000000-0000-0000-0000-000000000036', 'invite', 'h', 'inv-agent', now() + interval '7 days') $q$);
SELECT pg_temp.fails('no identity at a provider', '23514', $q$
    INSERT INTO credential (actor_id, kind, provider, subject)
    VALUES ('00000000-0000-0000-0000-000000000036', 'sso', 'school-adfs', 'grader@students.example.edu') $q$);
SELECT pg_temp.fails('and no session, which only signing in makes', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix, expires_at)
    VALUES ('00000000-0000-0000-0000-000000000036', 'session', 'h', 'sess-agent', now() + interval '12 hours') $q$);
SELECT pg_temp.fails('nor is a person''s session moved to an agent', '23514', $q$
    UPDATE credential SET actor_id = '00000000-0000-0000-0000-000000000036' WHERE token_prefix = 'sess-1' $q$);
SELECT pg_temp.ok('an agent''s token is revoked as any credential is', $q$
    UPDATE credential SET revoked_at = now() WHERE token_prefix = 'tok-agent-1' $q$);

-- Login IDs: a person's student or staff number, a sign-in name beside the email.
SELECT pg_temp.ok('a person has a login ID, as an administrator gives it', $q$
    UPDATE actor SET login_id = 'UNI20230001' WHERE id = '00000000-0000-0000-0000-000000000035' $q$);
SELECT pg_temp.ok('and an email beside it', $q$
    UPDATE actor SET login_id = 'T19880042', email = 'sato@campus.example.edu' WHERE id = '00000000-0000-0000-0000-000000000034' $q$);
SELECT pg_temp.fails('a login ID is unique regardless of case', '23505', $q$
    INSERT INTO actor (kind, display_name, login_id, created_by_actor_id)
    VALUES ('human', 'x', 'uni20230001', '00000000-0000-0000-0000-000000000031') $q$);
SELECT pg_temp.fails('and is not taken over by another person', '23505', $q$
    UPDATE actor SET login_id = 'Uni20230001' WHERE id = '00000000-0000-0000-0000-000000000037' $q$);
SELECT pg_temp.ok('its longest: 64 letters, digits, dots, hyphens and underscores', $q$
    UPDATE actor SET login_id = 'uni.2023-00_' || repeat('7', 52) WHERE id = '00000000-0000-0000-0000-000000000037' $q$);
SELECT pg_temp.fails('never longer', '23514', $q$
    UPDATE actor SET login_id = repeat('7', 65) WHERE id = '00000000-0000-0000-0000-000000000037' $q$);
SELECT pg_temp.fails('never empty', '23514', $q$
    UPDATE actor SET login_id = '' WHERE id = '00000000-0000-0000-0000-000000000037' $q$);
SELECT pg_temp.fails('never with an @, so never taken for an email', '23514', $q$
    UPDATE actor SET login_id = 'ken@example.edu' WHERE id = '00000000-0000-0000-0000-000000000037' $q$);
SELECT pg_temp.fails('never with a space, so kept trimmed', '23514', $q$
    UPDATE actor SET login_id = ' 20230003' WHERE id = '00000000-0000-0000-0000-000000000037' $q$);
SELECT pg_temp.fails('nor a space within', '23514', $q$
    UPDATE actor SET login_id = '2023 0003' WHERE id = '00000000-0000-0000-0000-000000000037' $q$);
SELECT pg_temp.fails('nor a line after it', '23514', $q$
    UPDATE actor SET login_id = E'20230003\n' WHERE id = '00000000-0000-0000-0000-000000000037' $q$);
SELECT pg_temp.fails('and in ASCII: no letter of another alphabet', '23514', $q$
    UPDATE actor SET login_id = 'unié2023' WHERE id = '00000000-0000-0000-0000-000000000037' $q$);
SELECT pg_temp.fails('nor a full-width digit', '23514', $q$
    UPDATE actor SET login_id = '２０２３' WHERE id = '00000000-0000-0000-0000-000000000037' $q$);
SELECT pg_temp.fails('an agent has no login ID', '23514', $q$
    UPDATE actor SET login_id = 'grader-v2' WHERE id = '00000000-0000-0000-0000-000000000036' $q$);
SELECT pg_temp.fails('nor is one registered with one', '23514', $q$
    INSERT INTO actor (kind, display_name, login_id, created_by_actor_id, hosting)
    VALUES ('agent', 'bot', 'bot-1', '00000000-0000-0000-0000-000000000031', 'mcp') $q$);
SELECT pg_temp.fails('nor has the system actor one', '23514', $q$
    UPDATE actor SET login_id = 'system' WHERE id = '00000000-0000-0000-0000-000000000033' $q$);
SELECT pg_temp.ok('a person who typed their own has it recorded unchecked', $q$
    INSERT INTO actor (kind, display_name, login_id, login_id_verified, created_by_actor_id)
    VALUES ('human', 'Wei', '20230009', false, '00000000-0000-0000-0000-000000000034') $q$);
SELECT pg_temp.fails('only a person''s login ID goes unchecked', '23514', $q$
    INSERT INTO actor (kind, display_name, login_id_verified, created_by_actor_id, hosting)
    VALUES ('agent', 'bot', false, '00000000-0000-0000-0000-000000000031', 'mcp') $q$);
SELECT pg_temp.fails('and only a login ID there is', '23514', $q$
    INSERT INTO actor (kind, display_name, email, login_id_verified, created_by_actor_id)
    VALUES ('human', 'x', 'x@example.edu', false, '00000000-0000-0000-0000-000000000031') $q$);
SELECT pg_temp.ok('every other actor''s login ID is vouched for', $q$
    DO $chk$
    BEGIN
        IF EXISTS (SELECT 1 FROM actor WHERE NOT login_id_verified AND login_id IS DISTINCT FROM '20230009') THEN
            RAISE EXCEPTION 'an actor registered otherwise than through a link is unverified';
        END IF;
    END $chk$ $q$);

-- A temporary password: one someone else set, which its person must change.
SELECT pg_temp.ok('a password is not temporary unless it is marked', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, label)
    VALUES ('00000000-0000-0000-0000-000000000035', 'password', 'h', 'own-1');
    DO $chk$
    BEGIN
        IF EXISTS (SELECT 1 FROM credential WHERE label = 'own-1' AND must_change) THEN
            RAISE EXCEPTION 'a password is temporary by default';
        END IF;
    END $chk$ $q$);
SELECT pg_temp.ok('a password someone else set is marked to be changed, saying who set it', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, issued_by_actor_id, must_change)
    VALUES ('00000000-0000-0000-0000-000000000035', 'password', 'h', '00000000-0000-0000-0000-000000000034', true) $q$);
SELECT pg_temp.fails('a temporary password says who set it', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, must_change)
    VALUES ('00000000-0000-0000-0000-000000000035', 'password', 'h', true) $q$);
SELECT pg_temp.fails('only a password is changed so', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix, expires_at, issued_by_actor_id, must_change)
    VALUES ('00000000-0000-0000-0000-000000000035', 'session', 'h', 'sess-mc', now() + interval '12 hours',
            '00000000-0000-0000-0000-000000000034', true) $q$);
SELECT pg_temp.fails('never a token', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix, issued_by_actor_id, must_change)
    VALUES ('00000000-0000-0000-0000-000000000036', 'api_token', 'h', 'tok-mc', '00000000-0000-0000-0000-000000000034', true) $q$);
SELECT pg_temp.fails('nor a password marked afterwards with nobody who set it', '23514', $q$
    UPDATE credential SET must_change = true WHERE label = 'own-1' $q$);

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

-- Agents a person owns, and their delegate seats ------------------------------
-- 38 Yuki's agent · 5a its seat in A, as Yuki's (52) delegate
SELECT pg_temp.ok('a person owns an agent', $q$
    INSERT INTO actor (id, kind, display_name, owner_actor_id, created_by_actor_id, hosting)
    VALUES ('00000000-0000-0000-0000-000000000038', 'agent', 'Yuki''s agent', '00000000-0000-0000-0000-000000000035', '00000000-0000-0000-0000-000000000035', 'mcp') $q$);
SELECT pg_temp.fails('an agent owns no agent', '23514', $q$
    INSERT INTO actor (kind, display_name, owner_actor_id, created_by_actor_id, hosting)
    VALUES ('agent', 'x', '00000000-0000-0000-0000-000000000036', '00000000-0000-0000-0000-000000000031', 'mcp') $q$);
SELECT pg_temp.fails('the system actor owns nothing', '23514', $q$
    INSERT INTO actor (kind, display_name, owner_actor_id, created_by_actor_id, hosting)
    VALUES ('agent', 'x', '00000000-0000-0000-0000-000000000033', '00000000-0000-0000-0000-000000000031', 'mcp') $q$);
SELECT pg_temp.fails('only an agent has an owner', '23514', $q$
    INSERT INTO actor (kind, display_name, owner_actor_id, created_by_actor_id)
    VALUES ('human', 'x', '00000000-0000-0000-0000-000000000035', '00000000-0000-0000-0000-000000000031') $q$);
SELECT pg_temp.fails('an owner is an actor that exists', '23514', $q$
    INSERT INTO actor (kind, display_name, owner_actor_id, created_by_actor_id, hosting)
    VALUES ('agent', 'x', '00000000-0000-0000-0000-0000000000ff', '00000000-0000-0000-0000-000000000031', 'mcp') $q$);
SELECT pg_temp.fails('an agent''s owner never changes: not to another person', '23001', $q$
    UPDATE actor SET owner_actor_id = '00000000-0000-0000-0000-000000000037' WHERE id = '00000000-0000-0000-0000-000000000038' $q$);
SELECT pg_temp.fails('nor to an agent', '23001', $q$
    UPDATE actor SET owner_actor_id = '00000000-0000-0000-0000-000000000036' WHERE id = '00000000-0000-0000-0000-000000000038' $q$);
SELECT pg_temp.fails('nor to nobody', '23001', $q$
    UPDATE actor SET owner_actor_id = NULL WHERE id = '00000000-0000-0000-0000-000000000038' $q$);
SELECT pg_temp.fails('and an agent registered with no owner is given none', '23001', $q$
    UPDATE actor SET owner_actor_id = '00000000-0000-0000-0000-000000000034' WHERE id = '00000000-0000-0000-0000-000000000036' $q$);
SELECT pg_temp.ok('the rest of an owned agent''s row changes, its owner named as it is', $q$
    UPDATE actor SET display_name = 'Yuki''s helper', owner_actor_id = '00000000-0000-0000-0000-000000000035'
     WHERE id = '00000000-0000-0000-0000-000000000038';
    UPDATE actor SET display_name = 'grader', owner_actor_id = NULL WHERE id = '00000000-0000-0000-0000-000000000036' $q$);
SELECT pg_temp.fails('an agent someone owns holds no platform role', '23514', $q$
    UPDATE actor SET platform_role = 'admin' WHERE id = '00000000-0000-0000-0000-000000000038' $q$);
SELECT pg_temp.fails('nor is an agent that holds one given an owner', '23514', $q$
    INSERT INTO actor (kind, display_name, platform_role, owner_actor_id, created_by_actor_id, hosting)
    VALUES ('agent', 'x', 'admin', '00000000-0000-0000-0000-000000000035', '00000000-0000-0000-0000-000000000031', 'mcp') $q$);
SELECT pg_temp.fails('nothing owns itself', '23514', $q$
    INSERT INTO actor (id, kind, display_name, owner_actor_id, created_by_actor_id, hosting)
    VALUES ('00000000-0000-0000-0000-0000000000fe', 'agent', 'x', '00000000-0000-0000-0000-0000000000fe', '00000000-0000-0000-0000-000000000031', 'mcp') $q$);
SELECT pg_temp.fails('who suspended an actor is an actor', '23503', $q$
    UPDATE actor SET suspended_by_actor_id = '00000000-0000-0000-0000-0000000000ff' WHERE id = '00000000-0000-0000-0000-000000000038' $q$);
SELECT pg_temp.ok('making an actor active again forgets who suspended it', $q$
    UPDATE actor SET status = 'suspended', suspended_by_actor_id = '00000000-0000-0000-0000-000000000035' WHERE id = '00000000-0000-0000-0000-000000000038';
    UPDATE actor SET status = 'active' WHERE id = '00000000-0000-0000-0000-000000000038';
    DO $chk$
    BEGIN
        IF EXISTS (SELECT 1 FROM actor WHERE id = '00000000-0000-0000-0000-000000000038' AND suspended_by_actor_id IS NOT NULL) THEN
            RAISE EXCEPTION 'suspended_by_actor_id outlived the suspension';
        END IF;
    END $chk$ $q$);
SELECT pg_temp.ok('an owned agent is seated as its owner''s delegate', $q$
    INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope, principal_member_id)
    VALUES ('00000000-0000-0000-0000-00000000005a', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000038', 'assistant', '00000000-0000-0000-0000-000000000035', 'listed', 'all', '00000000-0000-0000-0000-000000000052') $q$);
-- An agent decides only by proposal: its seat is written with action_decide
-- at confirm_required at most, owned or not; a person's as it is given.
SELECT pg_temp.ok('an agent''s seat is written deciding only by proposal', $q$
    UPDATE course_member SET perm_action_decide = 'autonomous' WHERE id = '00000000-0000-0000-0000-000000000053';
    UPDATE course_member SET perm_action_decide = 'pending_review' WHERE id = '00000000-0000-0000-0000-00000000005a';
    DO $chk$
    BEGIN
        IF EXISTS (SELECT 1 FROM course_member WHERE id IN ('00000000-0000-0000-0000-000000000053', '00000000-0000-0000-0000-00000000005a')
                   AND perm_action_decide <> 'confirm_required') THEN
            RAISE EXCEPTION 'an agent''s seat holds action_decide above confirm_required';
        END IF;
    END $chk$ $q$);
SELECT pg_temp.ok('and so is a new one', $q$
    INSERT INTO actor (id, kind, display_name, created_by_actor_id, hosting)
    VALUES ('00000000-0000-0000-0000-0000000003d0', 'agent', 'triage', '00000000-0000-0000-0000-000000000031', 'mcp');
    INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope, perm_action_decide)
    VALUES ('00000000-0000-0000-0000-0000000005d0', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-0000000003d0',
            'assistant', '00000000-0000-0000-0000-000000000034', 'all', 'all', 'autonomous');
    DO $chk$
    BEGIN
        IF NOT EXISTS (SELECT 1 FROM course_member WHERE id = '00000000-0000-0000-0000-0000000005d0' AND perm_action_decide = 'confirm_required') THEN
            RAISE EXCEPTION 'a new agent''s seat holds action_decide above confirm_required';
        END IF;
    END $chk$ $q$);
SELECT pg_temp.ok('a person''s seat decides as it is given, and an agent''s below the ceiling too', $q$
    UPDATE course_member SET perm_action_decide = 'pending_review' WHERE id = '00000000-0000-0000-0000-000000000058';
    UPDATE course_member SET perm_action_decide = 'denied' WHERE id = '00000000-0000-0000-0000-000000000053';
    DO $chk$
    BEGIN
        IF NOT EXISTS (SELECT 1 FROM course_member WHERE id = '00000000-0000-0000-0000-000000000051' AND perm_action_decide = 'autonomous')
           OR NOT EXISTS (SELECT 1 FROM course_member WHERE id = '00000000-0000-0000-0000-000000000058' AND perm_action_decide = 'pending_review')
           OR NOT EXISTS (SELECT 1 FROM course_member WHERE id = '00000000-0000-0000-0000-000000000053' AND perm_action_decide = 'denied') THEN
            RAISE EXCEPTION 'a level at or below the ceiling, or a person''s, was changed';
        END IF;
    END $chk$ $q$);
SELECT pg_temp.ok('a removed agent''s seat is history, and is left as it was written', $q$
    UPDATE course_member SET status = 'removed' WHERE id = '00000000-0000-0000-0000-0000000005d0';
    UPDATE course_member SET perm_action_decide = 'autonomous' WHERE id = '00000000-0000-0000-0000-0000000005d0';
    DO $chk$
    BEGIN
        IF NOT EXISTS (SELECT 1 FROM course_member WHERE id = '00000000-0000-0000-0000-0000000005d0' AND perm_action_decide = 'autonomous') THEN
            RAISE EXCEPTION 'a removed seat was changed';
        END IF;
    END $chk$ $q$);
SELECT pg_temp.fails('an owned agent is not seated without a principal', '23514', $q$
    INSERT INTO course_member (course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope)
    VALUES ('00000000-0000-0000-0000-000000000042', '00000000-0000-0000-0000-000000000038', 'assistant', '00000000-0000-0000-0000-000000000034', 'all', 'all') $q$);
SELECT pg_temp.fails('nor as the delegate of someone other than its owner', '23514', $q$
    INSERT INTO course_member (course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope, principal_member_id)
    VALUES ('00000000-0000-0000-0000-000000000042', '00000000-0000-0000-0000-000000000038', 'assistant', '00000000-0000-0000-0000-000000000035', 'listed', 'all', '00000000-0000-0000-0000-000000000055') $q$);
SELECT pg_temp.fails('a principal is a seat in the same course', '23503', $q$
    INSERT INTO course_member (course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope, principal_member_id)
    VALUES ('00000000-0000-0000-0000-000000000042', '00000000-0000-0000-0000-000000000038', 'assistant', '00000000-0000-0000-0000-000000000035', 'listed', 'all', '00000000-0000-0000-0000-000000000052') $q$);
SELECT pg_temp.fails('an actor nobody owns is nobody''s delegate', '23514', $q$
    INSERT INTO course_member (course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope, principal_member_id)
    VALUES ('00000000-0000-0000-0000-000000000042', '00000000-0000-0000-0000-000000000036', 'assistant', '00000000-0000-0000-0000-000000000034', 'all', 'all', '00000000-0000-0000-0000-000000000055') $q$);
SELECT pg_temp.fails('a seat is not its own principal', '23514', $q$
    UPDATE course_member SET principal_member_id = id WHERE id = '00000000-0000-0000-0000-00000000005a' $q$);
SELECT pg_temp.fails('a delegate''s principal is nobody''s delegate', '23514', $q$
    -- A removed seat is history and is not checked, so it can name a
    -- principal it should not; a delegate cannot then name it.
    INSERT INTO course_member (id, course_id, actor_id, role, status, added_by_actor_id, student_scope, assignment_scope, principal_member_id)
    VALUES ('00000000-0000-0000-0000-00000000005b', '00000000-0000-0000-0000-000000000042', '00000000-0000-0000-0000-000000000035', 'student', 'removed', '00000000-0000-0000-0000-000000000034', 'listed', 'all', '00000000-0000-0000-0000-000000000055');
    INSERT INTO course_member (course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope, principal_member_id)
    VALUES ('00000000-0000-0000-0000-000000000042', '00000000-0000-0000-0000-000000000038', 'assistant', '00000000-0000-0000-0000-000000000035', 'listed', 'all', '00000000-0000-0000-0000-00000000005b') $q$);
SELECT pg_temp.ok('a delegate''s seat is removed with its principal''s', $q$
    -- Mei (3a) seated in A (5d), her agent (39) as her delegate there (5c),
    -- paused; Mei's seat is then removed, and both are history afterwards.
    INSERT INTO actor (id, kind, display_name, created_by_actor_id)
    VALUES ('00000000-0000-0000-0000-00000000003a', 'human', 'Mei', '00000000-0000-0000-0000-000000000031');
    INSERT INTO actor (id, kind, display_name, owner_actor_id, created_by_actor_id, hosting)
    VALUES ('00000000-0000-0000-0000-000000000039', 'agent', 'Mei''s agent', '00000000-0000-0000-0000-00000000003a', '00000000-0000-0000-0000-00000000003a', 'mcp');
    INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope)
    VALUES ('00000000-0000-0000-0000-00000000005d', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-00000000003a', 'student', '00000000-0000-0000-0000-000000000034', 'listed', 'all');
    INSERT INTO course_member (id, course_id, actor_id, role, status, added_by_actor_id, student_scope, assignment_scope, principal_member_id)
    VALUES ('00000000-0000-0000-0000-00000000005c', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000039', 'assistant', 'paused', '00000000-0000-0000-0000-00000000003a', 'listed', 'all', '00000000-0000-0000-0000-00000000005d');
    UPDATE course_member SET status = 'removed' WHERE id = '00000000-0000-0000-0000-00000000005d';
    DO $chk$
    BEGIN
        IF NOT EXISTS (SELECT 1 FROM course_member WHERE id = '00000000-0000-0000-0000-00000000005c' AND status = 'removed') THEN
            RAISE EXCEPTION 'the delegate outlived its principal''s removal';
        END IF;
    END $chk$ $q$);
-- Owners changed before migration 0014, which the checks below stand in for
-- with its trigger switched off, left delegate seats behind in archived
-- courses that no longer match their agents' owners.
SELECT pg_temp.fails('a delegate seat is not resumed once its actor has another owner', '23514', $q$
    UPDATE course_member SET status = 'paused' WHERE id = '00000000-0000-0000-0000-00000000005a';
    ALTER TABLE actor DISABLE TRIGGER actor_owner_fixed;
    UPDATE actor SET owner_actor_id = '00000000-0000-0000-0000-000000000037' WHERE id = '00000000-0000-0000-0000-000000000038';
    ALTER TABLE actor ENABLE TRIGGER actor_owner_fixed;
    UPDATE course_member SET status = 'active' WHERE id = '00000000-0000-0000-0000-00000000005a' $q$);
SELECT pg_temp.ok('a removed delegate seat is history: it names the principal its agent''s owner had', $q$
    UPDATE course_member SET status = 'removed' WHERE id = '00000000-0000-0000-0000-00000000005a';
    ALTER TABLE actor DISABLE TRIGGER actor_owner_fixed;
    UPDATE actor SET owner_actor_id = '00000000-0000-0000-0000-000000000037' WHERE id = '00000000-0000-0000-0000-000000000038';
    UPDATE actor SET owner_actor_id = NULL WHERE id = '00000000-0000-0000-0000-000000000038';
    ALTER TABLE actor ENABLE TRIGGER actor_owner_fixed;
    INSERT INTO course_member (course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope)
    VALUES ('00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000038', 'assistant', '00000000-0000-0000-0000-000000000034', 'all', 'all') $q$);
SELECT pg_temp.ok('the new permissions default to denied on both tables', $q$
    DO $chk$
    BEGIN
        IF EXISTS (SELECT 1 FROM course_member WHERE id = '00000000-0000-0000-0000-00000000005a'
                   AND (perm_agent_delegate, perm_conversation_ask, perm_conversation_answer)
                       <> ('denied', 'denied', 'denied'))
           OR EXISTS (SELECT 1 FROM permission_preset WHERE id = '00000000-0000-0000-0000-000000000091'
                   AND (perm_agent_delegate, perm_conversation_ask, perm_conversation_answer)
                       <> ('denied', 'denied', 'denied')) THEN
            RAISE EXCEPTION 'a new permission does not default to denied';
        END IF;
    END $chk$ $q$);
SELECT pg_temp.fails('only a delegate''s seat answers the course', '23514', $q$
    UPDATE course_member SET answers_course = true WHERE id = '00000000-0000-0000-0000-000000000051' $q$);

-- Site chat: declared by nobody since 0025, and recorded nowhere since 0027 ---
SELECT pg_temp.ok('no actor names a site chat credential', $q$
    DO $chk$
    BEGIN
        IF EXISTS (SELECT 1 FROM information_schema.columns
                   WHERE table_schema = 'public' AND table_name = 'actor' AND column_name = 'site_chat_credential_id') THEN
            RAISE EXCEPTION 'actor.site_chat_credential_id is still there';
        END IF;
    END $chk$ $q$);

-- Join links: a way into a course, as a student ------------------------------
-- 1a1 Sato's (51) link to A · 3c Aoi, who registered through it · 5f her seat
SELECT pg_temp.ok('a join link keeps a hash of its token, and seats with its maker''s authority', $q$
    INSERT INTO course_join_link (id, course_id, token_prefix, secret_hash, preset_id, created_by_member_id, expires_at, max_uses)
    VALUES ('00000000-0000-0000-0000-0000000001a1', '00000000-0000-0000-0000-000000000041', 'join-1', 'sha256:' || repeat('0', 64),
            '00000000-0000-0000-0000-000000000091', '00000000-0000-0000-0000-000000000051', now() + interval '10 minutes', 2) $q$);
SELECT pg_temp.fails('never the token itself', '23514', $q$
    INSERT INTO course_join_link (course_id, token_prefix, secret_hash, preset_id, created_by_member_id, expires_at)
    VALUES ('00000000-0000-0000-0000-000000000041', 'join-2', 'aisjoin_join2abcdefgh_' || repeat('x', 43),
            '00000000-0000-0000-0000-000000000091', '00000000-0000-0000-0000-000000000051', now() + interval '10 minutes') $q$);
SELECT pg_temp.fails('a prefix finds one link', '23505', $q$
    INSERT INTO course_join_link (course_id, token_prefix, secret_hash, preset_id, created_by_member_id, expires_at)
    VALUES ('00000000-0000-0000-0000-000000000041', 'join-1', 'sha256:' || repeat('1', 64),
            '00000000-0000-0000-0000-000000000091', '00000000-0000-0000-0000-000000000051', now() + interval '10 minutes') $q$);
SELECT pg_temp.fails('its maker is a seat of its course', '23503', $q$
    INSERT INTO course_join_link (course_id, token_prefix, secret_hash, preset_id, created_by_member_id, expires_at)
    VALUES ('00000000-0000-0000-0000-000000000041', 'join-3', 'sha256:' || repeat('3', 64),
            '00000000-0000-0000-0000-000000000091', '00000000-0000-0000-0000-000000000055', now() + interval '10 minutes') $q$);
SELECT pg_temp.fails('it seats a student, and nobody else', '23514', $q$
    INSERT INTO course_join_link (course_id, token_prefix, secret_hash, role, preset_id, created_by_member_id, expires_at)
    VALUES ('00000000-0000-0000-0000-000000000041', 'join-4', 'sha256:' || repeat('4', 64), 'ta',
            '00000000-0000-0000-0000-000000000091', '00000000-0000-0000-0000-000000000051', now() + interval '10 minutes') $q$);
SELECT pg_temp.fails('it expires', '23502', $q$
    INSERT INTO course_join_link (course_id, token_prefix, secret_hash, preset_id, created_by_member_id, expires_at)
    VALUES ('00000000-0000-0000-0000-000000000041', 'join-5', 'sha256:' || repeat('5', 64),
            '00000000-0000-0000-0000-000000000091', '00000000-0000-0000-0000-000000000051', NULL) $q$);
SELECT pg_temp.fails('ten minutes after it is made, and no sooner', '23514', $q$
    INSERT INTO course_join_link (course_id, token_prefix, secret_hash, preset_id, created_by_member_id, created_at, expires_at)
    VALUES ('00000000-0000-0000-0000-000000000041', 'join-6', 'sha256:' || repeat('6', 64),
            '00000000-0000-0000-0000-000000000091', '00000000-0000-0000-0000-000000000051', now(), now() + interval '9 minutes 59 seconds') $q$);
SELECT pg_temp.fails('nor later', '23514', $q$
    INSERT INTO course_join_link (course_id, token_prefix, secret_hash, preset_id, created_by_member_id, created_at, expires_at)
    VALUES ('00000000-0000-0000-0000-000000000041', 'join-7', 'sha256:' || repeat('7', 64),
            '00000000-0000-0000-0000-000000000091', '00000000-0000-0000-0000-000000000051', now(), now() + interval '1 day') $q$);
SELECT pg_temp.fails('nor has its life lengthened', '23514', $q$
    UPDATE course_join_link SET expires_at = expires_at + interval '1 minute' WHERE id = '00000000-0000-0000-0000-0000000001a1' $q$);
SELECT pg_temp.fails('a limit on its uses is of one or more', '23514', $q$
    INSERT INTO course_join_link (course_id, token_prefix, secret_hash, preset_id, created_by_member_id, expires_at, max_uses)
    VALUES ('00000000-0000-0000-0000-000000000041', 'join-8', 'sha256:' || repeat('8', 64),
            '00000000-0000-0000-0000-000000000091', '00000000-0000-0000-0000-000000000051', now() + interval '10 minutes', 0) $q$);
SELECT pg_temp.fails('a list of domains names one at least', '23514', $q$
    INSERT INTO course_join_link (course_id, token_prefix, secret_hash, preset_id, created_by_member_id, expires_at, allowed_email_domains)
    VALUES ('00000000-0000-0000-0000-000000000041', 'join-9', 'sha256:' || repeat('9', 64),
            '00000000-0000-0000-0000-000000000091', '00000000-0000-0000-0000-000000000051', now() + interval '10 minutes', '{}') $q$);
SELECT pg_temp.fails('and twenty at most', '23514', $q$
    INSERT INTO course_join_link (course_id, token_prefix, secret_hash, preset_id, created_by_member_id, expires_at, allowed_email_domains)
    VALUES ('00000000-0000-0000-0000-000000000041', 'join-a', 'sha256:' || repeat('a', 64),
            '00000000-0000-0000-0000-000000000091', '00000000-0000-0000-0000-000000000051', now() + interval '10 minutes',
            array(SELECT 'd' || n || '.example.edu' FROM generate_series(1, 21) n)) $q$);
SELECT pg_temp.fails('none of them null', '23514', $q$
    INSERT INTO course_join_link (course_id, token_prefix, secret_hash, preset_id, created_by_member_id, expires_at, allowed_email_domains)
    VALUES ('00000000-0000-0000-0000-000000000041', 'join-b', 'sha256:' || repeat('b', 64),
            '00000000-0000-0000-0000-000000000091', '00000000-0000-0000-0000-000000000051', now() + interval '10 minutes',
            ARRAY['example.edu', NULL]) $q$);
SELECT pg_temp.ok('its uses are counted up to its limit', $q$
    UPDATE course_join_link SET uses = uses + 2 WHERE id = '00000000-0000-0000-0000-0000000001a1' $q$);
SELECT pg_temp.fails('and never past it', '23514', $q$
    UPDATE course_join_link SET uses = uses + 1 WHERE id = '00000000-0000-0000-0000-0000000001a1' $q$);
SELECT pg_temp.fails('nor below none', '23514', $q$
    UPDATE course_join_link SET uses = -1, max_uses = NULL WHERE id = '00000000-0000-0000-0000-0000000001a1' $q$);
SELECT pg_temp.ok('a person registers through it, their email vouched for by nobody, and is seated through it', $q$
    INSERT INTO actor (id, kind, display_name, email, email_verified, created_by_actor_id)
    VALUES ('00000000-0000-0000-0000-00000000003c', 'human', 'Aoi', 'aoi@example.edu', false, '00000000-0000-0000-0000-000000000034');
    INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope, join_link_id)
    VALUES ('00000000-0000-0000-0000-00000000005f', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-00000000003c',
            'student', '00000000-0000-0000-0000-000000000034', 'listed', 'all', '00000000-0000-0000-0000-0000000001a1') $q$);
SELECT pg_temp.fails('a seat names a link of its own course', '23503', $q$
    INSERT INTO course_member (course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope, join_link_id)
    VALUES ('00000000-0000-0000-0000-000000000042', '00000000-0000-0000-0000-00000000003c', 'student',
            '00000000-0000-0000-0000-000000000034', 'listed', 'all', '00000000-0000-0000-0000-0000000001a1') $q$);
SELECT pg_temp.fails('only a person''s email goes unchecked', '23514', $q$
    INSERT INTO actor (kind, display_name, email, email_verified, created_by_actor_id, hosting)
    VALUES ('agent', 'x', 'x@example.edu', false, '00000000-0000-0000-0000-000000000034', 'mcp') $q$);
SELECT pg_temp.fails('and only an email there is', '23514', $q$
    INSERT INTO actor (kind, display_name, email_verified, created_by_actor_id)
    VALUES ('human', 'x', false, '00000000-0000-0000-0000-000000000034') $q$);
SELECT pg_temp.ok('every other actor''s email is vouched for', $q$
    DO $chk$
    BEGIN
        IF EXISTS (SELECT 1 FROM actor WHERE NOT email_verified AND id <> '00000000-0000-0000-0000-00000000003c') THEN
            RAISE EXCEPTION 'an actor registered otherwise than through a link is unverified';
        END IF;
    END $chk$ $q$);
SELECT pg_temp.fails('a revocation says who revoked it', '23514', $q$
    UPDATE course_join_link SET revoked_at = now() WHERE id = '00000000-0000-0000-0000-0000000001a1' $q$);
SELECT pg_temp.fails('who revoked it is a seat of its course', '23503', $q$
    UPDATE course_join_link SET revoked_at = now(), revoked_by_member_id = '00000000-0000-0000-0000-000000000055'
    WHERE id = '00000000-0000-0000-0000-0000000001a1' $q$);
SELECT pg_temp.ok('a link is revoked, saying by whom', $q$
    UPDATE course_join_link SET revoked_at = now(), revoked_by_member_id = '00000000-0000-0000-0000-000000000051'
    WHERE id = '00000000-0000-0000-0000-0000000001a1' $q$);
SELECT pg_temp.ok('member_invite, which makes links, defaults to denied on both tables', $q$
    DO $chk$
    BEGIN
        IF EXISTS (SELECT 1 FROM course_member WHERE id = '00000000-0000-0000-0000-00000000005f' AND perm_member_invite <> 'denied')
           OR EXISTS (SELECT 1 FROM permission_preset WHERE id = '00000000-0000-0000-0000-000000000091'
                      AND perm_member_invite <> 'denied') THEN
            RAISE EXCEPTION 'member_invite does not default to denied';
        END IF;
    END $chk$ $q$);

-- Departments: a tree, and its administrators --------------------------------
-- 2a1 … 2a8 a chain, 2a1 at the top · 2b1 > 2b2 > 2b3 a department and what is
-- beneath it · 2c1, 2c2 two that would loop · da1, da2 Sato's appointments at 21
SELECT pg_temp.fails('a department is not its own parent', '23514', $q$
    INSERT INTO department (id, name, parent_id) VALUES ('00000000-0000-0000-0000-0000000002d1', 'Itself', '00000000-0000-0000-0000-0000000002d1') $q$);
SELECT pg_temp.fails('nor under a department beneath it', '23514', $q$
    INSERT INTO department (id, name) VALUES ('00000000-0000-0000-0000-0000000002c1', 'A');
    INSERT INTO department (id, name, parent_id) VALUES ('00000000-0000-0000-0000-0000000002c2', 'B', '00000000-0000-0000-0000-0000000002c1');
    UPDATE department SET parent_id = '00000000-0000-0000-0000-0000000002c2' WHERE id = '00000000-0000-0000-0000-0000000002c1' $q$);
SELECT pg_temp.fails('nor under itself, once it exists', '23514', $q$
    UPDATE department SET parent_id = id WHERE id = '00000000-0000-0000-0000-000000000021' $q$);
SELECT pg_temp.ok('a tree eight levels deep', $q$
    INSERT INTO department (id, name) VALUES ('00000000-0000-0000-0000-0000000002a1', 'Level 1');
    INSERT INTO department (id, name, parent_id) VALUES ('00000000-0000-0000-0000-0000000002a2', 'Level 2', '00000000-0000-0000-0000-0000000002a1');
    INSERT INTO department (id, name, parent_id) VALUES ('00000000-0000-0000-0000-0000000002a3', 'Level 3', '00000000-0000-0000-0000-0000000002a2');
    INSERT INTO department (id, name, parent_id) VALUES ('00000000-0000-0000-0000-0000000002a4', 'Level 4', '00000000-0000-0000-0000-0000000002a3');
    INSERT INTO department (id, name, parent_id) VALUES ('00000000-0000-0000-0000-0000000002a5', 'Level 5', '00000000-0000-0000-0000-0000000002a4');
    INSERT INTO department (id, name, parent_id) VALUES ('00000000-0000-0000-0000-0000000002a6', 'Level 6', '00000000-0000-0000-0000-0000000002a5');
    INSERT INTO department (id, name, parent_id) VALUES ('00000000-0000-0000-0000-0000000002a7', 'Level 7', '00000000-0000-0000-0000-0000000002a6');
    INSERT INTO department (id, name, parent_id) VALUES ('00000000-0000-0000-0000-0000000002a8', 'Level 8', '00000000-0000-0000-0000-0000000002a7'); $q$);
SELECT pg_temp.fails('but not nine', '23514', $q$
    INSERT INTO department (id, name, parent_id) VALUES ('00000000-0000-0000-0000-0000000002a9', 'Level 9', '00000000-0000-0000-0000-0000000002a8') $q$);
SELECT pg_temp.ok('a department with two levels beneath it', $q$
    INSERT INTO department (id, name) VALUES ('00000000-0000-0000-0000-0000000002b1', 'Faculty');
    INSERT INTO department (id, name, parent_id) VALUES ('00000000-0000-0000-0000-0000000002b2', 'School', '00000000-0000-0000-0000-0000000002b1');
    INSERT INTO department (id, name, parent_id) VALUES ('00000000-0000-0000-0000-0000000002b3', 'Department', '00000000-0000-0000-0000-0000000002b2') $q$);
SELECT pg_temp.fails('moved under the sixth level, with what is beneath it, it would make nine', '23514', $q$
    UPDATE department SET parent_id = '00000000-0000-0000-0000-0000000002a6' WHERE id = '00000000-0000-0000-0000-0000000002b1' $q$);
SELECT pg_temp.ok('under the fifth it makes eight, and back at the top one again', $q$
    UPDATE department SET parent_id = '00000000-0000-0000-0000-0000000002a5' WHERE id = '00000000-0000-0000-0000-0000000002b1';
    UPDATE department SET parent_id = NULL WHERE id = '00000000-0000-0000-0000-0000000002b1' $q$);
SELECT pg_temp.ok('a person administers a department, appointed by someone else', $q$
    INSERT INTO department_admin (id, dept_id, actor_id, appointed_by_actor_id)
    VALUES ('00000000-0000-0000-0000-000000000da1', '00000000-0000-0000-0000-000000000021', '00000000-0000-0000-0000-000000000034', '00000000-0000-0000-0000-000000000032') $q$);
SELECT pg_temp.fails('an agent administers nothing', '23514', $q$
    INSERT INTO department_admin (dept_id, actor_id, appointed_by_actor_id)
    VALUES ('00000000-0000-0000-0000-000000000021', '00000000-0000-0000-0000-000000000036', '00000000-0000-0000-0000-000000000032') $q$);
SELECT pg_temp.fails('nor does the system actor', '23514', $q$
    INSERT INTO department_admin (dept_id, actor_id, appointed_by_actor_id)
    VALUES ('00000000-0000-0000-0000-000000000021', '00000000-0000-0000-0000-000000000033', '00000000-0000-0000-0000-000000000032') $q$);
SELECT pg_temp.fails('one live appointment per person and department', '23505', $q$
    INSERT INTO department_admin (dept_id, actor_id, appointed_by_actor_id)
    VALUES ('00000000-0000-0000-0000-000000000021', '00000000-0000-0000-0000-000000000034', '00000000-0000-0000-0000-000000000031') $q$);
SELECT pg_temp.fails('nobody appoints themselves', '23514', $q$
    INSERT INTO department_admin (dept_id, actor_id, appointed_by_actor_id)
    VALUES ('00000000-0000-0000-0000-0000000002b1', '00000000-0000-0000-0000-000000000034', '00000000-0000-0000-0000-000000000034') $q$);
SELECT pg_temp.fails('an ending says who ended it', '23514', $q$
    UPDATE department_admin SET removed_at = now() WHERE id = '00000000-0000-0000-0000-000000000da1' $q$);
SELECT pg_temp.fails('an appointment is kept: not deleted', '23001', $q$
    DELETE FROM department_admin WHERE id = '00000000-0000-0000-0000-000000000da1' $q$);
SELECT pg_temp.fails('nor moved to another department', '23001', $q$
    UPDATE department_admin SET dept_id = '00000000-0000-0000-0000-0000000002b1' WHERE id = '00000000-0000-0000-0000-000000000da1' $q$);
SELECT pg_temp.fails('nor said to be someone else''s doing', '23001', $q$
    UPDATE department_admin SET appointed_by_actor_id = '00000000-0000-0000-0000-000000000031' WHERE id = '00000000-0000-0000-0000-000000000da1' $q$);
SELECT pg_temp.ok('an appointment ends', $q$
    UPDATE department_admin SET removed_at = now(), removed_by_actor_id = '00000000-0000-0000-0000-000000000032' WHERE id = '00000000-0000-0000-0000-000000000da1' $q$);
SELECT pg_temp.fails('and stays ended', '23001', $q$
    UPDATE department_admin SET removed_at = NULL, removed_by_actor_id = NULL WHERE id = '00000000-0000-0000-0000-000000000da1' $q$);
SELECT pg_temp.ok('the same person is appointed again, in a new row', $q$
    INSERT INTO department_admin (id, dept_id, actor_id, appointed_by_actor_id)
    VALUES ('00000000-0000-0000-0000-000000000da2', '00000000-0000-0000-0000-000000000021', '00000000-0000-0000-0000-000000000034', '00000000-0000-0000-0000-000000000031') $q$);

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
SELECT pg_temp.ok('file-only version: its one file, written with it', $q$
    INSERT INTO document_version (id, document_id, seq, author_member_id)
    VALUES ('00000000-0000-0000-0000-0000000000f2', '00000000-0000-0000-0000-0000000000e2', 1, '00000000-0000-0000-0000-000000000051');
    INSERT INTO document_version_file (version_id, document_id, position, filename, storage_key, content_type, byte_size)
    VALUES ('00000000-0000-0000-0000-0000000000f2', '00000000-0000-0000-0000-0000000000e2', 1, 'lecture2.pdf', 'k/lecture2.pdf',
            'application/pdf', 1024);
    SET CONSTRAINTS ALL IMMEDIATE;
    SET CONSTRAINTS ALL DEFERRED $q$);
SELECT pg_temp.fails('version needs text or a file', '23514', $q$
    INSERT INTO document_version (document_id, seq, author_member_id)
    VALUES ('00000000-0000-0000-0000-0000000000e1', 2, '00000000-0000-0000-0000-000000000051') $q$);
SELECT pg_temp.fails('a version keeps no file of its own, since 0027: its files are document_version_file''s', '42703', $q$
    INSERT INTO document_version (document_id, seq, storage_key, content_type, byte_size, author_member_id)
    VALUES ('00000000-0000-0000-0000-0000000000e1', 2, 'k/x', 'application/pdf', 1, '00000000-0000-0000-0000-000000000051') $q$);
SELECT pg_temp.ok('published pointer moves to own version', $q$
    UPDATE document SET published_version_id = '00000000-0000-0000-0000-0000000000f1'
    WHERE id = '00000000-0000-0000-0000-0000000000e1' $q$);
SELECT pg_temp.fails('published pointer cannot name another document''s version', '23503', $q$
    UPDATE document SET published_version_id = '00000000-0000-0000-0000-0000000000f2'
    WHERE id = '00000000-0000-0000-0000-0000000000e1' $q$);
SELECT pg_temp.fails('versions are append-only', '23001', $q$
    UPDATE document_version SET body_md = 'rewritten' WHERE id = '00000000-0000-0000-0000-0000000000f1' $q$);
SELECT pg_temp.fails('a version is never deleted', '23001', $q$
    DELETE FROM document_version WHERE id = '00000000-0000-0000-0000-0000000000f2' $q$);
SELECT pg_temp.fails('a purge says who made it and why', '23514', $q$
    UPDATE document_version SET purged_at = now() WHERE id = '00000000-0000-0000-0000-0000000000f2' $q$);
SELECT pg_temp.fails('a purge takes the text and not only the date', '23001', $q$
    UPDATE document_version SET purged_at = now(), purged_by_actor_id = '00000000-0000-0000-0000-000000000032',
                                purge_reason = 'personal data' WHERE id = '00000000-0000-0000-0000-0000000000f1' $q$);
SELECT pg_temp.fails('nor does it move the version', '23001', $q$
    UPDATE document_version SET seq = 9, purged_at = now(),
                                purged_by_actor_id = '00000000-0000-0000-0000-000000000032', purge_reason = 'personal data'
    WHERE id = '00000000-0000-0000-0000-0000000000f2' $q$);
SELECT pg_temp.ok('a version is purged: its file goes, where it was and who purged it stay', $q$
    UPDATE document_version SET purged_at = now(),
                                purged_by_actor_id = '00000000-0000-0000-0000-000000000032', purge_reason = 'personal data'
    WHERE id = '00000000-0000-0000-0000-0000000000f2';
    DO $chk$
    BEGIN
        IF NOT EXISTS (SELECT 1 FROM document_version WHERE id = '00000000-0000-0000-0000-0000000000f2'
                       AND seq = 1 AND purged_by_actor_id = '00000000-0000-0000-0000-000000000032') THEN
            RAISE EXCEPTION 'the tombstone does not say what was there';
        END IF;
        IF EXISTS (SELECT 1 FROM document_version_file WHERE version_id = '00000000-0000-0000-0000-0000000000f2') THEN
            RAISE EXCEPTION 'the purged version''s file is still recorded';
        END IF;
    END $chk$ $q$);
SELECT pg_temp.fails('a purged version is purged once', '23001', $q$
    UPDATE document_version SET purge_reason = 'something else' WHERE id = '00000000-0000-0000-0000-0000000000f2' $q$);
SELECT pg_temp.fails('a purged version holds nothing of what it held', '23514', $q$
    INSERT INTO document_version (document_id, seq, body_md, author_member_id, purged_at, purged_by_actor_id, purge_reason)
    VALUES ('00000000-0000-0000-0000-0000000000e2', 2, 'still here', '00000000-0000-0000-0000-000000000051',
            now(), '00000000-0000-0000-0000-000000000032', 'personal data') $q$);
SELECT pg_temp.fails('a document is purged only once it is archived', '23514', $q$
    UPDATE document SET purged_at = now(), purged_by_actor_id = '00000000-0000-0000-0000-000000000032', purge_reason = 'personal data'
    WHERE id = '00000000-0000-0000-0000-0000000000e2' $q$);
SELECT pg_temp.ok('a document is purged, archived', $q$
    UPDATE document SET status = 'archived', purged_at = now(), purged_by_actor_id = '00000000-0000-0000-0000-000000000032',
                        purge_reason = 'personal data'
    WHERE id = '00000000-0000-0000-0000-0000000000e2' $q$);
SELECT pg_temp.fails('a purged document stays archived', '23514', $q$
    UPDATE document SET status = 'active' WHERE id = '00000000-0000-0000-0000-0000000000e2' $q$);
SELECT pg_temp.fails('and stays purged, as it was purged', '23001', $q$
    UPDATE document SET purged_at = NULL, purged_by_actor_id = NULL, purge_reason = NULL
    WHERE id = '00000000-0000-0000-0000-0000000000e2' $q$);
SELECT pg_temp.ok('a purged document may still be renamed', $q$
    UPDATE document SET title = 'Removed' WHERE id = '00000000-0000-0000-0000-0000000000e2' $q$);
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
SELECT pg_temp.fails('a submitted file is never purged', '23514', $q$
    INSERT INTO document (course_id, kind, title, submission_id, status, purged_at, purged_by_actor_id, purge_reason)
    VALUES ('00000000-0000-0000-0000-000000000041', 'submission', 'essay.pdf', '00000000-0000-0000-0000-0000000000a1',
            'archived', now(), '00000000-0000-0000-0000-000000000032', 'personal data') $q$);

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
-- payload_hash has no default in the schema: the application always computes
-- it. A default for the rest of this transaction keeps each fixture below
-- about the one rule it tests. Rolled back with everything else.
ALTER TABLE action ALTER COLUMN payload_hash SET DEFAULT repeat('0', 64);

SELECT pg_temp.fails('payload hash is required', '23502', $q$
    INSERT INTO action (actor_id, action_type, target_type, idempotency_key, payload_hash, authz_result, status)
    VALUES ('00000000-0000-0000-0000-000000000034', 'grade.submit', 'submission', 'k-nohash', NULL, 'denied', 'denied') $q$);
SELECT pg_temp.fails('payload hash is 64 lowercase hex characters', '23514', $q$
    INSERT INTO action (actor_id, action_type, target_type, idempotency_key, payload_hash, authz_result, status)
    VALUES ('00000000-0000-0000-0000-000000000034', 'grade.submit', 'submission', 'k-badhash', 'ABC', 'denied', 'denied') $q$);
SELECT pg_temp.fails('denied authorization cannot carry another status', '23514', $q$
    INSERT INTO action (actor_id, action_type, target_type, idempotency_key, authz_result, status, executed_at)
    VALUES ('00000000-0000-0000-0000-000000000034', 'grade.submit', 'submission', 'k-d1', 'denied', 'executed', now()) $q$);
SELECT pg_temp.fails('denied status needs a denied authorization', '23514', $q$
    INSERT INTO action (actor_id, action_type, target_type, idempotency_key, authz_result, status)
    VALUES ('00000000-0000-0000-0000-000000000034', 'grade.submit', 'submission', 'k-d2', 'autonomous', 'denied') $q$);
SELECT pg_temp.fails('only confirm_required actions are proposed', '23514', $q$
    INSERT INTO action (actor_id, action_type, target_type, idempotency_key, authz_result, status)
    VALUES ('00000000-0000-0000-0000-000000000034', 'grade.submit', 'submission', 'k-p1', 'autonomous', 'proposed') $q$);
SELECT pg_temp.fails('only confirm_required actions have a decider', '23514', $q$
    INSERT INTO action (actor_id, course_id, member_id, action_type, target_type, idempotency_key, authz_result, status,
                        executed_at, decided_by_member_id, decided_at)
    VALUES ('00000000-0000-0000-0000-000000000036', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000053',
            'grade.submit', 'submission', 'k-p2', 'autonomous', 'executed', now(), '00000000-0000-0000-0000-000000000051', now()) $q$);
SELECT pg_temp.fails('only pending_review actions enter review', '23514', $q$
    INSERT INTO action (actor_id, action_type, target_type, idempotency_key, authz_result, status, executed_at, review_state)
    VALUES ('00000000-0000-0000-0000-000000000034', 'grade.submit', 'submission', 'k-r1', 'autonomous', 'executed', now(), 'pending') $q$);
SELECT pg_temp.fails('a failed action is not reviewed', '23514', $q$
    INSERT INTO action (actor_id, action_type, target_type, idempotency_key, authz_result, status, review_state)
    VALUES ('00000000-0000-0000-0000-000000000034', 'grade.submit', 'submission', 'k-r2', 'pending_review', 'failed', 'pending') $q$);
SELECT pg_temp.fails('executed needs executed_at', '23514', $q$
    INSERT INTO action (actor_id, action_type, target_type, idempotency_key, authz_result, status)
    VALUES ('00000000-0000-0000-0000-000000000034', 'grade.submit', 'submission', 'k-e1', 'autonomous', 'executed') $q$);
SELECT pg_temp.fails('executed_at only on executed', '23514', $q$
    INSERT INTO action (actor_id, action_type, target_type, idempotency_key, authz_result, status, executed_at)
    VALUES ('00000000-0000-0000-0000-000000000034', 'grade.submit', 'submission', 'k-e2', 'autonomous', 'failed', now()) $q$);
SELECT pg_temp.ok('pending_review action executes and waits for review, with its result stored', $q$
    INSERT INTO action (actor_id, course_id, member_id, action_type, target_type, idempotency_key, authz_result, status,
                        executed_at, review_state, result)
    VALUES ('00000000-0000-0000-0000-000000000036', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000053',
            'grade.submit', 'submission', 'k-pr', 'pending_review', 'executed', now(), 'pending', '{"grade_id": "x"}') $q$);
SELECT pg_temp.ok('proposal cancelled with a reason', $q$
    INSERT INTO action (actor_id, course_id, member_id, action_type, target_type, idempotency_key, authz_result, status, result)
    VALUES ('00000000-0000-0000-0000-000000000036', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000053',
            'grade.submit', 'submission', 'k-cancel', 'confirm_required', 'cancelled', '{"error": {"code": "proposal_expired"}}') $q$);

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
SELECT pg_temp.fails('an action is authorized by a platform role or a department''s appointment', '23514', $q$
    INSERT INTO action (actor_id, action_type, target_type, idempotency_key, authz_result, status, executed_at, authority)
    VALUES ('00000000-0000-0000-0000-000000000032', 'department.create', 'department', 'k-auth1', 'autonomous', 'executed', now(), 'course') $q$);
SELECT pg_temp.fails('only a department''s appointment names a department', '23514', $q$
    INSERT INTO action (actor_id, action_type, target_type, idempotency_key, authz_result, status, executed_at,
                        authority, authority_dept_id)
    VALUES ('00000000-0000-0000-0000-000000000032', 'department.create', 'department', 'k-auth2', 'autonomous', 'executed', now(),
            'platform', '00000000-0000-0000-0000-000000000021') $q$);
SELECT pg_temp.fails('the department it names exists', '23503', $q$
    INSERT INTO action (actor_id, action_type, target_type, idempotency_key, authz_result, status, executed_at,
                        authority, authority_dept_id)
    VALUES ('00000000-0000-0000-0000-000000000034', 'department.create', 'department', 'k-auth3', 'autonomous', 'executed', now(),
            'department', '00000000-0000-0000-0000-0000000000ff') $q$);
SELECT pg_temp.ok('in a seat''s capacity, a platform role''s, or a department''s by its appointment', $q$
    INSERT INTO action (actor_id, action_type, target_type, idempotency_key, authz_result, status, executed_at)
    VALUES ('00000000-0000-0000-0000-000000000034', 'grade.submit', 'submission', 'k-auth4', 'autonomous', 'executed', now());
    INSERT INTO action (actor_id, action_type, target_type, idempotency_key, authz_result, status, executed_at, authority)
    VALUES ('00000000-0000-0000-0000-000000000032', 'department.create', 'department', 'k-auth5', 'autonomous', 'executed', now(), 'platform');
    INSERT INTO action (actor_id, action_type, target_type, idempotency_key, authz_result, status, executed_at,
                        authority, authority_dept_id)
    VALUES ('00000000-0000-0000-0000-000000000034', 'department.create', 'department', 'k-auth6', 'autonomous', 'executed', now(),
            'department', '00000000-0000-0000-0000-000000000021');
    INSERT INTO action (actor_id, action_type, target_type, idempotency_key, authz_result, status, executed_at, authority)
    VALUES ('00000000-0000-0000-0000-000000000034', 'actor.invite_new', 'actor', 'k-auth7', 'autonomous', 'executed', now(), 'department') $q$);

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
SELECT pg_temp.fails('an override of a total says who made it, when and why', '23514', $q$
    INSERT INTO grade (student_member_id, component_id, origin, score, grader_member_id, created_by_action_id,
                       posted_at, posted_by_member_id, override_score)
    VALUES ('00000000-0000-0000-0000-000000000058', '00000000-0000-0000-0000-000000000061', 'computed', 71.5,
            '00000000-0000-0000-0000-000000000051', '00000000-0000-0000-0000-0000000000b1', now(),
            '00000000-0000-0000-0000-000000000051', 75) $q$);
SELECT pg_temp.fails('only a computed total is overridden', '23514', $q$
    INSERT INTO grade (student_member_id, component_id, origin, score, grader_member_id, created_by_action_id,
                       override_score, override_reason, override_by_member_id, overridden_at)
    VALUES ('00000000-0000-0000-0000-000000000058', '00000000-0000-0000-0000-000000000064', 'entered', 60,
            '00000000-0000-0000-0000-000000000051', '00000000-0000-0000-0000-0000000000b1',
            65, 'generous', '00000000-0000-0000-0000-000000000051', now()) $q$);
SELECT pg_temp.fails('an override is not negative', '23514', $q$
    INSERT INTO grade (student_member_id, component_id, origin, score, grader_member_id, created_by_action_id,
                       posted_at, posted_by_member_id, override_score, override_reason, override_by_member_id, overridden_at)
    VALUES ('00000000-0000-0000-0000-000000000058', '00000000-0000-0000-0000-000000000061', 'computed', 71.5,
            '00000000-0000-0000-0000-000000000051', '00000000-0000-0000-0000-0000000000b1', now(),
            '00000000-0000-0000-0000-000000000051', -1, 'harsh', '00000000-0000-0000-0000-000000000051', now()) $q$);
SELECT pg_temp.fails('an override has a reason of some length', '23514', $q$
    INSERT INTO grade (student_member_id, component_id, origin, score, grader_member_id, created_by_action_id,
                       posted_at, posted_by_member_id, override_score, override_reason, override_by_member_id, overridden_at)
    VALUES ('00000000-0000-0000-0000-000000000058', '00000000-0000-0000-0000-000000000061', 'computed', 71.5,
            '00000000-0000-0000-0000-000000000051', '00000000-0000-0000-0000-0000000000b1', now(),
            '00000000-0000-0000-0000-000000000051', 75, '', '00000000-0000-0000-0000-000000000051', now()) $q$);
SELECT pg_temp.ok('a computed total with an override beside it', $q$
    INSERT INTO grade (student_member_id, component_id, origin, score, grader_member_id, created_by_action_id,
                       posted_at, posted_by_member_id, override_score, override_reason, override_by_member_id, overridden_at)
    VALUES ('00000000-0000-0000-0000-000000000058', '00000000-0000-0000-0000-000000000061', 'computed', 71.5,
            '00000000-0000-0000-0000-000000000051', '00000000-0000-0000-0000-0000000000b1', now(),
            '00000000-0000-0000-0000-000000000051', 75, 'the lab work was strong', '00000000-0000-0000-0000-000000000051', now()) $q$);
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
SELECT pg_temp.ok('event names the student and assignment it belongs to', $q$
    INSERT INTO event (type, course_id, subject_type, subject_id, student_member_id, assignment_id)
    VALUES ('submission.submitted', '00000000-0000-0000-0000-000000000041', 'submission', '00000000-0000-0000-0000-0000000000a1',
            '00000000-0000-0000-0000-000000000052', '00000000-0000-0000-0000-000000000071') $q$);
SELECT pg_temp.fails('event student must be a real member', '23503', $q$
    INSERT INTO event (type, course_id, subject_type, student_member_id)
    VALUES ('submission.submitted', '00000000-0000-0000-0000-000000000041', 'submission', '00000000-0000-0000-0000-0000000000ff') $q$);
SELECT pg_temp.fails('event assignment must be a real assignment', '23503', $q$
    INSERT INTO event (type, course_id, subject_type, assignment_id)
    VALUES ('submission.submitted', '00000000-0000-0000-0000-000000000041', 'submission', '00000000-0000-0000-0000-0000000000ff') $q$);
SELECT pg_temp.fails('event scope columns are frozen with the rest of the row', '23001', $q$
    UPDATE event SET student_member_id = NULL $q$);
SELECT pg_temp.fails('events are append-only', '23001', $q$
    DELETE FROM event $q$);

-- Conversations ----------------------------------------------------------------
-- c1 Yuki (52) asks the grader (53) in A · c11, c12 its messages · c2 Yuki asks the grader again
SELECT pg_temp.ok('a member opens a conversation with another', $q$
    INSERT INTO conversation (id, course_id, opener_member_id, respondent_member_id, title)
    VALUES ('00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000052',
            '00000000-0000-0000-0000-000000000053', 'HW1') $q$);
SELECT pg_temp.fails('nobody opens a conversation with themselves', '23514', $q$
    INSERT INTO conversation (course_id, opener_member_id, respondent_member_id)
    VALUES ('00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000052', '00000000-0000-0000-0000-000000000052') $q$);
SELECT pg_temp.fails('both participants are seats of the conversation''s course', '23503', $q$
    INSERT INTO conversation (course_id, opener_member_id, respondent_member_id)
    VALUES ('00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000052', '00000000-0000-0000-0000-000000000054') $q$);
SELECT pg_temp.fails('conversation status must be open or closed', '23514', $q$
    INSERT INTO conversation (course_id, opener_member_id, respondent_member_id, status)
    VALUES ('00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000052', '00000000-0000-0000-0000-000000000053', 'paused') $q$);
SELECT pg_temp.fails('a title is at most 200 characters', '23514', $q$
    INSERT INTO conversation (course_id, opener_member_id, respondent_member_id, title)
    VALUES ('00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000052', '00000000-0000-0000-0000-000000000053', repeat('x', 201)) $q$);
SELECT pg_temp.fails('a title says something', '23514', $q$
    INSERT INTO conversation (course_id, opener_member_id, respondent_member_id, title)
    VALUES ('00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000052', '00000000-0000-0000-0000-000000000053', '') $q$);
SELECT pg_temp.fails('a closed reason is at most 500 characters', '23514', $q$
    INSERT INTO conversation (course_id, opener_member_id, respondent_member_id, status, closed_reason)
    VALUES ('00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000052', '00000000-0000-0000-0000-000000000053', 'closed', repeat('x', 501)) $q$);
SELECT pg_temp.fails('when the last message came and who wrote it are set together', '23514', $q$
    UPDATE conversation SET last_message_at = now() WHERE id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.fails('a conversation''s title never changes', '23001', $q$
    UPDATE conversation SET title = 'HW2' WHERE id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.fails('nor when it began', '23001', $q$
    UPDATE conversation SET created_at = now() - interval '1 day' WHERE id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.fails('an open conversation has no closed reason', '23514', $q$
    INSERT INTO conversation (course_id, opener_member_id, respondent_member_id, closed_reason)
    VALUES ('00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000052', '00000000-0000-0000-0000-000000000053', 'why') $q$);
SELECT pg_temp.fails('who spoke last is a participant', '23514', $q$
    UPDATE conversation SET last_message_at = now(), last_author_member_id = '00000000-0000-0000-0000-000000000051'
    WHERE id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.ok('the opener writes', $q$
    INSERT INTO conversation_message (id, conversation_id, course_id, seq, author_member_id, body, created_by_action_id)
    VALUES ('00000000-0000-0000-0000-000000000c11', '00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041', 1,
            '00000000-0000-0000-0000-000000000052', 'Why 8/10?', '00000000-0000-0000-0000-0000000000b1') $q$);
SELECT pg_temp.fails('only the two participants write', '23514', $q$
    INSERT INTO conversation_message (conversation_id, course_id, seq, author_member_id, body, created_by_action_id)
    VALUES ('00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041', 2,
            '00000000-0000-0000-0000-000000000051', 'Because.', '00000000-0000-0000-0000-0000000000b1') $q$);
SELECT pg_temp.fails('a conversation has one message at each seq', '23505', $q$
    INSERT INTO conversation_message (conversation_id, course_id, seq, author_member_id, body, created_by_action_id)
    VALUES ('00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041', 1,
            '00000000-0000-0000-0000-000000000052', 'Again', '00000000-0000-0000-0000-0000000000b1') $q$);
SELECT pg_temp.fails('a message''s seq counts from 1', '23514', $q$
    INSERT INTO conversation_message (conversation_id, course_id, seq, author_member_id, body, created_by_action_id)
    VALUES ('00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041', 0,
            '00000000-0000-0000-0000-000000000052', 'Before the first', '00000000-0000-0000-0000-0000000000b1') $q$);
SELECT pg_temp.fails('a message says something', '23514', $q$
    INSERT INTO conversation_message (conversation_id, course_id, seq, author_member_id, body, created_by_action_id)
    VALUES ('00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041', 2,
            '00000000-0000-0000-0000-000000000052', '', '00000000-0000-0000-0000-0000000000b1') $q$);
SELECT pg_temp.fails('a message is at most 20000 characters', '23514', $q$
    INSERT INTO conversation_message (conversation_id, course_id, seq, author_member_id, body, created_by_action_id)
    VALUES ('00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041', 2,
            '00000000-0000-0000-0000-000000000052', repeat('x', 20001), '00000000-0000-0000-0000-0000000000b1') $q$);
SELECT pg_temp.fails('a message names the action that wrote it', '23502', $q$
    INSERT INTO conversation_message (conversation_id, course_id, seq, author_member_id, body)
    VALUES ('00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041', 2,
            '00000000-0000-0000-0000-000000000052', 'Hello') $q$);
SELECT pg_temp.fails('a message is in its conversation''s course', '23503', $q$
    INSERT INTO conversation_message (conversation_id, course_id, seq, author_member_id, body, created_by_action_id)
    VALUES ('00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000042', 2,
            '00000000-0000-0000-0000-000000000052', 'Hello', '00000000-0000-0000-0000-0000000000b1') $q$);
SELECT pg_temp.ok('the respondent answers the opener''s message', $q$
    INSERT INTO conversation_message (id, conversation_id, course_id, seq, author_member_id, in_reply_to_message_id, body, created_by_action_id)
    VALUES ('00000000-0000-0000-0000-000000000c12', '00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041', 2,
            '00000000-0000-0000-0000-000000000053', '00000000-0000-0000-0000-000000000c11', 'The evidence is thin.',
            '00000000-0000-0000-0000-0000000000b1') $q$);
SELECT pg_temp.fails('an answer is the respondent''s', '23514', $q$
    INSERT INTO conversation_message (conversation_id, course_id, seq, author_member_id, in_reply_to_message_id, body, created_by_action_id)
    VALUES ('00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041', 3,
            '00000000-0000-0000-0000-000000000052', '00000000-0000-0000-0000-000000000c11', 'I answer myself',
            '00000000-0000-0000-0000-0000000000b1') $q$);
SELECT pg_temp.fails('an answer answers a message of the opener''s', '23514', $q$
    INSERT INTO conversation_message (conversation_id, course_id, seq, author_member_id, in_reply_to_message_id, body, created_by_action_id)
    VALUES ('00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041', 3,
            '00000000-0000-0000-0000-000000000053', '00000000-0000-0000-0000-000000000c12', 'And another thing',
            '00000000-0000-0000-0000-0000000000b1') $q$);
SELECT pg_temp.fails('an answer answers a message of its own conversation', '23503', $q$
    INSERT INTO conversation (id, course_id, opener_member_id, respondent_member_id)
    VALUES ('00000000-0000-0000-0000-0000000000c2', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000052',
            '00000000-0000-0000-0000-000000000053');
    INSERT INTO conversation_message (conversation_id, course_id, seq, author_member_id, in_reply_to_message_id, body, created_by_action_id)
    VALUES ('00000000-0000-0000-0000-0000000000c2', '00000000-0000-0000-0000-000000000041', 1,
            '00000000-0000-0000-0000-000000000053', '00000000-0000-0000-0000-000000000c11', 'Not yours to answer',
            '00000000-0000-0000-0000-0000000000b1') $q$);
SELECT pg_temp.fails('messages are append-only: no edit', '23001', $q$
    UPDATE conversation_message SET body = 'Why 9/10?' WHERE id = '00000000-0000-0000-0000-000000000c11' $q$);
SELECT pg_temp.fails('messages are append-only: no delete', '23001', $q$
    DELETE FROM conversation_message WHERE id = '00000000-0000-0000-0000-000000000c12' $q$);
SELECT pg_temp.fails('messages are not truncated', '23001', $q$
    TRUNCATE conversation_message CASCADE $q$);
SELECT pg_temp.fails('a conversation''s participants never change', '23001', $q$
    UPDATE conversation SET respondent_member_id = '00000000-0000-0000-0000-000000000051' WHERE id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.fails('a conversation is kept, not deleted', '23001', $q$
    DELETE FROM conversation WHERE id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.ok('the author retracts a message', $q$
    INSERT INTO conversation_message_retraction (message_id, course_id, retracted_by_member_id, created_by_action_id, reason)
    VALUES ('00000000-0000-0000-0000-000000000c11', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000052',
            '00000000-0000-0000-0000-0000000000b1', 'Wrong assignment') $q$);
SELECT pg_temp.fails('a message is retracted once', '23505', $q$
    INSERT INTO conversation_message_retraction (message_id, course_id, retracted_by_member_id, created_by_action_id)
    VALUES ('00000000-0000-0000-0000-000000000c11', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000051',
            '00000000-0000-0000-0000-0000000000b1') $q$);
SELECT pg_temp.fails('a retraction is in its message''s course', '23503', $q$
    INSERT INTO conversation_message_retraction (message_id, course_id, retracted_by_member_id, created_by_action_id)
    VALUES ('00000000-0000-0000-0000-000000000c12', '00000000-0000-0000-0000-000000000042', '00000000-0000-0000-0000-000000000055',
            '00000000-0000-0000-0000-0000000000b1') $q$);
SELECT pg_temp.fails('whoever retracts is a seat of the message''s course', '23503', $q$
    INSERT INTO conversation_message_retraction (message_id, course_id, retracted_by_member_id, created_by_action_id)
    VALUES ('00000000-0000-0000-0000-000000000c12', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000055',
            '00000000-0000-0000-0000-0000000000b1') $q$);
SELECT pg_temp.fails('a retraction names the action that made it', '23502', $q$
    INSERT INTO conversation_message_retraction (message_id, course_id, retracted_by_member_id)
    VALUES ('00000000-0000-0000-0000-000000000c12', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000053') $q$);
SELECT pg_temp.fails('a retraction''s reason is at most 500 characters', '23514', $q$
    INSERT INTO conversation_message_retraction (message_id, course_id, retracted_by_member_id, created_by_action_id, reason)
    VALUES ('00000000-0000-0000-0000-000000000c12', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000053',
            '00000000-0000-0000-0000-0000000000b1', repeat('x', 501)) $q$);
SELECT pg_temp.fails('retractions are append-only: no edit', '23001', $q$
    UPDATE conversation_message_retraction SET reason = 'Another' $q$);
SELECT pg_temp.fails('retractions are append-only', '23001', $q$
    DELETE FROM conversation_message_retraction $q$);
SELECT pg_temp.fails('retractions are not truncated', '23001', $q$
    TRUNCATE conversation_message_retraction $q$);
SELECT pg_temp.ok('a participant closes the conversation', $q$
    UPDATE conversation SET status = 'closed', closed_reason = 'Answered' WHERE id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.fails('nobody writes in a closed conversation', '23514', $q$
    INSERT INTO conversation_message (conversation_id, course_id, seq, author_member_id, body, created_by_action_id)
    VALUES ('00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041', 3,
            '00000000-0000-0000-0000-000000000052', 'One more thing', '00000000-0000-0000-0000-0000000000b1') $q$);
SELECT pg_temp.fails('a closed conversation stays closed', '23001', $q$
    UPDATE conversation SET status = 'open', closed_reason = NULL WHERE id = '00000000-0000-0000-0000-0000000000c1' $q$);
-- How far each participant has read a conversation: a participant's, forward only.
SELECT pg_temp.ok('a participant keeps a place in a conversation, closed or not', $q$
    INSERT INTO conversation_read (conversation_id, course_id, member_id, last_read_seq)
    VALUES ('00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000052', 1) $q$);
SELECT pg_temp.fails('one place each', '23505', $q$
    INSERT INTO conversation_read (conversation_id, course_id, member_id, last_read_seq)
    VALUES ('00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000052', 2) $q$);
SELECT pg_temp.fails('only a participant keeps one', '23514', $q$
    INSERT INTO conversation_read (conversation_id, course_id, member_id, last_read_seq)
    VALUES ('00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000051', 1) $q$);
SELECT pg_temp.fails('a place is in the conversation''s course', '23503', $q$
    INSERT INTO conversation_read (conversation_id, course_id, member_id, last_read_seq)
    VALUES ('00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000042', '00000000-0000-0000-0000-000000000053', 1) $q$);
SELECT pg_temp.fails('a place is no earlier than nothing read', '23514', $q$
    INSERT INTO conversation_read (conversation_id, course_id, member_id, last_read_seq)
    VALUES ('00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000053', -1) $q$);
SELECT pg_temp.ok('a place moves forward', $q$
    UPDATE conversation_read SET last_read_seq = 2, read_at = now()
     WHERE conversation_id = '00000000-0000-0000-0000-0000000000c1' AND member_id = '00000000-0000-0000-0000-000000000052' $q$);
SELECT pg_temp.fails('and never back', '23001', $q$
    UPDATE conversation_read SET last_read_seq = 1
     WHERE conversation_id = '00000000-0000-0000-0000-0000000000c1' AND member_id = '00000000-0000-0000-0000-000000000052' $q$);
SELECT pg_temp.fails('whose place it is never changes', '23001', $q$
    UPDATE conversation_read SET member_id = '00000000-0000-0000-0000-000000000053'
     WHERE conversation_id = '00000000-0000-0000-0000-0000000000c1' AND member_id = '00000000-0000-0000-0000-000000000052' $q$);
-- An answer's draft: one per conversation, in its course, kept unlogged,
-- and held to the shape the application writes it in.
SELECT pg_temp.ok('drafts are kept in an unlogged table', $q$
    DO $d$ BEGIN
        IF (SELECT relpersistence FROM pg_class WHERE relname = 'conversation_draft' AND relkind = 'r') IS DISTINCT FROM 'u' THEN
            RAISE EXCEPTION 'conversation_draft is not unlogged';
        END IF;
    END $d$ $q$);
SELECT pg_temp.ok('a conversation has a draft: an attempt, its version, the text so far and its steps', $q$
    INSERT INTO conversation_draft (conversation_id, course_id, attempt, version, body, steps)
    VALUES ('00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041', 'a1', 1, 'So far',
            '[{"kind":"reading_document","target":"HW3","state":"done"},{"kind":"writing","state":"running"}]') $q$);
SELECT pg_temp.fails('one draft to a conversation', '23505', $q$
    INSERT INTO conversation_draft (conversation_id, course_id, attempt, version)
    VALUES ('00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041', 'a2', 1) $q$);
SELECT pg_temp.fails('a draft is in its conversation''s course', '23503', $q$
    UPDATE conversation_draft SET course_id = '00000000-0000-0000-0000-000000000042'
     WHERE conversation_id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.fails('a draft names its attempt', '23514', $q$
    UPDATE conversation_draft SET attempt = '' WHERE conversation_id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.fails('in 64 characters at most', '23514', $q$
    UPDATE conversation_draft SET attempt = repeat('a', 65) WHERE conversation_id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.fails('a draft''s version is 1 or more', '23514', $q$
    UPDATE conversation_draft SET version = 0 WHERE conversation_id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.fails('its text is 20000 characters at most', '23514', $q$
    UPDATE conversation_draft SET body = repeat('x', 20001) WHERE conversation_id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.ok('20000 are taken', $q$
    UPDATE conversation_draft SET body = repeat('x', 20000) WHERE conversation_id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.fails('its steps are a list', '23514', $q$
    UPDATE conversation_draft SET steps = '{"kind":"thinking","state":"running"}'
     WHERE conversation_id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.fails('of 20 steps at most', '23514', $q$
    UPDATE conversation_draft SET steps = (SELECT jsonb_agg('{"kind":"thinking","state":"done"}'::jsonb) FROM generate_series(1, 21))
     WHERE conversation_id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.ok('20 are taken', $q$
    UPDATE conversation_draft SET steps = (SELECT jsonb_agg('{"kind":"thinking","state":"done"}'::jsonb) FROM generate_series(1, 20))
     WHERE conversation_id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.fails('a step is of a kind there is', '23514', $q$
    UPDATE conversation_draft SET steps = '[{"kind":"dreaming","state":"running"}]'
     WHERE conversation_id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.fails('and running or done', '23514', $q$
    UPDATE conversation_draft SET steps = '[{"kind":"thinking"}]'
     WHERE conversation_id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.fails('and nothing else', '23514', $q$
    UPDATE conversation_draft SET steps = '[{"kind":"thinking","state":"done","body":"Ken''s grades"}]'
     WHERE conversation_id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.fails('a step is an object', '23514', $q$
    UPDATE conversation_draft SET steps = '["thinking"]' WHERE conversation_id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.fails('what a step is about is text', '23514', $q$
    UPDATE conversation_draft SET steps = '[{"kind":"tool","target":42,"state":"done"}]'
     WHERE conversation_id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.fails('of 120 characters at most', '23514', $q$
    UPDATE conversation_draft SET steps = jsonb_build_array(jsonb_build_object('kind', 'tool', 'state', 'done', 'target', repeat('x', 121)))
     WHERE conversation_id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.ok('120 are taken', $q$
    UPDATE conversation_draft SET steps = jsonb_build_array(jsonb_build_object('kind', 'tool', 'state', 'done', 'target', repeat('x', 120)))
     WHERE conversation_id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.fails('an attempt that is over keeps no text', '23514', $q$
    UPDATE conversation_draft SET done = true WHERE conversation_id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.ok('an attempt that is over keeps nothing but its end', $q$
    UPDATE conversation_draft SET done = true, body = NULL, steps = '[]' WHERE conversation_id = '00000000-0000-0000-0000-0000000000c1' $q$);
SELECT pg_temp.ok('a draft is deleted', $q$
    DELETE FROM conversation_draft WHERE conversation_id = '00000000-0000-0000-0000-0000000000c1' $q$);
-- A message's files: written with it, in its conversation and course, and
-- kept as they are. ca1, ca2 Yuki's two files on her question c11.
SELECT pg_temp.ok('a message carries files, in order', $q$
    INSERT INTO conversation_attachment (id, message_id, conversation_id, course_id, position, filename, storage_key,
                                         content_type, byte_size, checksum, created_at)
    SELECT '00000000-0000-0000-0000-000000000ca1', m.id, m.conversation_id, m.course_id, 1, 'essay draft.pdf', 'k/ca1',
           'application/pdf', 1024, 'sha256:00', m.created_at
    FROM conversation_message m WHERE m.id = '00000000-0000-0000-0000-000000000c11';
    INSERT INTO conversation_attachment (id, message_id, conversation_id, course_id, position, filename, storage_key,
                                         content_type, byte_size, created_at)
    SELECT '00000000-0000-0000-0000-000000000ca2', m.id, m.conversation_id, m.course_id, 2, '作業 3.docx', 'k/ca2',
           'application/vnd.openxmlformats-officedocument.wordprocessingml.document', 0, m.created_at
    FROM conversation_message m WHERE m.id = '00000000-0000-0000-0000-000000000c11' $q$);
SELECT pg_temp.fails('one file at each place', '23505', $q$
    INSERT INTO conversation_attachment (message_id, conversation_id, course_id, position, filename, storage_key, content_type, byte_size)
    VALUES ('00000000-0000-0000-0000-000000000c11', '00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041',
            2, 'again.pdf', 'k/ca3', 'application/pdf', 1) $q$);
SELECT pg_temp.fails('a file is attached once', '23505', $q$
    INSERT INTO conversation_attachment (message_id, conversation_id, course_id, position, filename, storage_key, content_type, byte_size)
    VALUES ('00000000-0000-0000-0000-000000000c12', '00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041',
            1, 'copy.pdf', 'k/ca1', 'application/pdf', 1) $q$);
SELECT pg_temp.fails('a file is in its message''s conversation', '23503', $q$
    INSERT INTO conversation_attachment (message_id, conversation_id, course_id, position, filename, storage_key, content_type, byte_size)
    VALUES ('00000000-0000-0000-0000-000000000c11', '00000000-0000-0000-0000-0000000000c2', '00000000-0000-0000-0000-000000000041',
            3, 'elsewhere.pdf', 'k/ca4', 'application/pdf', 1) $q$);
SELECT pg_temp.fails('and in its conversation''s course', '23503', $q$
    INSERT INTO conversation_attachment (message_id, conversation_id, course_id, position, filename, storage_key, content_type, byte_size)
    VALUES ('00000000-0000-0000-0000-000000000c11', '00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000042',
            3, 'elsewhere.pdf', 'k/ca4', 'application/pdf', 1) $q$);
SELECT pg_temp.fails('a file is written with its message, never added to it afterwards', '23514', $q$
    INSERT INTO conversation_attachment (message_id, conversation_id, course_id, position, filename, storage_key, content_type, byte_size,
                                         created_at)
    VALUES ('00000000-0000-0000-0000-000000000c11', '00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041',
            3, 'later.pdf', 'k/ca4', 'application/pdf', 1, now() + interval '1 minute') $q$);
SELECT pg_temp.fails('its place counts from 1', '23514', $q$
    INSERT INTO conversation_attachment (message_id, conversation_id, course_id, position, filename, storage_key, content_type, byte_size)
    VALUES ('00000000-0000-0000-0000-000000000c11', '00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041',
            0, 'first.pdf', 'k/ca4', 'application/pdf', 1) $q$);
SELECT pg_temp.fails('a file has a name', '23514', $q$
    INSERT INTO conversation_attachment (message_id, conversation_id, course_id, position, filename, storage_key, content_type, byte_size)
    VALUES ('00000000-0000-0000-0000-000000000c11', '00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041',
            3, '', 'k/ca4', 'application/pdf', 1) $q$);
SELECT pg_temp.fails('of 255 characters at most', '23514', $q$
    INSERT INTO conversation_attachment (message_id, conversation_id, course_id, position, filename, storage_key, content_type, byte_size)
    VALUES ('00000000-0000-0000-0000-000000000c11', '00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041',
            3, repeat('x', 252) || '.pdf', 'k/ca4', 'application/pdf', 1) $q$);
SELECT pg_temp.fails('a name, not a path', '23514', $q$
    INSERT INTO conversation_attachment (message_id, conversation_id, course_id, position, filename, storage_key, content_type, byte_size)
    VALUES ('00000000-0000-0000-0000-000000000c11', '00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041',
            3, 'home/yuki/essay.pdf', 'k/ca4', 'application/pdf', 1) $q$);
SELECT pg_temp.fails('nor a Windows one', '23514', $q$
    INSERT INTO conversation_attachment (message_id, conversation_id, course_id, position, filename, storage_key, content_type, byte_size)
    VALUES ('00000000-0000-0000-0000-000000000c11', '00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041',
            3, 'C:\essay.pdf', 'k/ca4', 'application/pdf', 1) $q$);
SELECT pg_temp.fails('on one line', '23514', $q$
    INSERT INTO conversation_attachment (message_id, conversation_id, course_id, position, filename, storage_key, content_type, byte_size)
    VALUES ('00000000-0000-0000-0000-000000000c11', '00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041',
            3, E'essay\n.pdf', 'k/ca4', 'application/pdf', 1) $q$);
SELECT pg_temp.fails('with nothing around it', '23514', $q$
    INSERT INTO conversation_attachment (message_id, conversation_id, course_id, position, filename, storage_key, content_type, byte_size)
    VALUES ('00000000-0000-0000-0000-000000000c11', '00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041',
            3, ' essay.pdf', 'k/ca4', 'application/pdf', 1) $q$);
SELECT pg_temp.fails('a file has a type', '23514', $q$
    INSERT INTO conversation_attachment (message_id, conversation_id, course_id, position, filename, storage_key, content_type, byte_size)
    VALUES ('00000000-0000-0000-0000-000000000c11', '00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041',
            3, 'essay.pdf', 'k/ca4', '', 1) $q$);
SELECT pg_temp.fails('and no size below nothing', '23514', $q$
    INSERT INTO conversation_attachment (message_id, conversation_id, course_id, position, filename, storage_key, content_type, byte_size)
    VALUES ('00000000-0000-0000-0000-000000000c11', '00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-000000000041',
            3, 'essay.pdf', 'k/ca4', 'application/pdf', -1) $q$);
SELECT pg_temp.fails('files are kept as they are: no rename', '23001', $q$
    UPDATE conversation_attachment SET filename = 'final.pdf' WHERE id = '00000000-0000-0000-0000-000000000ca1' $q$);
SELECT pg_temp.fails('no delete, retracted or not', '23001', $q$
    DELETE FROM conversation_attachment WHERE id = '00000000-0000-0000-0000-000000000ca2' $q$);
SELECT pg_temp.fails('no truncate', '23001', $q$
    TRUNCATE conversation_attachment CASCADE $q$);
-- Conversations are between a person and an agent: a person answers none.
SELECT pg_temp.fails('a conversation''s respondent is an agent''s seat, never a person''s', '23514', $q$
    INSERT INTO conversation (course_id, opener_member_id, respondent_member_id)
    VALUES ('00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000052', '00000000-0000-0000-0000-000000000051') $q$);
SELECT pg_temp.fails('not even a closed one', '23514', $q$
    INSERT INTO conversation (course_id, opener_member_id, respondent_member_id, status)
    VALUES ('00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000051', '00000000-0000-0000-0000-000000000058', 'closed') $q$);
SELECT pg_temp.ok('a person''s seat is written answering nothing, whatever it is told; an agent''s as it is told', $q$
    UPDATE course_member SET perm_conversation_answer = 'autonomous'
     WHERE id IN ('00000000-0000-0000-0000-000000000052', '00000000-0000-0000-0000-000000000053');
    DO $chk$
    BEGIN
        IF NOT EXISTS (SELECT 1 FROM course_member WHERE id = '00000000-0000-0000-0000-000000000052' AND perm_conversation_answer = 'denied')
           OR NOT EXISTS (SELECT 1 FROM course_member WHERE id = '00000000-0000-0000-0000-000000000053' AND perm_conversation_answer = 'autonomous') THEN
            RAISE EXCEPTION 'a person''s seat answers, or an agent''s was not written as it was told';
        END IF;
    END $chk$ $q$);
SELECT pg_temp.ok('a person is seated answering nothing, whatever the row says', $q$
    INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope, perm_conversation_answer)
    VALUES ('00000000-0000-0000-0000-0000000005c1', '00000000-0000-0000-0000-000000000042', '00000000-0000-0000-0000-000000000032',
            'ta', '00000000-0000-0000-0000-000000000032', 'all', 'all', 'autonomous');
    DO $chk$
    BEGIN
        IF NOT EXISTS (SELECT 1 FROM course_member WHERE id = '00000000-0000-0000-0000-0000000005c1' AND perm_conversation_answer = 'denied') THEN
            RAISE EXCEPTION 'a person was seated answering';
        END IF;
    END $chk$ $q$);

-- Memory ---------------------------------------------------------------------
-- 3b Sato's tutor, an agent he owns · 5e its seat in A, his delegate, answering the course
-- mx memory entries. Who made an entry and when, and the text's hash, are
-- the application's to write; defaults for the rest of this transaction keep
-- each check below about the one rule it tests. Rolled back with everything
-- else.
INSERT INTO actor (id, kind, display_name, owner_actor_id, created_by_actor_id, hosting)
VALUES ('00000000-0000-0000-0000-00000000003b', 'agent', 'Sato''s tutor', '00000000-0000-0000-0000-000000000034', '00000000-0000-0000-0000-000000000034', 'mcp');
INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope, principal_member_id, answers_course)
VALUES ('00000000-0000-0000-0000-00000000005e', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-00000000003b', 'assistant',
        '00000000-0000-0000-0000-000000000034', 'listed', 'listed', '00000000-0000-0000-0000-000000000051', true);
ALTER TABLE memory_entry
    ALTER COLUMN id SET DEFAULT gen_random_uuid(),
    ALTER COLUMN source SET DEFAULT 'agent',
    ALTER COLUMN created_by_actor_id SET DEFAULT '00000000-0000-0000-0000-00000000003b',
    ALTER COLUMN updated_by_actor_id SET DEFAULT '00000000-0000-0000-0000-00000000003b',
    ALTER COLUMN created_by_action_id SET DEFAULT '00000000-0000-0000-0000-0000000000b1',
    ALTER COLUMN updated_by_action_id SET DEFAULT '00000000-0000-0000-0000-0000000000b1',
    ALTER COLUMN created_at SET DEFAULT now(),
    ALTER COLUMN updated_at SET DEFAULT now();

SELECT pg_temp.ok('an agent keeps memory about its owner', $q$
    INSERT INTO memory_entry (id, holder_actor_id, scope, subject_actor_id, body, search_text, text_hash)
    VALUES ('00000000-0000-0000-0000-0000000000f1', '00000000-0000-0000-0000-00000000003b', 'owner',
            '00000000-0000-0000-0000-000000000034', 'Prefers worked examples.', 'prefers worked examples', '\x01') $q$);
SELECT pg_temp.ok('owner memory may say in which course it was learnt', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, course_id, subject_actor_id, body, text_hash)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'owner', '00000000-0000-0000-0000-000000000041',
            '00000000-0000-0000-0000-000000000034', 'Teaches CS101 on Mondays.', '\x02') $q$);
SELECT pg_temp.ok('an agent keeps memory about someone who asks it, in its seat', $q$
    INSERT INTO memory_entry (id, holder_actor_id, scope, course_id, holder_member_id, subject_actor_id, subject_member_id,
                              body, search_text, text_hash)
    VALUES ('00000000-0000-0000-0000-0000000000f2', '00000000-0000-0000-0000-00000000003b', 'asker', '00000000-0000-0000-0000-000000000041',
            '00000000-0000-0000-0000-00000000005e', '00000000-0000-0000-0000-000000000035', '00000000-0000-0000-0000-000000000052',
            'Finds recursion hard.', 'finds recursion hard', '\x01') $q$);
SELECT pg_temp.ok('an agent proposes to a course''s shared memory', $q$
    INSERT INTO memory_entry (id, holder_actor_id, scope, course_id, holder_member_id, status, body, text_hash)
    VALUES ('00000000-0000-0000-0000-0000000000f3', '00000000-0000-0000-0000-00000000003b', 'course', '00000000-0000-0000-0000-000000000041',
            '00000000-0000-0000-0000-00000000005e', 'proposed', 'HW3 is due Friday.', '\x01') $q$);
SELECT pg_temp.ok('course staff write a course''s shared memory, in force at once', $q$
    INSERT INTO memory_entry (id, holder_actor_id, scope, course_id, holder_member_id, body, text_hash, source, created_by_actor_id, updated_by_actor_id)
    VALUES ('00000000-0000-0000-0000-0000000000f4', '00000000-0000-0000-0000-00000000003b', 'course', '00000000-0000-0000-0000-000000000041',
            '00000000-0000-0000-0000-00000000005e', 'Office hours are on Tuesdays.', '\x02', 'staff',
            '00000000-0000-0000-0000-000000000034', '00000000-0000-0000-0000-000000000034') $q$);
SELECT pg_temp.ok('the bucket and the search vector are worked out from the row', $q$
    DO $chk$
    BEGIN
        IF (SELECT bucket FROM memory_entry WHERE id = '00000000-0000-0000-0000-0000000000f2')
               <> 'asker:00000000-0000-0000-0000-000000000052'
           OR (SELECT bucket FROM memory_entry WHERE id = '00000000-0000-0000-0000-0000000000f3')
               <> 'course:00000000-0000-0000-0000-00000000005e'
           OR (SELECT bucket FROM memory_entry WHERE id = '00000000-0000-0000-0000-0000000000f1') <> 'owner'
           OR NOT (SELECT search @@ to_tsquery('simple', 'recursion') FROM memory_entry WHERE id = '00000000-0000-0000-0000-0000000000f2') THEN
            RAISE EXCEPTION 'bucket or search not as the row says';
        END IF;
    END $chk$ $q$);
SELECT pg_temp.fails('a person keeps no memory', '23514', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, subject_actor_id, body, text_hash)
    VALUES ('00000000-0000-0000-0000-000000000035', 'owner', '00000000-0000-0000-0000-000000000035', 'Note to self.', '\x09') $q$);
SELECT pg_temp.fails('owner memory is about the agent''s owner', '23514', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, subject_actor_id, body, text_hash)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'owner', '00000000-0000-0000-0000-000000000035', 'Yuki is shy.', '\x09') $q$);
SELECT pg_temp.fails('an agent nobody owns keeps no owner memory', '23514', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, subject_actor_id, body, text_hash)
    VALUES ('00000000-0000-0000-0000-000000000036', 'owner', '00000000-0000-0000-0000-000000000034', 'Sato grades late.', '\x09') $q$);
SELECT pg_temp.fails('the holder''s seat is the holder''s', '23514', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, course_id, holder_member_id, body, text_hash)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'course', '00000000-0000-0000-0000-000000000041',
            '00000000-0000-0000-0000-000000000053', 'Borrowed seat.', '\x09') $q$);
SELECT pg_temp.fails('the subject''s seat is the subject''s', '23514', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, course_id, holder_member_id, subject_actor_id, subject_member_id, body, text_hash)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'asker', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-00000000005e',
            '00000000-0000-0000-0000-000000000037', '00000000-0000-0000-0000-000000000052', 'Ken, filed under Yuki.', '\x09') $q$);
SELECT pg_temp.fails('both seats are of the entry''s course', '23503', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, course_id, holder_member_id, subject_actor_id, subject_member_id, body, text_hash)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'asker', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-00000000005e',
            '00000000-0000-0000-0000-000000000037', '00000000-0000-0000-0000-000000000054', 'Ken, from section B.', '\x09') $q$);
SELECT pg_temp.fails('an entry names the action that wrote it', '23502', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, subject_actor_id, body, text_hash, created_by_action_id)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'owner', '00000000-0000-0000-0000-000000000034', 'Out of nowhere.', '\x09', NULL) $q$);

-- The shape of each scope.
SELECT pg_temp.fails('scope is owner, asker or course', '23514', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, course_id, holder_member_id, body, text_hash)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'team', '00000000-0000-0000-0000-000000000041',
            '00000000-0000-0000-0000-00000000005e', 'Team notes.', '\x09') $q$);
SELECT pg_temp.fails('status is active, proposed or rejected', '23514', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, course_id, holder_member_id, status, body, text_hash)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'course', '00000000-0000-0000-0000-000000000041',
            '00000000-0000-0000-0000-00000000005e', 'archived', 'Old news.', '\x09') $q$);
SELECT pg_temp.fails('source is agent, owner or staff', '23514', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, subject_actor_id, body, text_hash, source)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'owner', '00000000-0000-0000-0000-000000000034', 'Heard it somewhere.', '\x09', 'model') $q$);
SELECT pg_temp.fails('owner memory is kept in no seat', '23514', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, course_id, holder_member_id, subject_actor_id, body, text_hash)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'owner', '00000000-0000-0000-0000-000000000041',
            '00000000-0000-0000-0000-00000000005e', '00000000-0000-0000-0000-000000000034', 'In a seat.', '\x09') $q$);
SELECT pg_temp.fails('owner memory is never proposed', '23514', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, subject_actor_id, status, body, text_hash)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'owner', '00000000-0000-0000-0000-000000000034', 'proposed', 'For review.', '\x09') $q$);
SELECT pg_temp.fails('asker memory is kept in the agent''s seat', '23514', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, course_id, subject_actor_id, subject_member_id, body, text_hash)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'asker', '00000000-0000-0000-0000-000000000041',
            '00000000-0000-0000-0000-000000000035', '00000000-0000-0000-0000-000000000052', 'Seatless.', '\x09') $q$);
-- Without the seat a bucket is keyed on, there is no bucket to put it in.
SELECT pg_temp.fails('asker memory names whom it is about', '23502', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, course_id, holder_member_id, body, text_hash)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'asker', '00000000-0000-0000-0000-000000000041',
            '00000000-0000-0000-0000-00000000005e', 'About somebody.', '\x09') $q$);
SELECT pg_temp.fails('asker memory is never proposed', '23514', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, course_id, holder_member_id, subject_actor_id, subject_member_id, status, body, text_hash)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'asker', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-00000000005e',
            '00000000-0000-0000-0000-000000000035', '00000000-0000-0000-0000-000000000052', 'proposed', 'For review.', '\x09') $q$);
SELECT pg_temp.fails('shared memory is about nobody', '23514', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, course_id, holder_member_id, subject_actor_id, body, text_hash)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'course', '00000000-0000-0000-0000-000000000041',
            '00000000-0000-0000-0000-00000000005e', '00000000-0000-0000-0000-000000000035', 'Yuki is behind.', '\x09') $q$);
SELECT pg_temp.fails('shared memory is kept in the agent''s seat', '23502', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, course_id, body, text_hash)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'course', '00000000-0000-0000-0000-000000000041', 'Seatless.', '\x09') $q$);

-- The text.
SELECT pg_temp.fails('an entry in force has text', '23514', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, subject_actor_id, text_hash)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'owner', '00000000-0000-0000-0000-000000000034', '\x09') $q$);
SELECT pg_temp.fails('and the hash of its text', '23514', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, subject_actor_id, body)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'owner', '00000000-0000-0000-0000-000000000034', 'Unhashed.') $q$);
SELECT pg_temp.fails('an entry says something', '23514', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, subject_actor_id, body, text_hash)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'owner', '00000000-0000-0000-0000-000000000034', '', '\x09') $q$);
SELECT pg_temp.fails('an entry is at most 1000 characters', '23514', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, subject_actor_id, body, text_hash)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'owner', '00000000-0000-0000-0000-000000000034', repeat('x', 1001), '\x09') $q$);
-- Characters are counted as the database's encoding counts them: in UTF-8,
-- which the server's deployments use, a character of four bytes is one.
SELECT pg_temp.ok('1000 characters of four bytes each are 4000 bytes, the most', $q$
    DO $utf8$
    BEGIN
        IF current_setting('server_encoding') = 'UTF8' THEN
            INSERT INTO memory_entry (holder_actor_id, scope, subject_actor_id, body, text_hash)
            VALUES ('00000000-0000-0000-0000-00000000003b', 'owner', '00000000-0000-0000-0000-000000000034',
                    repeat(chr(128512), 1000), '\x03');
        END IF;
    END $utf8$ $q$);
SELECT pg_temp.fails('a rejected proposal keeps no text', '23514', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, course_id, holder_member_id, status, body, text_hash, decided_at)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'course', '00000000-0000-0000-0000-000000000041',
            '00000000-0000-0000-0000-00000000005e', 'rejected', 'Kept anyway.', '\x09', now()) $q$);
SELECT pg_temp.ok('at most five tags', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, subject_actor_id, body, text_hash, tags)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'owner', '00000000-0000-0000-0000-000000000034', 'Tagged.', '\x04',
            '{goal,fact,progress,preference,difficulty}') $q$);
SELECT pg_temp.fails('not six', '23514', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, subject_actor_id, body, text_hash, tags)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'owner', '00000000-0000-0000-0000-000000000034', 'Over-tagged.', '\x09',
            '{goal,fact,progress,preference,difficulty,misc}') $q$);

-- One text per bucket.
SELECT pg_temp.fails('the same text twice in one bucket', '23505', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, subject_actor_id, body, text_hash)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'owner', '00000000-0000-0000-0000-000000000034', 'Prefers worked examples.', '\x01') $q$);
SELECT pg_temp.fails('nor proposed beside the same text in force', '23505', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, course_id, holder_member_id, status, body, text_hash)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'course', '00000000-0000-0000-0000-000000000041',
            '00000000-0000-0000-0000-00000000005e', 'proposed', 'Office hours are on Tuesdays.', '\x02') $q$);
SELECT pg_temp.ok('the same text in another bucket', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, course_id, holder_member_id, subject_actor_id, subject_member_id, body, text_hash)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'asker', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-00000000005e',
            '00000000-0000-0000-0000-000000000037', '00000000-0000-0000-0000-000000000058', 'Finds recursion hard.', '\x01') $q$);

-- Review.
SELECT pg_temp.fails('a proposal is undecided', '23514', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, course_id, holder_member_id, status, body, text_hash, decided_at, decided_by_member_id)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'course', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-00000000005e',
            'proposed', 'Decided already.', '\x09', now(), '00000000-0000-0000-0000-000000000051') $q$);
SELECT pg_temp.fails('a rejection says when it was made', '23514', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, course_id, holder_member_id, status)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'course', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-00000000005e',
            'rejected') $q$);
SELECT pg_temp.fails('a decision''s reason is at most 500 characters', '23514', $q$
    UPDATE memory_entry SET decided_at = now(), decided_by_member_id = '00000000-0000-0000-0000-000000000051',
                            decision_reason = repeat('x', 501)
    WHERE id = '00000000-0000-0000-0000-0000000000f4' $q$);
SELECT pg_temp.fails('who decided is a seat of the entry''s course', '23503', $q$
    UPDATE memory_entry SET decided_at = now(), decided_by_member_id = '00000000-0000-0000-0000-000000000055'
    WHERE id = '00000000-0000-0000-0000-0000000000f4' $q$);
SELECT pg_temp.ok('a shared proposal names the entry it corrects', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, course_id, holder_member_id, status, body, text_hash, replaces_id)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'course', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-00000000005e',
            'proposed', 'Office hours are on Wednesdays.', '\x05', '00000000-0000-0000-0000-0000000000f4') $q$);
SELECT pg_temp.fails('only a shared proposal replaces an entry', '23514', $q$
    UPDATE memory_entry SET replaces_id = '00000000-0000-0000-0000-0000000000f4' WHERE id = '00000000-0000-0000-0000-0000000000f1' $q$);
SELECT pg_temp.ok('a proposal is rejected, and its text goes', $q$
    UPDATE memory_entry SET status = 'rejected', body = NULL, search_text = '', text_hash = NULL, decided_at = now(),
                            decided_by_member_id = '00000000-0000-0000-0000-000000000051', decision_reason = 'Too vague.'
    WHERE id = '00000000-0000-0000-0000-0000000000f3' $q$);
SELECT pg_temp.fails('a rejected entry stays as it is', '23001', $q$
    UPDATE memory_entry SET decision_reason = 'On second thoughts.' WHERE id = '00000000-0000-0000-0000-0000000000f3' $q$);
SELECT pg_temp.ok('the same text proposed again once rejected', $q$
    INSERT INTO memory_entry (holder_actor_id, scope, course_id, holder_member_id, status, body, text_hash)
    VALUES ('00000000-0000-0000-0000-00000000003b', 'course', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-00000000005e',
            'proposed', 'HW3 is due Friday.', '\x01') $q$);
SELECT pg_temp.ok('a rejected entry is deleted', $q$
    DELETE FROM memory_entry WHERE id = '00000000-0000-0000-0000-0000000000f3' $q$);

-- What never changes, and what does.
SELECT pg_temp.ok('an entry''s text, tags and pin change, and its version with them', $q$
    UPDATE memory_entry SET body = 'Prefers worked examples in Python.', search_text = 'prefers worked examples in python',
                            text_hash = '\x06', tags = '{preference}', pinned = true, version = version + 1, source = 'owner'
    WHERE id = '00000000-0000-0000-0000-0000000000f1' $q$);
SELECT pg_temp.fails('whose an entry is never changes', '23001', $q$
    UPDATE memory_entry SET holder_actor_id = '00000000-0000-0000-0000-000000000036' WHERE id = '00000000-0000-0000-0000-0000000000f1' $q$);
SELECT pg_temp.fails('nor its scope', '23001', $q$
    UPDATE memory_entry SET scope = 'course' WHERE id = '00000000-0000-0000-0000-0000000000f2' $q$);
SELECT pg_temp.fails('nor whom it is about', '23001', $q$
    UPDATE memory_entry SET subject_actor_id = '00000000-0000-0000-0000-000000000037', subject_member_id = '00000000-0000-0000-0000-000000000058'
    WHERE id = '00000000-0000-0000-0000-0000000000f2' $q$);
SELECT pg_temp.fails('nor in which seat it is kept', '23001', $q$
    UPDATE memory_entry SET holder_member_id = '00000000-0000-0000-0000-000000000053' WHERE id = '00000000-0000-0000-0000-0000000000f2' $q$);
SELECT pg_temp.fails('nor its course', '23001', $q$
    UPDATE memory_entry SET course_id = '00000000-0000-0000-0000-000000000041' WHERE id = '00000000-0000-0000-0000-0000000000f1' $q$);
SELECT pg_temp.fails('nor when, by whom and by what action it was made', '23001', $q$
    UPDATE memory_entry SET created_at = created_at - interval '1 day' WHERE id = '00000000-0000-0000-0000-0000000000f1' $q$);

-- Freezing.
SELECT pg_temp.ok('an entry is frozen when its seat is removed', $q$
    UPDATE memory_entry SET purge_after = now() + interval '30 days', purge_reason = 'seat_removed'
    WHERE id = '00000000-0000-0000-0000-0000000000f2' $q$);
SELECT pg_temp.fails('a frozen entry says why', '23514', $q$
    UPDATE memory_entry SET purge_reason = NULL WHERE id = '00000000-0000-0000-0000-0000000000f2' $q$);
SELECT pg_temp.fails('an entry is frozen only for a removed seat or an archived course', '23514', $q$
    UPDATE memory_entry SET purge_reason = 'stale' WHERE id = '00000000-0000-0000-0000-0000000000f2' $q$);

-- The owner's switch, and the count of writes.
SELECT pg_temp.ok('an owner switches an agent''s memory off', $q$
    INSERT INTO memory_setting (holder_actor_id, enabled, updated_by_actor_id, updated_at)
    VALUES ('00000000-0000-0000-0000-00000000003b', false, '00000000-0000-0000-0000-000000000034', now()) $q$);
SELECT pg_temp.fails('an agent has one setting', '23505', $q$
    INSERT INTO memory_setting (holder_actor_id, enabled, updated_by_actor_id, updated_at)
    VALUES ('00000000-0000-0000-0000-00000000003b', true, '00000000-0000-0000-0000-000000000034', now()) $q$);
SELECT pg_temp.ok('an hour''s writes are counted', $q$
    INSERT INTO memory_write_count (holder_actor_id, hour, n)
    VALUES ('00000000-0000-0000-0000-00000000003b', date_trunc('hour', now()), 1) $q$);
SELECT pg_temp.fails('a count is of one write or more', '23514', $q$
    INSERT INTO memory_write_count (holder_actor_id, hour, n)
    VALUES ('00000000-0000-0000-0000-00000000003b', date_trunc('hour', now()) - interval '1 hour', 0) $q$);

-- Services -------------------------------------------------------------------
-- 20a1 the transcription service · 20c1 its credential
SELECT pg_temp.ok('a service is an actor with a scope', $q$
    INSERT INTO actor (id, kind, display_name, service_scope, created_by_actor_id)
    VALUES ('00000000-0000-0000-0000-0000000020a1', 'service', 'Transcription', 'document_text',
            '00000000-0000-0000-0000-000000000032') $q$);
SELECT pg_temp.fails('one service for each scope', '23505', $q$
    INSERT INTO actor (kind, display_name, service_scope) VALUES ('service', 'Another', 'document_text') $q$);
SELECT pg_temp.fails('a service says what it is for', '23514', $q$
    INSERT INTO actor (kind, display_name) VALUES ('service', 'Nothing in particular') $q$);
SELECT pg_temp.fails('for one of the things there are services for', '23514', $q$
    INSERT INTO actor (kind, display_name, service_scope) VALUES ('service', 'Grader', 'grading') $q$);
SELECT pg_temp.fails('only a service has a scope', '23514', $q$
    INSERT INTO actor (kind, display_name, service_scope, hosting) VALUES ('agent', 'Transcriber', 'document_text', 'mcp') $q$);
SELECT pg_temp.fails('a service has no email', '23514', $q$
    UPDATE actor SET email = 'transcriber@example.edu' WHERE id = '00000000-0000-0000-0000-0000000020a1' $q$);
SELECT pg_temp.fails('nor a platform role', '23514', $q$
    UPDATE actor SET platform_role = 'admin' WHERE id = '00000000-0000-0000-0000-0000000020a1' $q$);
SELECT pg_temp.ok('a service holds a service credential', $q$
    INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, label, issued_by_actor_id)
    VALUES ('00000000-0000-0000-0000-0000000020c1', '00000000-0000-0000-0000-0000000020a1', 'service', 'h', 'svc-1',
            'runtime', '00000000-0000-0000-0000-000000000032') $q$);
SELECT pg_temp.fails('found by its prefix', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash) VALUES ('00000000-0000-0000-0000-0000000020a1', 'service', 'h') $q$);
SELECT pg_temp.fails('and nothing else: no API token', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix)
    VALUES ('00000000-0000-0000-0000-0000000020a1', 'api_token', 'h', 'svc-tok-1') $q$);
SELECT pg_temp.fails('no password', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash) VALUES ('00000000-0000-0000-0000-0000000020a1', 'password', 'h') $q$);
SELECT pg_temp.fails('no session', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix, expires_at)
    VALUES ('00000000-0000-0000-0000-0000000020a1', 'session', 'h', 'svc-sess-1', now() + interval '1 hour') $q$);
SELECT pg_temp.fails('an agent holds no service credential', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix)
    VALUES ('00000000-0000-0000-0000-000000000036', 'service', 'h', 'svc-agent-1') $q$);
SELECT pg_temp.fails('nor a person', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix)
    VALUES ('00000000-0000-0000-0000-000000000034', 'service', 'h', 'svc-person-1') $q$);
SELECT pg_temp.ok('an agent''s token', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix)
    VALUES ('00000000-0000-0000-0000-000000000036', 'api_token', 'h', 'svc-agent-tok-1') $q$);
SELECT pg_temp.fails('is not made into one', '23514', $q$
    UPDATE credential SET kind = 'service' WHERE token_prefix = 'svc-agent-tok-1' $q$);
SELECT pg_temp.fails('a service is seated in no course', '23514', $q$
    INSERT INTO course_member (course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope)
    VALUES ('00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-0000000020a1', 'assistant',
            '00000000-0000-0000-0000-000000000034', 'all', 'all') $q$);
SELECT pg_temp.fails('nor moved into a seat', '23514', $q$
    UPDATE course_member SET actor_id = '00000000-0000-0000-0000-0000000020a1' WHERE id = '00000000-0000-0000-0000-000000000053' $q$);

-- Text versions --------------------------------------------------------------
-- 20e1 slides (material) · 20e2 an exam's instructions · 20e3 a submitted file
-- 20f1 the slides' file · 20f2 their text · 20f3 the exam's file · 20f4 the submitted file · 20d1, 20d3, 20d4 the files
INSERT INTO document (id, course_id, kind, title, submission_id) VALUES
    ('00000000-0000-0000-0000-0000000020e1', '00000000-0000-0000-0000-000000000041', 'material', 'Slides', NULL),
    ('00000000-0000-0000-0000-0000000020e2', '00000000-0000-0000-0000-000000000041', 'instructions', 'Exam', NULL),
    ('00000000-0000-0000-0000-0000000020e3', '00000000-0000-0000-0000-000000000041', 'submission', 'essay.pdf',
     '00000000-0000-0000-0000-0000000000a1');
SELECT pg_temp.ok('a version with a file is queued for its text as its file is recorded', $q$
    INSERT INTO document_version (id, document_id, seq, author_member_id) VALUES
        ('00000000-0000-0000-0000-0000000020f1', '00000000-0000-0000-0000-0000000020e1', 1, '00000000-0000-0000-0000-000000000051'),
        ('00000000-0000-0000-0000-0000000020f3', '00000000-0000-0000-0000-0000000020e2', 1, '00000000-0000-0000-0000-000000000051');
    INSERT INTO document_version_file (id, version_id, document_id, position, filename, storage_key, content_type, byte_size) VALUES
        ('00000000-0000-0000-0000-0000000020d1', '00000000-0000-0000-0000-0000000020f1', '00000000-0000-0000-0000-0000000020e1', 1,
         'slides.pdf', 'k/slides.pdf', 'application/pdf', 2048),
        ('00000000-0000-0000-0000-0000000020d3', '00000000-0000-0000-0000-0000000020f3', '00000000-0000-0000-0000-0000000020e2', 1,
         'exam.pdf', 'k/exam.pdf', 'application/pdf', 512);
    SET CONSTRAINTS ALL IMMEDIATE;
    SET CONSTRAINTS ALL DEFERRED;
    DO $chk$
    BEGIN
        IF (SELECT count(*) FROM document_version_text
            WHERE version_id IN ('00000000-0000-0000-0000-0000000020f1', '00000000-0000-0000-0000-0000000020f3')
              AND status = 'pending' AND NOT backfill AND revision = 1 AND attempts = 0
              AND course_id = '00000000-0000-0000-0000-000000000041') <> 2 THEN
            RAISE EXCEPTION 'the versions were not queued';
        END IF;
    END $chk$ $q$);
SELECT pg_temp.ok('a version of text alone, and a submitted file, are not', $q$
    INSERT INTO document_version (id, document_id, seq, body_md, author_member_id)
    VALUES ('00000000-0000-0000-0000-0000000020f2', '00000000-0000-0000-0000-0000000020e1', 2, '# Slides',
            '00000000-0000-0000-0000-000000000051');
    INSERT INTO document_version (id, document_id, seq, author_member_id)
    VALUES ('00000000-0000-0000-0000-0000000020f4', '00000000-0000-0000-0000-0000000020e3', 1, '00000000-0000-0000-0000-000000000052');
    INSERT INTO document_version_file (id, version_id, document_id, position, filename, storage_key, content_type, byte_size)
    VALUES ('00000000-0000-0000-0000-0000000020d4', '00000000-0000-0000-0000-0000000020f4', '00000000-0000-0000-0000-0000000020e3', 1,
            'essay.pdf', 'k/essay.pdf', 'application/pdf', 100);
    SET CONSTRAINTS ALL IMMEDIATE;
    SET CONSTRAINTS ALL DEFERRED;
    DO $chk$
    BEGIN
        IF EXISTS (SELECT 1 FROM document_version_text
                   WHERE version_id IN ('00000000-0000-0000-0000-0000000020f2', '00000000-0000-0000-0000-0000000020f4')) THEN
            RAISE EXCEPTION 'a version with nothing to transcribe was queued';
        END IF;
    END $chk$ $q$);
SELECT pg_temp.fails('nor may they be', '23514', $q$
    INSERT INTO document_version_text (version_id, file_id, document_id, course_id)
    VALUES ('00000000-0000-0000-0000-0000000020f4', '00000000-0000-0000-0000-0000000020d4', '00000000-0000-0000-0000-0000000020e3',
            '00000000-0000-0000-0000-000000000041') $q$);
SELECT pg_temp.fails('one text version for each file', '23505', $q$
    INSERT INTO document_version_text (version_id, file_id, document_id, course_id)
    VALUES ('00000000-0000-0000-0000-0000000020f1', '00000000-0000-0000-0000-0000000020d1', '00000000-0000-0000-0000-0000000020e1',
            '00000000-0000-0000-0000-000000000041') $q$);
SELECT pg_temp.fails('a text version names its file, since 0027: nothing takes the first for it', '23514', $q$
    INSERT INTO document_version_text (version_id, document_id, course_id)
    VALUES ('00000000-0000-0000-0000-0000000020f1', '00000000-0000-0000-0000-0000000020e1', '00000000-0000-0000-0000-000000000041') $q$);
SELECT pg_temp.fails('in its document''s course', '23001', $q$
    UPDATE document_version_text SET course_id = '00000000-0000-0000-0000-000000000042'
    WHERE version_id = '00000000-0000-0000-0000-0000000020f1' $q$);
SELECT pg_temp.fails('a text version stays its file''s', '23001', $q$
    UPDATE document_version_text SET version_id = '00000000-0000-0000-0000-0000000020f3', document_id = '00000000-0000-0000-0000-0000000020e2'
    WHERE version_id = '00000000-0000-0000-0000-0000000020f1' $q$);
SELECT pg_temp.fails('a claim holds a lease', '23514', $q$
    UPDATE document_version_text SET status = 'working' WHERE version_id = '00000000-0000-0000-0000-0000000020f1' $q$);
SELECT pg_temp.fails('made by a credential', '23514', $q$
    UPDATE document_version_text SET status = 'working', lease_id = gen_random_uuid(), claimed_until = now() + interval '10 minutes'
    WHERE version_id = '00000000-0000-0000-0000-0000000020f1' $q$);
SELECT pg_temp.ok('the service claims it', $q$
    UPDATE document_version_text SET status = 'working', lease_id = gen_random_uuid(), claimed_until = now() + interval '10 minutes',
                                     claimed_by_credential_id = '00000000-0000-0000-0000-0000000020c1', claimed_at = now(),
                                     attempts = 1
    WHERE version_id = '00000000-0000-0000-0000-0000000020f1' $q$);
SELECT pg_temp.fails('a text is there only when it is done', '23514', $q$
    UPDATE document_version_text SET body = '## Page 1', source = 'ai', model = 'M', produced_at = now()
    WHERE version_id = '00000000-0000-0000-0000-0000000020f1' $q$);
SELECT pg_temp.fails('done has its text', '23514', $q$
    UPDATE document_version_text SET status = 'done', lease_id = NULL, claimed_until = NULL
    WHERE version_id = '00000000-0000-0000-0000-0000000020f1' $q$);
SELECT pg_temp.fails('the service''s says what model made it', '23514', $q$
    UPDATE document_version_text SET status = 'done', lease_id = NULL, claimed_until = NULL, body = '## Page 1', source = 'ai',
                                     produced_at = now()
    WHERE version_id = '00000000-0000-0000-0000-0000000020f1' $q$);
SELECT pg_temp.fails('at most 2 MiB of it', '23514', $q$
    UPDATE document_version_text SET status = 'done', lease_id = NULL, claimed_until = NULL, body = repeat('x', 2097153),
                                     source = 'ai', model = 'M', produced_at = now()
    WHERE version_id = '00000000-0000-0000-0000-0000000020f1' $q$);
SELECT pg_temp.fails('and not nothing', '23514', $q$
    UPDATE document_version_text SET status = 'done', lease_id = NULL, claimed_until = NULL, body = '',
                                     source = 'ai', model = 'M', produced_at = now()
    WHERE version_id = '00000000-0000-0000-0000-0000000020f1' $q$);
SELECT pg_temp.fails('a page count is a count of pages', '23514', $q$
    UPDATE document_version_text SET status = 'done', lease_id = NULL, claimed_until = NULL, body = '## Page 1',
                                     source = 'ai', model = 'M', produced_at = now(), pages = 0
    WHERE version_id = '00000000-0000-0000-0000-0000000020f1' $q$);
SELECT pg_temp.ok('the service writes its text', $q$
    UPDATE document_version_text SET status = 'done', lease_id = NULL, claimed_until = NULL, body = repeat('x', 2097152),
                                     source = 'ai', model = 'M', produced_at = now(), pages = 12, revision = 2
    WHERE version_id = '00000000-0000-0000-0000-0000000020f1' $q$);
SELECT pg_temp.fails('done gives no reason', '23514', $q$
    UPDATE document_version_text SET reason = 'fine' WHERE version_id = '00000000-0000-0000-0000-0000000020f1' $q$);
SELECT pg_temp.fails('staff''s says who edited it', '23514', $q$
    UPDATE document_version_text SET body = '## Page 1 (corrected)', source = 'staff'
    WHERE version_id = '00000000-0000-0000-0000-0000000020f1' $q$);
SELECT pg_temp.fails('a member of the course', '23503', $q$
    UPDATE document_version_text SET body = '## Page 1 (corrected)', source = 'staff',
                                     edited_by_member_id = '00000000-0000-0000-0000-000000000055', edited_at = now()
    WHERE version_id = '00000000-0000-0000-0000-0000000020f1' $q$);
SELECT pg_temp.ok('staff edit it', $q$
    UPDATE document_version_text SET body = '## Page 1 (corrected)', source = 'staff',
                                     edited_by_member_id = '00000000-0000-0000-0000-000000000051', edited_at = now(), revision = 3
    WHERE version_id = '00000000-0000-0000-0000-0000000020f1' $q$);
SELECT pg_temp.fails('failed says why', '23514', $q$
    UPDATE document_version_text SET status = 'failed' WHERE version_id = '00000000-0000-0000-0000-0000000020f3' $q$);
SELECT pg_temp.ok('skipped, saying why', $q$
    UPDATE document_version_text SET status = 'skipped', reason = 'too_many_pages'
    WHERE version_id = '00000000-0000-0000-0000-0000000020f3' $q$);
SELECT pg_temp.fails('a text version is not deleted', '23001', $q$
    DELETE FROM document_version_text WHERE version_id = '00000000-0000-0000-0000-0000000020f3' $q$);
SELECT pg_temp.ok('but goes with its file when its version is purged', $q$
    UPDATE document_version SET purged_at = now(),
                                purged_by_actor_id = '00000000-0000-0000-0000-000000000032', purge_reason = 'personal data'
    WHERE id = '00000000-0000-0000-0000-0000000020f1';
    DO $chk$
    BEGIN
        IF EXISTS (SELECT 1 FROM document_version_text WHERE version_id = '00000000-0000-0000-0000-0000000020f1') THEN
            RAISE EXCEPTION 'the text of a purged version is still there';
        END IF;
    END $chk$ $q$);
SELECT pg_temp.fails('and a purged version is queued no more', '23514', $q$
    INSERT INTO document_version_text (version_id, file_id, document_id, course_id)
    VALUES ('00000000-0000-0000-0000-0000000020f1', '00000000-0000-0000-0000-0000000020d1', '00000000-0000-0000-0000-0000000020e1',
            '00000000-0000-0000-0000-000000000041') $q$);
SELECT pg_temp.ok('a service credential is revoked as any is', $q$
    UPDATE credential SET revoked_at = now() WHERE id = '00000000-0000-0000-0000-0000000020c1' $q$);

-- Identity providers the site sets up (migration 0022) -----------------------
-- adfs, set up by admin (32); its secret sealed, as package secrets seals it
SELECT pg_temp.ok('a provider is set up, its secret sealed', $q$
    INSERT INTO sso_provider (id, display_name, issuer, client_id, client_secret_sealed, client_secret_hint, subject_claim,
                              created_by_actor_id, updated_by_actor_id)
    VALUES ('adfs', 'School NetID', 'https://adfs.example.edu/adfs', 'aishie', 'v1.0123456789abcdef.' || repeat('A', 60), '…abcd',
            'upn', '00000000-0000-0000-0000-000000000032', '00000000-0000-0000-0000-000000000032') $q$);
SELECT pg_temp.fails('one provider to an id', '23505', $q$
    INSERT INTO sso_provider (id, display_name, issuer, client_id, client_secret_sealed, client_secret_hint,
                              created_by_actor_id, updated_by_actor_id)
    VALUES ('adfs', 'Again', 'https://adfs.example.edu/adfs', 'aishie', 'v1.0123456789abcdef.' || repeat('A', 60), '…',
            '00000000-0000-0000-0000-000000000032', '00000000-0000-0000-0000-000000000032') $q$);
SELECT pg_temp.fails('an id is lower-case letters, digits and hyphens', '23514', $q$
    INSERT INTO sso_provider (id, display_name, issuer, client_id, client_secret_sealed, client_secret_hint,
                              created_by_actor_id, updated_by_actor_id)
    VALUES ('ADFS/2', 'Two', 'https://adfs.example.edu/adfs', 'aishie', 'v1.0123456789abcdef.' || repeat('A', 60), '…',
            '00000000-0000-0000-0000-000000000032', '00000000-0000-0000-0000-000000000032') $q$);
SELECT pg_temp.fails('a secret is never kept in the clear', '23514', $q$
    INSERT INTO sso_provider (id, display_name, issuer, client_id, client_secret_sealed, client_secret_hint,
                              created_by_actor_id, updated_by_actor_id)
    VALUES ('google', 'Google', 'https://accounts.google.com', 'aishie', 's3cret-for-the-token-endpoint', '…',
            '00000000-0000-0000-0000-000000000032', '00000000-0000-0000-0000-000000000032') $q$);
SELECT pg_temp.fails('nor more of it in its hint than four characters', '23514', $q$
    UPDATE sso_provider SET client_secret_hint = 's3cret-for-the-token-endpoint' WHERE id = 'adfs' $q$);
SELECT pg_temp.fails('a sign-in asks for openid', '23514', $q$
    UPDATE sso_provider SET scopes = '{profile,email}' WHERE id = 'adfs' $q$);
SELECT pg_temp.fails('an issuer is an http or https URL', '23514', $q$
    UPDATE sso_provider SET issuer = 'adfs.example.edu' WHERE id = 'adfs' $q$);
SELECT pg_temp.fails('a name on the button is one line', '23514', $q$
    UPDATE sso_provider SET display_name = E'School\nNetID' WHERE id = 'adfs' $q$);
SELECT pg_temp.fails('linking by email needs the claim and the domains', '23514', $q$
    UPDATE sso_provider SET link_by_email = true, email_claim = 'email' WHERE id = 'adfs' $q$);
SELECT pg_temp.fails('domains are kept in lower case', '23514', $q$
    UPDATE sso_provider SET allowed_email_domains = '{Campus.example.edu}' WHERE id = 'adfs' $q$);
SELECT pg_temp.ok('linking by email within the domains', $q$
    UPDATE sso_provider SET link_by_email = true, email_claim = 'email', allowed_email_domains = '{campus.example.edu}',
                            version = version + 1
    WHERE id = 'adfs' $q$);
SELECT pg_temp.fails('a version counts from 1', '23514', $q$
    UPDATE sso_provider SET version = 0 WHERE id = 'adfs' $q$);
SELECT pg_temp.fails('an id never changes', '23514', $q$
    UPDATE sso_provider SET id = 'adfs2' WHERE id = 'adfs' $q$);
SELECT pg_temp.fails('who changed it is an actor', '23503', $q$
    UPDATE sso_provider SET updated_by_actor_id = '00000000-0000-0000-0000-0000000000ff' WHERE id = 'adfs' $q$);
SELECT pg_temp.ok('and a provider is removed', $q$
    DELETE FROM sso_provider WHERE id = 'adfs' $q$);

-- Files of versions ----------------------------------------------------------
-- 22e1 a lecture (material) · 22f1 its version of three files, 22d1..22d3 · 22f2 one refused
INSERT INTO document (id, course_id, kind, title) VALUES
    ('00000000-0000-0000-0000-0000000022e1', '00000000-0000-0000-0000-000000000041', 'material', 'Week 3');
SELECT pg_temp.ok('a version holds several files, each queued for its text', $q$
    INSERT INTO document_version (id, document_id, seq, body_md, author_member_id, created_at)
    VALUES ('00000000-0000-0000-0000-0000000022f1', '00000000-0000-0000-0000-0000000022e1', 1, 'Read these.',
            '00000000-0000-0000-0000-000000000051', '2026-09-30 09:00:00+00');
    INSERT INTO document_version_file (id, version_id, document_id, position, filename, storage_key, content_type, byte_size,
                                       checksum, created_at) VALUES
        ('00000000-0000-0000-0000-0000000022d1', '00000000-0000-0000-0000-0000000022f1', '00000000-0000-0000-0000-0000000022e1', 1,
         'slides.pdf', 'k/22d1', 'application/pdf', 10, 'sha256:1', '2026-09-30 09:00:00+00'),
        ('00000000-0000-0000-0000-0000000022d2', '00000000-0000-0000-0000-0000000022f1', '00000000-0000-0000-0000-0000000022e1', 2,
         'handout.docx', 'k/22d2', 'application/msword', 20, NULL, '2026-09-30 09:00:00+00'),
        ('00000000-0000-0000-0000-0000000022d3', '00000000-0000-0000-0000-0000000022f1', '00000000-0000-0000-0000-0000000022e1', 3,
         'loops.py', 'k/22d3', 'text/x-python', 30, NULL, '2026-09-30 09:00:00+00');
    SET CONSTRAINTS ALL IMMEDIATE;
    SET CONSTRAINTS ALL DEFERRED;
    DO $chk$
    BEGIN
        IF (SELECT count(*) FROM document_version_text WHERE version_id = '00000000-0000-0000-0000-0000000022f1'
            AND status = 'pending') <> 3 THEN
            RAISE EXCEPTION 'each file was not queued for its text';
        END IF;
    END $chk$ $q$);
SELECT pg_temp.fails('a version''s files are numbered from 1, none missing', '23514', $q$
    INSERT INTO document_version (id, document_id, seq, author_member_id, created_at)
    VALUES ('00000000-0000-0000-0000-0000000022f2', '00000000-0000-0000-0000-0000000022e1', 2, '00000000-0000-0000-0000-000000000051',
            '2026-09-30 10:00:00+00');
    INSERT INTO document_version_file (version_id, document_id, position, filename, storage_key, content_type, byte_size, created_at)
    VALUES ('00000000-0000-0000-0000-0000000022f2', '00000000-0000-0000-0000-0000000022e1', 1, 'a.pdf', 'k/22d4', 'application/pdf',
            10, '2026-09-30 10:00:00+00'),
           ('00000000-0000-0000-0000-0000000022f2', '00000000-0000-0000-0000-0000000022e1', 3, 'c.pdf', 'k/22d6', 'application/pdf',
            10, '2026-09-30 10:00:00+00') $q$);
SELECT pg_temp.fails('a version of files alone has at least one', '23514', $q$
    INSERT INTO document_version (id, document_id, seq, author_member_id, created_at)
    VALUES ('00000000-0000-0000-0000-0000000022f2', '00000000-0000-0000-0000-0000000022e1', 2, '00000000-0000-0000-0000-000000000051',
            '2026-09-30 10:00:00+00') $q$);
SELECT pg_temp.fails('nothing is added to a version afterwards', '23514', $q$
    INSERT INTO document_version_file (version_id, document_id, position, filename, storage_key, content_type, byte_size)
    VALUES ('00000000-0000-0000-0000-0000000022f1', '00000000-0000-0000-0000-0000000022e1', 4, 'more.pdf', 'k/22d5',
            'application/pdf', 10) $q$);
SELECT pg_temp.fails('nor to another document''s version', '23503', $q$
    INSERT INTO document_version_file (version_id, document_id, position, filename, storage_key, content_type, byte_size, created_at)
    VALUES ('00000000-0000-0000-0000-0000000022f1', '00000000-0000-0000-0000-0000000020e1', 4, 'more.pdf', 'k/22d5',
            'application/pdf', 10, '2026-09-30 09:00:00+00') $q$);
SELECT pg_temp.fails('a file''s name is a name, not a path', '23514', $q$
    INSERT INTO document_version (id, document_id, seq, author_member_id, created_at)
    VALUES ('00000000-0000-0000-0000-0000000022f2', '00000000-0000-0000-0000-0000000022e1', 2, '00000000-0000-0000-0000-000000000051',
            '2026-09-30 10:00:00+00');
    INSERT INTO document_version_file (version_id, document_id, position, filename, storage_key, content_type, byte_size, created_at)
    VALUES ('00000000-0000-0000-0000-0000000022f2', '00000000-0000-0000-0000-0000000022e1', 1, '../a.pdf', 'k/22d4',
            'application/pdf', 10, '2026-09-30 10:00:00+00') $q$);
SELECT pg_temp.fails('one file to a key', '23505', $q$
    INSERT INTO document_version (id, document_id, seq, author_member_id, created_at)
    VALUES ('00000000-0000-0000-0000-0000000022f2', '00000000-0000-0000-0000-0000000022e1', 2, '00000000-0000-0000-0000-000000000051',
            '2026-09-30 10:00:00+00');
    INSERT INTO document_version_file (version_id, document_id, position, filename, storage_key, content_type, byte_size, created_at)
    VALUES ('00000000-0000-0000-0000-0000000022f2', '00000000-0000-0000-0000-0000000022e1', 1, 'a.pdf', 'k/22d4', 'application/pdf',
            10, '2026-09-30 10:00:00+00'),
           ('00000000-0000-0000-0000-0000000022f2', '00000000-0000-0000-0000-0000000022e1', 2, 'b.pdf', 'k/22d2', 'application/pdf',
            10, '2026-09-30 10:00:00+00') $q$);
SELECT pg_temp.fails('at most 100 files to a version', '23514', $q$
    INSERT INTO document_version (id, document_id, seq, author_member_id, created_at)
    VALUES ('00000000-0000-0000-0000-0000000022f2', '00000000-0000-0000-0000-0000000022e1', 2, '00000000-0000-0000-0000-000000000051',
            '2026-09-30 10:00:00+00');
    INSERT INTO document_version_file (version_id, document_id, position, filename, storage_key, content_type, byte_size, created_at)
    SELECT '00000000-0000-0000-0000-0000000022f2', '00000000-0000-0000-0000-0000000022e1', n, 'f' || n, 'k/22x' || n,
           'application/pdf', 10, '2026-09-30 10:00:00+00'
    FROM generate_series(1, 101) AS n $q$);
SELECT pg_temp.fails('a file is kept as it was written', '23001', $q$
    UPDATE document_version_file SET filename = 'renamed.pdf' WHERE id = '00000000-0000-0000-0000-0000000022d1' $q$);
SELECT pg_temp.fails('and not deleted', '23001', $q$
    DELETE FROM document_version_file WHERE id = '00000000-0000-0000-0000-0000000022d3' $q$);
SELECT pg_temp.ok('a file''s text is written by its file', $q$
    UPDATE document_version_text SET status = 'done', body = '## Page 1', source = 'staff', revision = revision + 1,
                                     edited_by_member_id = '00000000-0000-0000-0000-000000000051', edited_at = now()
    WHERE version_id = '00000000-0000-0000-0000-0000000022f1' AND file_id = '00000000-0000-0000-0000-0000000022d2' $q$);
SELECT pg_temp.ok('several are claimed at once', $q$
    UPDATE document_version_text SET status = 'working', lease_id = gen_random_uuid(), claimed_until = now() + interval '10 minutes',
                                     claimed_by_credential_id = '00000000-0000-0000-0000-0000000020c1', claimed_at = now(),
                                     attempts = 1
    WHERE version_id = '00000000-0000-0000-0000-0000000022f1' AND status = 'pending' $q$);
SELECT pg_temp.ok('a version''s files go with it, and their texts, when it is purged', $q$
    UPDATE document_version SET body_md = NULL, purged_at = now(),
                                purged_by_actor_id = '00000000-0000-0000-0000-000000000032', purge_reason = 'personal data'
    WHERE id = '00000000-0000-0000-0000-0000000022f1';
    DO $chk$
    BEGIN
        IF EXISTS (SELECT 1 FROM document_version_file WHERE version_id = '00000000-0000-0000-0000-0000000022f1')
           OR EXISTS (SELECT 1 FROM document_version_text WHERE version_id = '00000000-0000-0000-0000-0000000022f1') THEN
            RAISE EXCEPTION 'the files of a purged version are still there';
        END IF;
    END $chk$ $q$);
SELECT pg_temp.fails('and a purged version takes no file', '23514', $q$
    INSERT INTO document_version_file (version_id, document_id, position, filename, storage_key, content_type, byte_size, created_at)
    VALUES ('00000000-0000-0000-0000-0000000022f1', '00000000-0000-0000-0000-0000000022e1', 1, 'back.pdf', 'k/22d9',
            'application/pdf', 10, '2026-09-30 09:00:00+00') $q$);
SELECT pg_temp.ok('nothing writes a version''s file in its own columns any more, to be named after its document', $q$
    DO $chk$
    BEGIN
        IF EXISTS (SELECT 1 FROM pg_proc WHERE proname IN ('document_file_name', 'document_version_text_one_file_at_a_time')) THEN
            RAISE EXCEPTION 'a function kept for the release before 0027 is still there';
        END IF;
    END $chk$ $q$);

-- Hosting: one mode for each agent, for good -----------------------------------
-- 25a1 Sato's tutor, a runtime agent · 25a2 his script, an mcp agent
-- · 25a4 the agent runtime service · 25c1.. their tokens
SELECT pg_temp.ok('an agent is a runtime agent or an mcp agent', $q$
    INSERT INTO actor (id, kind, display_name, owner_actor_id, created_by_actor_id, hosting) VALUES
        ('00000000-0000-0000-0000-0000000025a1', 'agent', 'Tutor', '00000000-0000-0000-0000-000000000034',
         '00000000-0000-0000-0000-000000000034', 'runtime'),
        ('00000000-0000-0000-0000-0000000025a2', 'agent', 'Script', '00000000-0000-0000-0000-000000000034',
         '00000000-0000-0000-0000-000000000034', 'mcp') $q$);
SELECT pg_temp.fails('and nothing else', '23514', $q$
    INSERT INTO actor (kind, display_name, hosting) VALUES ('agent', 'Elsewhere', 'self_hosted') $q$);
SELECT pg_temp.fails('nor none: an agent registered naming none is refused, since 0027', '23514', $q$
    INSERT INTO actor (kind, display_name, created_by_actor_id)
    VALUES ('agent', 'Old style', '00000000-0000-0000-0000-000000000034') $q$);
SELECT pg_temp.fails('a person has no hosting', '23514', $q$
    INSERT INTO actor (kind, display_name, hosting) VALUES ('human', 'Hosted person', 'mcp') $q$);
SELECT pg_temp.fails('an agent''s hosting is never taken away', '23001', $q$
    UPDATE actor SET hosting = NULL WHERE id = '00000000-0000-0000-0000-0000000025a1' $q$);
SELECT pg_temp.fails('nor changed, from runtime', '23001', $q$
    UPDATE actor SET hosting = 'mcp' WHERE id = '00000000-0000-0000-0000-0000000025a1' $q$);
SELECT pg_temp.fails('nor to runtime', '23001', $q$
    UPDATE actor SET hosting = 'runtime' WHERE id = '00000000-0000-0000-0000-0000000025a2' $q$);
SELECT pg_temp.ok('an update that leaves it as it is passes', $q$
    UPDATE actor SET display_name = 'CS101 tutor', hosting = 'runtime' WHERE id = '00000000-0000-0000-0000-0000000025a1' $q$);
SELECT pg_temp.ok('the agent runtime is a site service', $q$
    INSERT INTO actor (id, kind, display_name, service_scope, created_by_actor_id)
    VALUES ('00000000-0000-0000-0000-0000000025a4', 'service', 'Agent runtime', 'agent_runtime',
            '00000000-0000-0000-0000-000000000032') $q$);
SELECT pg_temp.fails('a runtime agent holds no token of its owner''s', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix, issued_by_actor_id)
    VALUES ('00000000-0000-0000-0000-0000000025a1', 'api_token', 'h', 'host-own-1', '00000000-0000-0000-0000-000000000034') $q$);
SELECT pg_temp.ok('its token is the one issued to the runtime', $q$
    INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, issued_by_actor_id, issued_to_service)
    VALUES ('00000000-0000-0000-0000-0000000025c1', '00000000-0000-0000-0000-0000000025a1', 'api_token', 'h', 'host-rt-1',
            '00000000-0000-0000-0000-0000000025a4', 'agent_runtime') $q$);
SELECT pg_temp.fails('one at a time', '23505', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix, issued_to_service)
    VALUES ('00000000-0000-0000-0000-0000000025a1', 'api_token', 'h', 'host-rt-2', 'agent_runtime') $q$);
SELECT pg_temp.ok('another once the first is revoked', $q$
    UPDATE credential SET revoked_at = now() WHERE id = '00000000-0000-0000-0000-0000000025c1';
    INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, issued_to_service)
    VALUES ('00000000-0000-0000-0000-0000000025c2', '00000000-0000-0000-0000-0000000025a1', 'api_token', 'h', 'host-rt-3',
            'agent_runtime') $q$);
SELECT pg_temp.fails('and the first is not revived beside it', '23505', $q$
    UPDATE credential SET revoked_at = NULL WHERE id = '00000000-0000-0000-0000-0000000025c1' $q$);
SELECT pg_temp.fails('an mcp agent is issued no token for the runtime', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix, issued_to_service)
    VALUES ('00000000-0000-0000-0000-0000000025a2', 'api_token', 'h', 'host-rt-4', 'agent_runtime') $q$);
SELECT pg_temp.ok('and holds its owner''s tokens', $q$
    INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, issued_by_actor_id)
    VALUES ('00000000-0000-0000-0000-0000000025c3', '00000000-0000-0000-0000-0000000025a2', 'api_token', 'h', 'host-own-2',
            '00000000-0000-0000-0000-000000000034') $q$);
SELECT pg_temp.fails('a token is issued to the agent runtime and to no other', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix, issued_to_service)
    VALUES ('00000000-0000-0000-0000-0000000025a1', 'api_token', 'h', 'host-rt-5', 'document_text') $q$);
SELECT pg_temp.fails('only an API token is issued to it', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix, issued_to_service)
    VALUES ('00000000-0000-0000-0000-0000000025a4', 'service', 'h', 'host-svc-1', 'agent_runtime') $q$);
SELECT pg_temp.fails('whom a token was issued to never changes', '23001', $q$
    UPDATE credential SET issued_to_service = NULL WHERE id = '00000000-0000-0000-0000-0000000025c2' $q$);
SELECT pg_temp.fails('nor is an owner''s token made the runtime''s', '23001', $q$
    UPDATE credential SET issued_to_service = 'agent_runtime' WHERE id = '00000000-0000-0000-0000-0000000025c3' $q$);
SELECT pg_temp.fails('nor moved to a runtime agent', '23514', $q$
    UPDATE credential SET actor_id = '00000000-0000-0000-0000-0000000025a1' WHERE id = '00000000-0000-0000-0000-0000000025c3' $q$);
SELECT pg_temp.ok('revoking a runtime''s token passes', $q$
    UPDATE credential SET revoked_at = now() WHERE id = '00000000-0000-0000-0000-0000000025c2' $q$);

-- Renditions: an Office file's PDF -----------------------------------------------
-- 26e1 a handout (material) · 26f1 its version of three files, 26d1..26d3 · ca2 Yuki's Word file on c11
SELECT pg_temp.ok('an Office file of a version is queued for its PDF as it is recorded; a PDF, and a .csv called an Excel file, are not', $q$
    INSERT INTO document (id, course_id, kind, title) VALUES
        ('00000000-0000-0000-0000-0000000026e1', '00000000-0000-0000-0000-000000000041', 'material', 'Handout');
    INSERT INTO document_version (id, document_id, seq, author_member_id, created_at)
    VALUES ('00000000-0000-0000-0000-0000000026f1', '00000000-0000-0000-0000-0000000026e1', 1, '00000000-0000-0000-0000-000000000051',
            '2026-09-30 09:00:00+00');
    INSERT INTO document_version_file (id, version_id, document_id, position, filename, storage_key, content_type, byte_size,
                                       created_at) VALUES
        ('00000000-0000-0000-0000-0000000026d1', '00000000-0000-0000-0000-0000000026f1', '00000000-0000-0000-0000-0000000026e1', 1,
         'handout.doc', 'k/26d1', 'application/msword', 10, '2026-09-30 09:00:00+00'),
        ('00000000-0000-0000-0000-0000000026d2', '00000000-0000-0000-0000-0000000026f1', '00000000-0000-0000-0000-0000000026e1', 2,
         'slides.pdf', 'k/26d2', 'application/pdf', 10, '2026-09-30 09:00:00+00'),
        ('00000000-0000-0000-0000-0000000026d3', '00000000-0000-0000-0000-0000000026f1', '00000000-0000-0000-0000-0000000026e1', 3,
         'marks.csv', 'k/26d3', 'application/vnd.ms-excel', 10, '2026-09-30 09:00:00+00');
    DO $chk$
    BEGIN
        IF (SELECT string_agg(f.filename || ':' || r.status || ':' || r.backfill, ' ')
            FROM file_rendition r JOIN document_version_file f ON f.id = r.file_id
            WHERE f.version_id = '00000000-0000-0000-0000-0000000026f1') IS DISTINCT FROM 'handout.doc:queued:false' THEN
            RAISE EXCEPTION 'the Office file alone was not queued';
        END IF;
    END $chk$ $q$);
SELECT pg_temp.ok('and so is a message''s Office file, with its message', $q$
    DO $chk$
    BEGIN
        IF NOT EXISTS (SELECT 1 FROM file_rendition WHERE attachment_id = '00000000-0000-0000-0000-000000000ca2'
                       AND status = 'queued' AND course_id = '00000000-0000-0000-0000-000000000041')
           OR EXISTS (SELECT 1 FROM file_rendition WHERE attachment_id = '00000000-0000-0000-0000-000000000ca1') THEN
            RAISE EXCEPTION 'the Word file alone was not queued';
        END IF;
    END $chk$ $q$);
SELECT pg_temp.ok('which files are converted: an Office name, of an Office type or of none in particular', $q$
    DO $chk$
    BEGIN
        IF NOT file_rendition_convertible('Lecture 3.PPTX', 'application/vnd.openxmlformats-officedocument.presentationml.presentation')
           OR NOT file_rendition_convertible('a.xlsm', 'Application/vnd.ms-excel.sheet.macroEnabled.12; charset=binary')
           OR NOT file_rendition_convertible('a.odg', 'application/octet-stream')
           OR NOT file_rendition_convertible('a.docx', 'application/zip')
           OR NOT file_rendition_convertible('a.rtf', 'text/rtf')
           OR file_rendition_convertible('a.pdf', 'application/pdf')
           OR file_rendition_convertible('a.docx', 'text/html')
           OR file_rendition_convertible('a.zip', 'application/zip')
           OR file_rendition_convertible('docx', 'application/msword')
           OR file_rendition_convertible('a.docx.png', 'image/png')
           OR file_rendition_convertible(NULL, NULL) THEN
            RAISE EXCEPTION 'the table converts the wrong files';
        END IF;
    END $chk$ $q$);
SELECT pg_temp.fails('one rendition to a file', '23505', $q$
    INSERT INTO file_rendition (course_id, file_id)
    VALUES ('00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-0000000026d1') $q$);
SELECT pg_temp.fails('of one file, a version''s or a message''s', '23514', $q$
    INSERT INTO file_rendition (course_id, file_id, attachment_id)
    VALUES ('00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-0000000026d3', '00000000-0000-0000-0000-000000000ca1') $q$);
SELECT pg_temp.fails('and of no file that is not converted', '23514', $q$
    INSERT INTO file_rendition (course_id, file_id)
    VALUES ('00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-0000000026d2') $q$);
SELECT pg_temp.fails('made queued, never claimed or done', '23514', $q$
    INSERT INTO file_rendition (course_id, attachment_id, status, reason)
    VALUES ('00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000ca1', 'failed', 'unsupported') $q$);
SELECT pg_temp.fails('a claim holds a lease', '23514', $q$
    UPDATE file_rendition SET status = 'claimed', attempts = 1 WHERE file_id = '00000000-0000-0000-0000-0000000026d1' $q$);
SELECT pg_temp.fails('made with a credential', '23514', $q$
    UPDATE file_rendition SET status = 'claimed', lease_id = gen_random_uuid(), claimed_until = now() + interval '10 minutes',
                              attempts = 1
    WHERE file_id = '00000000-0000-0000-0000-0000000026d1' $q$);
SELECT pg_temp.ok('claimed, with a lease and the credential that made it', $q$
    UPDATE file_rendition SET status = 'claimed', lease_id = gen_random_uuid(), claimed_until = now() + interval '10 minutes',
                              claimed_by_credential_id = '00000000-0000-0000-0000-0000000020c1', claimed_at = now(), attempts = 1
    WHERE file_id = '00000000-0000-0000-0000-0000000026d1' $q$);
SELECT pg_temp.fails('done has its PDF', '23514', $q$
    UPDATE file_rendition SET status = 'done', lease_id = NULL, claimed_until = NULL
    WHERE file_id = '00000000-0000-0000-0000-0000000026d1' $q$);
SELECT pg_temp.fails('and its page count', '23514', $q$
    UPDATE file_rendition SET status = 'done', lease_id = NULL, claimed_until = NULL, storage_key = 'renditions/26d1',
                              byte_size = 100, produced_at = now()
    WHERE file_id = '00000000-0000-0000-0000-0000000026d1' $q$);
SELECT pg_temp.fails('a PDF is five bytes at least', '23514', $q$
    UPDATE file_rendition SET status = 'done', lease_id = NULL, claimed_until = NULL, storage_key = 'renditions/26d1',
                              byte_size = 4, page_count = 1, produced_at = now()
    WHERE file_id = '00000000-0000-0000-0000-0000000026d1' $q$);
SELECT pg_temp.fails('nothing but done has a PDF', '23514', $q$
    UPDATE file_rendition SET status = 'failed', reason = 'timeout', lease_id = NULL, claimed_until = NULL,
                              storage_key = 'renditions/26d1', byte_size = 100, page_count = 1, produced_at = now()
    WHERE file_id = '00000000-0000-0000-0000-0000000026d1' $q$);
SELECT pg_temp.fails('failed says why', '23514', $q$
    UPDATE file_rendition SET status = 'failed', lease_id = NULL, claimed_until = NULL
    WHERE file_id = '00000000-0000-0000-0000-0000000026d1' $q$);
SELECT pg_temp.fails('in one of its words', '23514', $q$
    UPDATE file_rendition SET status = 'skipped', reason = 'it was too hard', lease_id = NULL, claimed_until = NULL
    WHERE file_id = '00000000-0000-0000-0000-0000000026d1' $q$);
SELECT pg_temp.fails('and nothing else does', '23514', $q$
    UPDATE file_rendition SET reason = 'timeout' WHERE file_id = '00000000-0000-0000-0000-0000000026d1' $q$);
SELECT pg_temp.ok('done, with its PDF', $q$
    UPDATE file_rendition SET status = 'done', lease_id = NULL, claimed_until = NULL, storage_key = 'renditions/26d1',
                              byte_size = 100, checksum = 'sha256:26', page_count = 3, produced_at = now()
    WHERE file_id = '00000000-0000-0000-0000-0000000026d1' $q$);
SELECT pg_temp.fails('and kept as it was made', '23001', $q$
    UPDATE file_rendition SET page_count = 4 WHERE file_id = '00000000-0000-0000-0000-0000000026d1' $q$);
SELECT pg_temp.fails('nor queued again', '23001', $q$
    UPDATE file_rendition SET status = 'queued', storage_key = NULL, byte_size = NULL, checksum = NULL, page_count = NULL,
                              produced_at = NULL
    WHERE file_id = '00000000-0000-0000-0000-0000000026d1' $q$);
SELECT pg_temp.fails('one PDF to a key', '23505', $q$
    UPDATE file_rendition SET status = 'done', storage_key = 'renditions/26d1', byte_size = 100, page_count = 1,
                              produced_at = now()
    WHERE attachment_id = '00000000-0000-0000-0000-000000000ca2' $q$);
SELECT pg_temp.fails('a rendition stays its file''s', '23001', $q$
    UPDATE file_rendition SET attachment_id = NULL, file_id = '00000000-0000-0000-0000-0000000026d1'
    WHERE attachment_id = '00000000-0000-0000-0000-000000000ca2' $q$);
SELECT pg_temp.ok('a failed one is queued again', $q$
    UPDATE file_rendition SET status = 'failed', reason = 'conversion_failed' WHERE attachment_id = '00000000-0000-0000-0000-000000000ca2';
    UPDATE file_rendition SET status = 'queued', reason = NULL, attempts = 0, queued_at = now()
    WHERE attachment_id = '00000000-0000-0000-0000-000000000ca2' $q$);
SELECT pg_temp.fails('a rendition goes only with its file', '23001', $q$
    DELETE FROM file_rendition WHERE attachment_id = '00000000-0000-0000-0000-000000000ca2' $q$);
SELECT pg_temp.fails('nor all at once', '23001', $q$
    TRUNCATE file_rendition $q$);
SELECT pg_temp.ok('a version''s files'' renditions go with them when it is purged', $q$
    UPDATE document_version SET purged_at = now(),
                                purged_by_actor_id = '00000000-0000-0000-0000-000000000032', purge_reason = 'personal data'
    WHERE id = '00000000-0000-0000-0000-0000000026f1';
    DO $chk$
    BEGIN
        IF EXISTS (SELECT 1 FROM file_rendition WHERE file_id = '00000000-0000-0000-0000-0000000026d1')
           OR EXISTS (SELECT 1 FROM document_version_file WHERE version_id = '00000000-0000-0000-0000-0000000026f1') THEN
            RAISE EXCEPTION 'the rendition of a purged version''s file is still there';
        END IF;
    END $chk$ $q$);

-- What an answer relied on (migration 0029) -----------------------------------
-- 29e1 Week 6 (material) · 29f1 its version of two files, 29d1, 29d2 · 29f2 its version of text alone
-- 29e2 a document of CS101 B · 29c1 Yuki's conversation with the grader, open
-- 29a1 her question · 29a2 the grader's answer, saying what it relied on
-- c12 the grader's answer to c11, which says nothing of its sources
SELECT pg_temp.ok('an answer names the versions, files and pages it relied on, in order', $q$
    INSERT INTO conversation (id, course_id, opener_member_id, respondent_member_id)
    VALUES ('00000000-0000-0000-0000-0000000029c1', '00000000-0000-0000-0000-000000000041', '00000000-0000-0000-0000-000000000052',
            '00000000-0000-0000-0000-000000000053');
    INSERT INTO conversation_message (id, conversation_id, course_id, seq, author_member_id, body, created_by_action_id)
    VALUES ('00000000-0000-0000-0000-0000000029a1', '00000000-0000-0000-0000-0000000029c1', '00000000-0000-0000-0000-000000000041', 1,
            '00000000-0000-0000-0000-000000000052', 'Where is the rubric?', '00000000-0000-0000-0000-0000000000b1');
    INSERT INTO conversation_message (id, conversation_id, course_id, seq, author_member_id, in_reply_to_message_id, body,
                                      created_by_action_id, sources_stated)
    VALUES ('00000000-0000-0000-0000-0000000029a2', '00000000-0000-0000-0000-0000000029c1', '00000000-0000-0000-0000-000000000041', 2,
            '00000000-0000-0000-0000-000000000053', '00000000-0000-0000-0000-0000000029a1', 'In Week 6.',
            '00000000-0000-0000-0000-0000000000b1', true);
    INSERT INTO document (id, course_id, kind, title) VALUES
        ('00000000-0000-0000-0000-0000000029e1', '00000000-0000-0000-0000-000000000041', 'material', 'Week 6'),
        ('00000000-0000-0000-0000-0000000029e2', '00000000-0000-0000-0000-000000000042', 'material', 'Week 6, section B');
    INSERT INTO document_version (id, document_id, seq, body_md, author_member_id) VALUES
        ('00000000-0000-0000-0000-0000000029f1', '00000000-0000-0000-0000-0000000029e1', 1, NULL, '00000000-0000-0000-0000-000000000051'),
        ('00000000-0000-0000-0000-0000000029f2', '00000000-0000-0000-0000-0000000029e1', 2, '# Week 6', '00000000-0000-0000-0000-000000000051'),
        ('00000000-0000-0000-0000-0000000029f3', '00000000-0000-0000-0000-0000000029e2', 1, '# Week 6', '00000000-0000-0000-0000-000000000055');
    INSERT INTO document_version_file (id, version_id, document_id, position, filename, storage_key, content_type, byte_size) VALUES
        ('00000000-0000-0000-0000-0000000029d1', '00000000-0000-0000-0000-0000000029f1', '00000000-0000-0000-0000-0000000029e1', 1,
         'slides.pdf', 'k/29d1', 'application/pdf', 10),
        ('00000000-0000-0000-0000-0000000029d2', '00000000-0000-0000-0000-0000000029f1', '00000000-0000-0000-0000-0000000029e1', 2,
         'notes.md', 'k/29d2', 'text/markdown', 10);
    INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id, file_id, file_version_id,
                                             slide, part) VALUES
        ('00000000-0000-0000-0000-0000000029a2', '00000000-0000-0000-0000-000000000041', 1, '00000000-0000-0000-0000-0000000029e1',
         '00000000-0000-0000-0000-0000000029f1', '00000000-0000-0000-0000-0000000029d1', '00000000-0000-0000-0000-0000000029f1', 4, 1);
    INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id) VALUES
        ('00000000-0000-0000-0000-0000000029a2', '00000000-0000-0000-0000-000000000041', 2, '00000000-0000-0000-0000-0000000029e1',
         '00000000-0000-0000-0000-0000000029f2') $q$);
SELECT pg_temp.fails('only an answer says what it relied on', '23514', $q$
    INSERT INTO conversation_message (conversation_id, course_id, seq, author_member_id, body, created_by_action_id, sources_stated)
    VALUES ('00000000-0000-0000-0000-0000000029c1', '00000000-0000-0000-0000-000000000041', 3,
            '00000000-0000-0000-0000-000000000052', 'And the rubric?', '00000000-0000-0000-0000-0000000000b1', true) $q$);
SELECT pg_temp.fails('one source at each place', '23505', $q$
    INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id)
    VALUES ('00000000-0000-0000-0000-0000000029a2', '00000000-0000-0000-0000-000000000041', 2, '00000000-0000-0000-0000-0000000029e1',
            '00000000-0000-0000-0000-0000000029f1') $q$);
SELECT pg_temp.fails('at most 20 to an answer', '23514', $q$
    INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id)
    VALUES ('00000000-0000-0000-0000-0000000029a2', '00000000-0000-0000-0000-000000000041', 21, '00000000-0000-0000-0000-0000000029e1',
            '00000000-0000-0000-0000-0000000029f1') $q$);
SELECT pg_temp.fails('a source is in its answer''s course', '23503', $q$
    INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id)
    VALUES ('00000000-0000-0000-0000-0000000029a2', '00000000-0000-0000-0000-000000000042', 3, '00000000-0000-0000-0000-0000000029e2',
            '00000000-0000-0000-0000-0000000029f3') $q$);
SELECT pg_temp.fails('and so is its document', '23503', $q$
    INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id)
    VALUES ('00000000-0000-0000-0000-0000000029a2', '00000000-0000-0000-0000-000000000041', 3, '00000000-0000-0000-0000-0000000029e2',
            '00000000-0000-0000-0000-0000000029f3') $q$);
SELECT pg_temp.fails('a version is its document''s', '23503', $q$
    INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id)
    VALUES ('00000000-0000-0000-0000-0000000029a2', '00000000-0000-0000-0000-000000000041', 3, '00000000-0000-0000-0000-0000000000e1',
            '00000000-0000-0000-0000-0000000029f1') $q$);
SELECT pg_temp.fails('a file is its version''s', '23503', $q$
    INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id, file_id, file_version_id)
    VALUES ('00000000-0000-0000-0000-0000000029a2', '00000000-0000-0000-0000-000000000041', 3, '00000000-0000-0000-0000-0000000029e1',
            '00000000-0000-0000-0000-0000000029f2', '00000000-0000-0000-0000-0000000029d1', '00000000-0000-0000-0000-0000000029f2') $q$);
SELECT pg_temp.fails('and named with the source''s version', '23514', $q$
    INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id, file_id, file_version_id)
    VALUES ('00000000-0000-0000-0000-0000000029a2', '00000000-0000-0000-0000-000000000041', 3, '00000000-0000-0000-0000-0000000029e1',
            '00000000-0000-0000-0000-0000000029f2', '00000000-0000-0000-0000-0000000029d1', '00000000-0000-0000-0000-0000000029f1') $q$);
SELECT pg_temp.fails('a file named, its version named too', '23514', $q$
    INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id, file_id)
    VALUES ('00000000-0000-0000-0000-0000000029a2', '00000000-0000-0000-0000-000000000041', 3, '00000000-0000-0000-0000-0000000029e1',
            '00000000-0000-0000-0000-0000000029f1', '00000000-0000-0000-0000-0000000029d1') $q$);
SELECT pg_temp.fails('a page or a slide, not both', '23514', $q$
    INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id, file_id, file_version_id,
                                             page, slide)
    VALUES ('00000000-0000-0000-0000-0000000029a2', '00000000-0000-0000-0000-000000000041', 3, '00000000-0000-0000-0000-0000000029e1',
            '00000000-0000-0000-0000-0000000029f1', '00000000-0000-0000-0000-0000000029d1', '00000000-0000-0000-0000-0000000029f1', 1, 1) $q$);
SELECT pg_temp.fails('a page counts from 1', '23514', $q$
    INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id, file_id, file_version_id, page)
    VALUES ('00000000-0000-0000-0000-0000000029a2', '00000000-0000-0000-0000-000000000041', 3, '00000000-0000-0000-0000-0000000029e1',
            '00000000-0000-0000-0000-0000000029f1', '00000000-0000-0000-0000-0000000029d1', '00000000-0000-0000-0000-0000000029f1', 0) $q$);
SELECT pg_temp.fails('and so does a part', '23514', $q$
    INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id, file_id, file_version_id, part)
    VALUES ('00000000-0000-0000-0000-0000000029a2', '00000000-0000-0000-0000-000000000041', 3, '00000000-0000-0000-0000-0000000029e1',
            '00000000-0000-0000-0000-0000000029f1', '00000000-0000-0000-0000-0000000029d1', '00000000-0000-0000-0000-0000000029f1', 0) $q$);
SELECT pg_temp.fails('an answer that said nothing of its sources names none', '23514', $q$
    INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id)
    VALUES ('00000000-0000-0000-0000-000000000c12', '00000000-0000-0000-0000-000000000041', 1, '00000000-0000-0000-0000-0000000029e1',
            '00000000-0000-0000-0000-0000000029f1') $q$);
SELECT pg_temp.fails('a question names no sources', '23514', $q$
    INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id)
    VALUES ('00000000-0000-0000-0000-000000000c11', '00000000-0000-0000-0000-000000000041', 1, '00000000-0000-0000-0000-0000000029e1',
            '00000000-0000-0000-0000-0000000029f1') $q$);
SELECT pg_temp.fails('a source is written with its answer, never added to it afterwards', '23514', $q$
    INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id, created_at)
    VALUES ('00000000-0000-0000-0000-0000000029a2', '00000000-0000-0000-0000-000000000041', 3, '00000000-0000-0000-0000-0000000029e1',
            '00000000-0000-0000-0000-0000000029f1', now() + interval '1 minute') $q$);
SELECT pg_temp.fails('a student''s work is no source', '23514', $q$
    INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id)
    VALUES ('00000000-0000-0000-0000-0000000029a2', '00000000-0000-0000-0000-000000000041', 3, '00000000-0000-0000-0000-0000000020e3',
            '00000000-0000-0000-0000-0000000020f4') $q$);
SELECT pg_temp.fails('nor is a purged version', '23514', $q$
    INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id)
    VALUES ('00000000-0000-0000-0000-0000000029a2', '00000000-0000-0000-0000-000000000041', 3, '00000000-0000-0000-0000-0000000020e1',
            '00000000-0000-0000-0000-0000000020f1') $q$);
SELECT pg_temp.fails('a source is kept as it was written', '23001', $q$
    UPDATE conversation_message_source SET slide = 5 WHERE message_id = '00000000-0000-0000-0000-0000000029a2' AND position = 1 $q$);
SELECT pg_temp.fails('and names its file while there is one', '23001', $q$
    UPDATE conversation_message_source SET file_id = NULL, file_version_id = NULL
    WHERE message_id = '00000000-0000-0000-0000-0000000029a2' AND position = 1 $q$);
SELECT pg_temp.fails('a source is never deleted', '23001', $q$
    DELETE FROM conversation_message_source WHERE message_id = '00000000-0000-0000-0000-0000000029a2' $q$);
SELECT pg_temp.fails('nor all at once', '23001', $q$
    TRUNCATE conversation_message_source $q$);
SELECT pg_temp.ok('a version is purged: its sources stay, naming no file', $q$
    UPDATE document_version SET purged_at = now(),
                                purged_by_actor_id = '00000000-0000-0000-0000-000000000032', purge_reason = 'personal data'
    WHERE id = '00000000-0000-0000-0000-0000000029f1';
    DO $chk$
    BEGIN
        IF (SELECT string_agg(position || ':' || coalesce(file_id::text, '-') || ':' || coalesce(file_version_id::text, '-')
                              || ':' || coalesce(slide::text, '-') || ':' || coalesce(part::text, '-'), ' ' ORDER BY position)
            FROM conversation_message_source WHERE message_id = '00000000-0000-0000-0000-0000000029a2') IS DISTINCT FROM '1:-:-:4:1 2:-:-:-:-' THEN
            RAISE EXCEPTION 'the sources of a purged version are not as they were, but for their file';
        END IF;
    END $chk$ $q$);

\o
ROLLBACK;
\echo 'All checks passed.'

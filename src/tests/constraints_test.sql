-- AIshiteru Core — database-enforced rule tests
--
-- Run against a throwaway database that has the migrations applied. Every
-- statement runs inside one transaction that is rolled back at the end, but
-- the fixtures use fixed ids, so do not point this at a database with data.
--
--   createdb aishiteru_test
--   for f in migrations/*.up.sql; do psql -v ON_ERROR_STOP=1 -d aishiteru_test -f "$f"; done
--   psql -X -d aishiteru_test -f tests/constraints_test.sql
--   dropdb aishiteru_test
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
--      1cx credentials · 1ax join links
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
    VALUES ('00000000-0000-0000-0000-000000000036', 'sso', 'polyu-adfs', 'grader@connect.polyu.hk') $q$);
SELECT pg_temp.fails('and no session, which only signing in makes', '23514', $q$
    INSERT INTO credential (actor_id, kind, secret_hash, token_prefix, expires_at)
    VALUES ('00000000-0000-0000-0000-000000000036', 'session', 'h', 'sess-agent', now() + interval '12 hours') $q$);
SELECT pg_temp.fails('nor is a person''s session moved to an agent', '23514', $q$
    UPDATE credential SET actor_id = '00000000-0000-0000-0000-000000000036' WHERE token_prefix = 'sess-1' $q$);
SELECT pg_temp.ok('an agent''s token is revoked as any credential is', $q$
    UPDATE credential SET revoked_at = now() WHERE token_prefix = 'tok-agent-1' $q$);

-- Login IDs: a person's student or staff number, a sign-in name beside the email.
SELECT pg_temp.ok('a person has a login ID, as an administrator gives it', $q$
    UPDATE actor SET login_id = 'HNU20230001' WHERE id = '00000000-0000-0000-0000-000000000035' $q$);
SELECT pg_temp.ok('and an email beside it', $q$
    UPDATE actor SET login_id = 'T19880042', email = 'sato@hainanu.edu.cn' WHERE id = '00000000-0000-0000-0000-000000000034' $q$);
SELECT pg_temp.fails('a login ID is unique regardless of case', '23505', $q$
    INSERT INTO actor (kind, display_name, login_id, created_by_actor_id)
    VALUES ('human', 'x', 'hnu20230001', '00000000-0000-0000-0000-000000000031') $q$);
SELECT pg_temp.fails('and is not taken over by another person', '23505', $q$
    UPDATE actor SET login_id = 'Hnu20230001' WHERE id = '00000000-0000-0000-0000-000000000037' $q$);
SELECT pg_temp.ok('its longest: 64 letters, digits, dots, hyphens and underscores', $q$
    UPDATE actor SET login_id = 'hnu.2023-00_' || repeat('7', 52) WHERE id = '00000000-0000-0000-0000-000000000037' $q$);
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
    UPDATE actor SET login_id = 'hnué2023' WHERE id = '00000000-0000-0000-0000-000000000037' $q$);
SELECT pg_temp.fails('nor a full-width digit', '23514', $q$
    UPDATE actor SET login_id = '２０２３' WHERE id = '00000000-0000-0000-0000-000000000037' $q$);
SELECT pg_temp.fails('an agent has no login ID', '23514', $q$
    UPDATE actor SET login_id = 'grader-v2' WHERE id = '00000000-0000-0000-0000-000000000036' $q$);
SELECT pg_temp.fails('nor is one registered with one', '23514', $q$
    INSERT INTO actor (kind, display_name, login_id, created_by_actor_id)
    VALUES ('agent', 'bot', 'bot-1', '00000000-0000-0000-0000-000000000031') $q$);
SELECT pg_temp.fails('nor has the system actor one', '23514', $q$
    UPDATE actor SET login_id = 'system' WHERE id = '00000000-0000-0000-0000-000000000033' $q$);
SELECT pg_temp.ok('a person who typed their own has it recorded unchecked', $q$
    INSERT INTO actor (kind, display_name, login_id, login_id_verified, created_by_actor_id)
    VALUES ('human', 'Wei', '20230009', false, '00000000-0000-0000-0000-000000000034') $q$);
SELECT pg_temp.fails('only a person''s login ID goes unchecked', '23514', $q$
    INSERT INTO actor (kind, display_name, login_id_verified, created_by_actor_id)
    VALUES ('agent', 'bot', false, '00000000-0000-0000-0000-000000000031') $q$);
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
    INSERT INTO actor (id, kind, display_name, owner_actor_id, created_by_actor_id)
    VALUES ('00000000-0000-0000-0000-000000000038', 'agent', 'Yuki''s agent', '00000000-0000-0000-0000-000000000035', '00000000-0000-0000-0000-000000000035') $q$);
SELECT pg_temp.fails('an agent owns no agent', '23514', $q$
    INSERT INTO actor (kind, display_name, owner_actor_id, created_by_actor_id)
    VALUES ('agent', 'x', '00000000-0000-0000-0000-000000000036', '00000000-0000-0000-0000-000000000031') $q$);
SELECT pg_temp.fails('the system actor owns nothing', '23514', $q$
    INSERT INTO actor (kind, display_name, owner_actor_id, created_by_actor_id)
    VALUES ('agent', 'x', '00000000-0000-0000-0000-000000000033', '00000000-0000-0000-0000-000000000031') $q$);
SELECT pg_temp.fails('only an agent has an owner', '23514', $q$
    INSERT INTO actor (kind, display_name, owner_actor_id, created_by_actor_id)
    VALUES ('human', 'x', '00000000-0000-0000-0000-000000000035', '00000000-0000-0000-0000-000000000031') $q$);
SELECT pg_temp.fails('an owner is an actor that exists', '23514', $q$
    INSERT INTO actor (kind, display_name, owner_actor_id, created_by_actor_id)
    VALUES ('agent', 'x', '00000000-0000-0000-0000-0000000000ff', '00000000-0000-0000-0000-000000000031') $q$);
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
    INSERT INTO actor (kind, display_name, platform_role, owner_actor_id, created_by_actor_id)
    VALUES ('agent', 'x', 'admin', '00000000-0000-0000-0000-000000000035', '00000000-0000-0000-0000-000000000031') $q$);
SELECT pg_temp.fails('nothing owns itself', '23514', $q$
    INSERT INTO actor (id, kind, display_name, owner_actor_id, created_by_actor_id)
    VALUES ('00000000-0000-0000-0000-0000000000fe', 'agent', 'x', '00000000-0000-0000-0000-0000000000fe', '00000000-0000-0000-0000-000000000031') $q$);
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
    INSERT INTO actor (id, kind, display_name, created_by_actor_id)
    VALUES ('00000000-0000-0000-0000-0000000003d0', 'agent', 'triage', '00000000-0000-0000-0000-000000000031');
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
    INSERT INTO actor (id, kind, display_name, owner_actor_id, created_by_actor_id)
    VALUES ('00000000-0000-0000-0000-000000000039', 'agent', 'Mei''s agent', '00000000-0000-0000-0000-00000000003a', '00000000-0000-0000-0000-00000000003a');
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

-- Site chat: which credential of an agent's declared it ------------------------
-- 1c1 the grader's (36) token · 1c2 Sato's (34) session
INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, expires_at) VALUES
    ('00000000-0000-0000-0000-0000000001c1', '00000000-0000-0000-0000-000000000036', 'api_token', 'h', 'sc-agent', NULL),
    ('00000000-0000-0000-0000-0000000001c2', '00000000-0000-0000-0000-000000000034', 'session', 'h', 'sc-person', now() + interval '12 hours');
SELECT pg_temp.ok('an agent declares site chat with a credential of its own', $q$
    UPDATE actor SET site_chat_credential_id = '00000000-0000-0000-0000-0000000001c1' WHERE id = '00000000-0000-0000-0000-000000000036' $q$);
SELECT pg_temp.fails('not with someone else''s', '23503', $q$
    UPDATE actor SET site_chat_credential_id = '00000000-0000-0000-0000-0000000001c2' WHERE id = '00000000-0000-0000-0000-000000000036' $q$);
SELECT pg_temp.fails('nor with one that does not exist', '23503', $q$
    UPDATE actor SET site_chat_credential_id = '00000000-0000-0000-0000-0000000000ff' WHERE id = '00000000-0000-0000-0000-000000000036' $q$);
SELECT pg_temp.fails('a person declares no site chat, even with a credential of their own', '23514', $q$
    UPDATE actor SET site_chat_credential_id = '00000000-0000-0000-0000-0000000001c2' WHERE id = '00000000-0000-0000-0000-000000000034' $q$);
SELECT pg_temp.fails('a credential that declared it is not moved to another actor', '23503', $q$
    UPDATE credential SET actor_id = '00000000-0000-0000-0000-000000000038' WHERE id = '00000000-0000-0000-0000-0000000001c1' $q$);
SELECT pg_temp.ok('revoking it leaves the row as it is: whether it is live is read, not kept', $q$
    UPDATE credential SET revoked_at = now() WHERE id = '00000000-0000-0000-0000-0000000001c1';
    DO $chk$
    BEGIN
        IF NOT EXISTS (SELECT 1 FROM actor WHERE id = '00000000-0000-0000-0000-000000000036'
                       AND site_chat_credential_id = '00000000-0000-0000-0000-0000000001c1') THEN
            RAISE EXCEPTION 'revoking the credential changed the actor';
        END IF;
    END $chk$ $q$);
SELECT pg_temp.ok('and switching it off clears it', $q$
    UPDATE actor SET site_chat_credential_id = NULL WHERE id = '00000000-0000-0000-0000-000000000036' $q$);

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
    INSERT INTO actor (kind, display_name, email, email_verified, created_by_actor_id)
    VALUES ('agent', 'x', 'x@example.edu', false, '00000000-0000-0000-0000-000000000034') $q$);
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
SELECT pg_temp.fails('a version is never deleted', '23001', $q$
    DELETE FROM document_version WHERE id = '00000000-0000-0000-0000-0000000000f2' $q$);
SELECT pg_temp.fails('a purge says who made it and why', '23514', $q$
    UPDATE document_version SET storage_key = NULL, purged_at = now() WHERE id = '00000000-0000-0000-0000-0000000000f2' $q$);
SELECT pg_temp.fails('a purge takes the file and not only the date', '23001', $q$
    UPDATE document_version SET purged_at = now(), purged_by_actor_id = '00000000-0000-0000-0000-000000000032',
                                purge_reason = 'personal data' WHERE id = '00000000-0000-0000-0000-0000000000f2' $q$);
SELECT pg_temp.fails('nor does it move the version', '23001', $q$
    UPDATE document_version SET storage_key = NULL, seq = 9, purged_at = now(),
                                purged_by_actor_id = '00000000-0000-0000-0000-000000000032', purge_reason = 'personal data'
    WHERE id = '00000000-0000-0000-0000-0000000000f2' $q$);
SELECT pg_temp.ok('a version is purged: its file goes, what it was and who purged it stay', $q$
    UPDATE document_version SET storage_key = NULL, purged_at = now(),
                                purged_by_actor_id = '00000000-0000-0000-0000-000000000032', purge_reason = 'personal data'
    WHERE id = '00000000-0000-0000-0000-0000000000f2';
    DO $chk$
    BEGIN
        IF NOT EXISTS (SELECT 1 FROM document_version WHERE id = '00000000-0000-0000-0000-0000000000f2'
                       AND seq = 1 AND content_type = 'application/pdf' AND byte_size = 1024 AND storage_key IS NULL) THEN
            RAISE EXCEPTION 'the tombstone does not say what was there';
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

-- Memory ---------------------------------------------------------------------
-- 3b Sato's tutor, an agent he owns · 5e its seat in A, his delegate, answering the course
-- mx memory entries. Who made an entry and when, and the text's hash, are
-- the application's to write; defaults for the rest of this transaction keep
-- each check below about the one rule it tests. Rolled back with everything
-- else.
INSERT INTO actor (id, kind, display_name, owner_actor_id, created_by_actor_id)
VALUES ('00000000-0000-0000-0000-00000000003b', 'agent', 'Sato''s tutor', '00000000-0000-0000-0000-000000000034', '00000000-0000-0000-0000-000000000034');
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

\o
ROLLBACK;
\echo 'All checks passed.'

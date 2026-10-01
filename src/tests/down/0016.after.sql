-- AIshie Core — after 0016_login_ids.down.sql, in `make db-test-sql`
--
-- The login IDs are gone, and whether anyone vouched for them, and which
-- password is temporary; the people stay, with their emails, and the student
-- with no email keeps the temporary password, live, and the session signed
-- in with it.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public'
               AND (table_name, column_name) IN (('actor', 'login_id'), ('actor', 'login_id_verified'),
                                                 ('credential', 'must_change'))) THEN
        RAISE EXCEPTION 'FAIL  0016 down: a column it added is still there';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conname IN ('actor_login_id_valid', 'actor_login_id_is_a_persons',
                                                             'actor_unverified_login_id_is_a_persons',
                                                             'credential_must_change_is_an_issued_password'))
       OR EXISTS (SELECT 1 FROM pg_class WHERE relname = 'actor_login_id_key') THEN
        RAISE EXCEPTION 'FAIL  0016 down: a constraint or an index it added is still there';
    END IF;
    IF (SELECT count(*) FROM actor WHERE id IN ('00000000-0000-0000-0016-000000000031', '00000000-0000-0000-0016-000000000035',
                                                '00000000-0000-0000-0016-000000000037') AND status = 'active') <> 3 THEN
        RAISE EXCEPTION 'FAIL  0016 down: a person did not stay';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM actor WHERE id = '00000000-0000-0000-0016-000000000037' AND email = 'fang@campus.example.edu') THEN
        RAISE EXCEPTION 'FAIL  0016 down: an email did not stay';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM credential WHERE id = '00000000-0000-0000-0016-0000000000c2' AND revoked_at IS NULL
                   AND kind = 'password' AND issued_by_actor_id = '00000000-0000-0000-0016-000000000031') THEN
        RAISE EXCEPTION 'FAIL  0016 down: the temporary password did not stay, as an ordinary one';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM credential WHERE id = '00000000-0000-0000-0016-0000000000c3' AND revoked_at IS NULL) THEN
        RAISE EXCEPTION 'FAIL  0016 down: the session did not stay';
    END IF;
END $chk$;
\echo 'PASS  0016 down with login IDs, one of them unchecked, and a temporary password signed in with'

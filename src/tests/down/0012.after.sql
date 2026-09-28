-- AIshiteru Core — after 0012_join_links.down.sql, in `make db-test-sql`
--
-- The links are gone, and which seat came through which, and whose email
-- nobody vouches for; the person who registered through a link stays, with
-- their password, and both seats stay, as ordinary seats.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'course_join_link') THEN
        RAISE EXCEPTION 'FAIL  0012 down: course_join_link is still there';
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public'
               AND (table_name, column_name) IN (('actor', 'email_verified'), ('course_member', 'join_link_id'))) THEN
        RAISE EXCEPTION 'FAIL  0012 down: a column it added is still there';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conname IN ('actor_unverified_email_is_a_persons', 'course_member_join_link_fk')) THEN
        RAISE EXCEPTION 'FAIL  0012 down: a constraint it added is still there';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM actor WHERE id = '00000000-0000-0000-0012-000000000035' AND email = 'yuki@example.edu'
                   AND status = 'active') THEN
        RAISE EXCEPTION 'FAIL  0012 down: the person who registered through a link did not stay';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM credential WHERE id = '00000000-0000-0000-0012-0000000000c1' AND revoked_at IS NULL) THEN
        RAISE EXCEPTION 'FAIL  0012 down: their password did not stay';
    END IF;
    IF (SELECT count(*) FROM course_member WHERE id IN ('00000000-0000-0000-0012-000000000052', '00000000-0000-0000-0012-000000000053')
          AND status = 'active' AND role = 'student') <> 2 THEN
        RAISE EXCEPTION 'FAIL  0012 down: a seat taken through a link did not stay';
    END IF;
END $chk$;
\echo 'PASS  0012 down with join links, a person registered through one and seats taken through it'

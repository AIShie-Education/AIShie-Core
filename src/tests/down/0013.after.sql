-- AIshie Core — after 0013_member_invite.down.sql, in `make db-test-sql`
--
-- member_invite is gone from both tables; the seat, the preset and the link
-- stay, the seat still managing members.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public'
               AND table_name IN ('course_member', 'permission_preset') AND column_name = 'perm_member_invite') THEN
        RAISE EXCEPTION 'FAIL  0013 down: perm_member_invite is still there';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM course_member WHERE id = '00000000-0000-0000-0013-000000000051'
                   AND status = 'active' AND perm_member_manage = 'autonomous') THEN
        RAISE EXCEPTION 'FAIL  0013 down: the instructor''s seat did not stay as it was';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM permission_preset WHERE id = '00000000-0000-0000-0013-000000000091') THEN
        RAISE EXCEPTION 'FAIL  0013 down: the department''s preset did not stay';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM course_join_link WHERE id = '00000000-0000-0000-0013-0000000001a1') THEN
        RAISE EXCEPTION 'FAIL  0013 down: the link did not stay';
    END IF;
END $chk$;
\echo 'PASS  0013 down with member_invite held by a seat and a preset, and a link made'

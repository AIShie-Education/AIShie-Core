-- AIshiteru Core — migration 0012 (down)
-- Reverts 0012_join_links.up.sql.
--
-- Lost: every join link, so a link handed out stops working (the release
-- before knows none); which seat was taken through which link; and which
-- emails nobody but their person vouches for, so that an account registered
-- through a link is afterwards like one an administrator registered. Kept:
-- those accounts, their passwords and sessions, and the seats taken through
-- links, as ordinary seats; the actions and events that made them.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP INDEX IF EXISTS course_member_join_link_idx;
ALTER TABLE course_member
    DROP CONSTRAINT IF EXISTS course_member_join_link_fk,
    DROP COLUMN IF EXISTS join_link_id;

DROP TABLE IF EXISTS course_join_link;

ALTER TABLE actor
    DROP CONSTRAINT IF EXISTS actor_unverified_email_is_a_persons,
    DROP COLUMN IF EXISTS email_verified;

COMMIT;

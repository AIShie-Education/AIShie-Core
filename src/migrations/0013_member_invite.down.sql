-- AIshiteru Core — migration 0013 (down)
-- Reverts 0013_member_invite.up.sql.
--
-- Lost: every seat's and every preset's level of member_invite. The release
-- before this one knows neither it nor join links. Kept: the seats, the
-- presets, and the links, which 0012's down takes away.

BEGIN;

SET LOCAL lock_timeout = '10s';

ALTER TABLE course_member DROP COLUMN IF EXISTS perm_member_invite;
ALTER TABLE permission_preset DROP COLUMN IF EXISTS perm_member_invite;

COMMIT;

-- AIshie Core — migration 0024 (down)
-- Reverts 0024_conversation_export.up.sql.
--
-- Lost: nothing but the index. The exports already made stay recorded as
-- the actions they are, and their files stay in the file store until the
-- sweep of the release that made them removes them; the release before
-- this one neither reads nor removes them, and a later release that does
-- removes them once they are old enough.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP INDEX IF EXISTS conversation_message_created_idx;

COMMIT;

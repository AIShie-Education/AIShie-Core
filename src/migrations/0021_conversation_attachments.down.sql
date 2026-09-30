-- AIshie Core — migration 0021 (down)
-- Reverts 0021_conversation_attachments.up.sql.
--
-- Lost: which message each file was attached to, and what it was called.
-- The messages stay as they were written, and the record of each message's
-- action still names the upload tokens it attached. The files stay in the
-- file store, under conversations/, which the release before this one
-- neither reads nor sweeps; migrated up again, nothing points at them, and
-- the orphan sweep removes them once they are as old as it waits for.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TABLE IF EXISTS conversation_attachment;
DROP FUNCTION IF EXISTS conversation_attachment_check_message();

COMMIT;

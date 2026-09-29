-- AIshiteru Core — migration 0019 (down)
-- Reverts 0019_conversation_drafts.up.sql.
--
-- Lost: the drafts of the answers being written now, which the release
-- before this one neither writes nor reads. Kept: everything else; the
-- answers themselves were never here.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TABLE IF EXISTS conversation_draft;

COMMIT;

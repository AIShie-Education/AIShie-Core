-- AIshie Core — migration 0029 (down)
-- Reverts 0029_message_sources.up.sql.
--
-- Lost: which course materials each answer said it relied on, and which
-- answers said so, even saying none (sources_stated). The release before
-- this one never reads them, and neither its answers nor anything else of
-- a conversation changes. The record of each answer's action keeps
-- its sources as the call gave them (its payload), as it keeps what the
-- answer said, for those who decide actions in the course.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TABLE IF EXISTS conversation_message_source;
DROP FUNCTION IF EXISTS conversation_message_source_check();
DROP FUNCTION IF EXISTS conversation_message_source_guarded();
-- Its check goes with it.
ALTER TABLE conversation_message DROP COLUMN IF EXISTS sources_stated;

COMMIT;

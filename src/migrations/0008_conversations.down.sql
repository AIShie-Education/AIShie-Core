-- AIshiteru Core — migration 0008 (down)
-- Reverts 0008_conversations.up.sql.
--
-- The conversations go, with their messages and retractions. The actions
-- that wrote them stay, as history, with the bodies in their payloads; their
-- events stay too, since the event table is append-only. A reply that is
-- still waiting for a decision is cancelled here, as a decision cancels a
-- proposal whose tool the release no longer has (tool_removed), so that the
-- approval queue of the release before this holds nothing it cannot carry
-- out.

BEGIN;

SET LOCAL lock_timeout = '10s';

UPDATE action
   SET status = 'cancelled',
       result = jsonb_build_object('error', jsonb_build_object(
           'code', 'failed_precondition',
           'message', 'the proposal can no longer be carried out',
           'details', jsonb_build_object('reason', 'tool_removed')))
 WHERE status = 'proposed' AND action_type LIKE 'conversation.%';

DROP TABLE IF EXISTS conversation_message_retraction;
DROP TABLE IF EXISTS conversation_message;
DROP TABLE IF EXISTS conversation;

DROP FUNCTION IF EXISTS conversation_message_check_author();
DROP FUNCTION IF EXISTS conversation_check_change();

COMMIT;

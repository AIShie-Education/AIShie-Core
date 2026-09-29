-- AIshiteru Core — migration 0018 (down)
-- Reverts 0018_conversations_with_agents.up.sql.
--
-- Lost: only the refusals. The release before this one may open a
-- conversation with a person again, and give a person conversation_answer
-- again. Kept: every conversation the up closed, which stays closed, as a
-- closed conversation always does; every proposal it cancelled; and every
-- seat and preset with the levels it has, those the up lowered among them,
-- since nothing says what they were.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TRIGGER IF EXISTS conversation_respondent_is_agent ON conversation;
DROP FUNCTION IF EXISTS conversation_check_respondent();
DROP TRIGGER IF EXISTS course_member_person_ceiling ON course_member;
DROP FUNCTION IF EXISTS course_member_person_answers_nothing();

COMMIT;

-- AIshiteru Core — migration 0018 (up)
-- Conversations are between a person and an agent: a person asks, an agent
-- answers, and nothing else. Design reference: docs/schema.md §2.2
-- (Ceilings), §2.8. PostgreSQL 13+.
--
-- People talk to people elsewhere. So a person answers no conversation: a
-- person's seat holds conversation_answer at denied, as an agent's holds
-- action_decide at confirm_required at most (0014), and a conversation's
-- respondent is an agent's seat. The application refuses a person as a
-- respondent, and a person answering, with the reason
-- conversations_are_with_agents, and member views give it as the ceiling's
-- reason for a person's conversation_answer. The database holds the rows to
-- it: authorization never reads actor.kind, and so cannot cap a person's
-- level itself.
--
-- What is there already is brought to it, once, here:
--
--   * every seat of a person's that is not removed, and every preset for
--     people (told by its role, anything but assistant, as 0013 and 0014
--     told presets for agents), is lowered to conversation_answer denied: the
--     built-in instructor preset among them, which was the one to give it. A
--     removed seat is history, and is left as it was;
--   * every proposal that could only fail now is cancelled, as a decision
--     cancels one that can no longer be carried out: an answer or a question
--     waiting for approval in a conversation closed below, and a
--     conversation waiting to be opened with a person;
--   * every conversation open with a person as its respondent is closed,
--     closed_reason conversations_are_with_agents, as removing a seat closes
--     its conversations (seat_removed), and its participants are told so in
--     the feed (conversation.closed, and action.cancelled for what was
--     cancelled). It stays readable, as every closed conversation does.
--
-- From now on the database refuses a conversation whose respondent is a
-- person's seat, and writes a person's seat that is not removed with
-- conversation_answer denied, whatever it is told: cut down, not refused,
-- so that the previous release's calls go through. Both read kind to limit,
-- never to grant, as 0014's trigger does.
--
-- The previous release keeps working while this goes in, but for people
-- answering. Its conversation.open refuses a person as a respondent anyway
-- once the person's seat answers nothing, as not addressable; its
-- member.update_perms and its seating give a person conversation_answer
-- denied whatever they name; and a person's conversation.answer is denied.

BEGIN;

-- Waits for a busy table rather than queueing every call behind it; a
-- deploy that times out here is run again.
SET LOCAL lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- Seats and presets: a person answers nothing
-- ---------------------------------------------------------------------------

UPDATE course_member m
   SET perm_conversation_answer = 'denied'
  FROM actor a
 WHERE a.id = m.actor_id AND a.kind <> 'agent'
   AND m.status <> 'removed' AND m.perm_conversation_answer <> 'denied';

UPDATE permission_preset
   SET perm_conversation_answer = 'denied'
 WHERE role <> 'assistant' AND perm_conversation_answer <> 'denied';

CREATE FUNCTION course_member_person_answers_nothing() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM actor WHERE id = NEW.actor_id AND kind <> 'agent') THEN
        NEW.perm_conversation_answer := 'denied';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER course_member_person_ceiling
    BEFORE INSERT OR UPDATE ON course_member
    FOR EACH ROW
    WHEN (NEW.status <> 'removed' AND NEW.perm_conversation_answer <> 'denied')
    EXECUTE FUNCTION course_member_person_answers_nothing();

-- ---------------------------------------------------------------------------
-- Conversations: a respondent is an agent
-- ---------------------------------------------------------------------------

-- The conversations to close: open, their respondent a person's seat.
CREATE TEMPORARY TABLE with_a_person ON COMMIT DROP AS
SELECT c.id, c.course_id
  FROM conversation c
  JOIN course_member r ON r.id = c.respondent_member_id
  JOIN actor ra ON ra.id = r.actor_id
 WHERE c.status = 'open' AND ra.kind <> 'agent';

-- What waits for approval and can only fail now: answers and questions in
-- them, and conversations to be opened with a person. The result is the one
-- a decision stores for a proposal it cancels (pipeline.Cancellation).
CREATE TEMPORARY TABLE cancelled ON COMMIT DROP AS
WITH gone AS (
    UPDATE action a
       SET status = 'cancelled',
           result = jsonb_build_object('error', jsonb_build_object(
               'code', 'failed_precondition',
               'message', 'the proposal can no longer be carried out',
               'details', jsonb_build_object('reason', 'conversations_are_with_agents')))
     WHERE a.status = 'proposed'
       AND ((a.action_type IN ('conversation.answer', 'conversation.ask') AND a.target_type = 'conversation'
             AND a.target_id IN (SELECT id FROM with_a_person))
         OR (a.action_type = 'conversation.open' AND EXISTS (
               SELECT 1 FROM course_member r JOIN actor ra ON ra.id = r.actor_id
                WHERE r.id::text = a.payload->>'respondent_member_id' AND ra.kind <> 'agent')))
    RETURNING a.id, a.course_id)
SELECT id, course_id FROM gone;

UPDATE conversation
   SET status = 'closed', closed_reason = 'conversations_are_with_agents'
 WHERE id IN (SELECT id FROM with_a_person);

CREATE FUNCTION conversation_check_respondent() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM course_member m JOIN actor a ON a.id = m.actor_id
                WHERE m.id = NEW.respondent_member_id AND m.course_id = NEW.course_id AND a.kind <> 'agent') THEN
        RAISE EXCEPTION 'conversation %: its respondent is a person''s seat; conversations are between a person and an agent', NEW.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW; -- a seat that is not there, or not in the course, the foreign key refuses
END;
$$;

CREATE TRIGGER conversation_respondent_is_agent
    BEFORE INSERT ON conversation
    FOR EACH ROW EXECUTE FUNCTION conversation_check_respondent();

-- ---------------------------------------------------------------------------
-- The feed: the participants are told
-- ---------------------------------------------------------------------------

-- As the tools write events, and last: whoever is writing events now has
-- taken the numbers before these, and commits first; whoever comes after
-- waits for this, and takes the numbers after. A reader whose cursor has
-- passed a number never finds an event behind it later (events.Flush). Taken
-- here, after the rows above, as a call takes its rows before it writes its
-- events.
LOCK TABLE event IN EXCLUSIVE MODE;

INSERT INTO event (type, course_id, action_id, subject_type, subject_id, payload)
SELECT 'action.cancelled', c.course_id, c.id, 'action', c.id,
       jsonb_build_object('reason', 'conversations_are_with_agents')
  FROM cancelled c
 ORDER BY c.id;

INSERT INTO event (type, course_id, subject_type, subject_id, payload)
SELECT 'conversation.closed', w.course_id, 'conversation', w.id,
       jsonb_build_object('conversation_id', w.id, 'reason', 'conversations_are_with_agents')
  FROM with_a_person w
 ORDER BY w.id;

COMMIT;

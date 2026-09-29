-- AIshiteru Core — after 0018_conversations_with_agents.up.sql, in `make db-test-sql`
--
-- Every person's seat that is not removed answers nothing, and every preset
-- for people; the removed seat, the agent's seat and the preset for agents
-- are as they were. Every conversation open with a person is closed, saying
-- why, and its participants are told so in the feed; those with the tutor
-- stay open, and the one closed before keeps its reason. What waited for
-- approval and could only fail is cancelled, saying why, and nothing else
-- is. What was written before counts as read by both participants. And the
-- database holds the rule from now on.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
DECLARE
    wrong text;
BEGIN
    SELECT string_agg(right(id::text, 2) || ' ' || perm_conversation_answer, ', ' ORDER BY id) INTO wrong
      FROM course_member
     WHERE (id IN ('00000000-0000-0000-0018-000000000051', '00000000-0000-0000-0018-000000000052',
                   '00000000-0000-0000-0018-000000000053') AND perm_conversation_answer <> 'denied')
        OR (id IN ('00000000-0000-0000-0018-000000000054', '00000000-0000-0000-0018-000000000056')
            AND perm_conversation_answer <> 'autonomous');
    IF wrong IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  0018 up: seats: %', wrong;
    END IF;
    SELECT string_agg(name || ' ' || perm_conversation_answer, ', ' ORDER BY name) INTO wrong
      FROM permission_preset
     WHERE (id IN ('00000000-0000-0000-0018-000000000091', '00000000-0000-0000-0018-000000000092') AND perm_conversation_answer <> 'denied')
        OR (id = '00000000-0000-0000-0018-000000000093' AND perm_conversation_answer <> 'autonomous');
    IF wrong IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  0018 up: presets: %', wrong;
    END IF;

    SELECT string_agg(right(id::text, 2) || ' ' || status || ' ' || coalesce(closed_reason, '-'), ', ' ORDER BY id) INTO wrong
      FROM conversation
     WHERE (id IN ('00000000-0000-0000-0018-0000000000c1', '00000000-0000-0000-0018-0000000000c2',
                   '00000000-0000-0000-0018-0000000000c5')
            AND (status, closed_reason) IS DISTINCT FROM ('closed', 'conversations_are_with_agents'))
        OR (id IN ('00000000-0000-0000-0018-0000000000c3', '00000000-0000-0000-0018-0000000000c6')
            AND (status, closed_reason) IS DISTINCT FROM ('open', NULL))
        OR (id = '00000000-0000-0000-0018-0000000000c4' AND (status, closed_reason) IS DISTINCT FROM ('closed', 'Thanks!'));
    IF wrong IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  0018 up: conversations: %', wrong;
    END IF;
    IF (SELECT count(*) FROM event WHERE type = 'conversation.closed' AND subject_type = 'conversation' AND action_id IS NULL
          AND course_id = '00000000-0000-0000-0018-000000000041' AND payload->>'reason' = 'conversations_are_with_agents'
          AND payload->>'conversation_id' = subject_id::text
          AND subject_id IN ('00000000-0000-0000-0018-0000000000c1', '00000000-0000-0000-0018-0000000000c2',
                             '00000000-0000-0000-0018-0000000000c5')) <> 3
       OR (SELECT count(*) FROM event WHERE type = 'conversation.closed' AND course_id = '00000000-0000-0000-0018-000000000041') <> 3 THEN
        RAISE EXCEPTION 'FAIL  0018 up: the participants of the conversations closed are not told, once each, or others are';
    END IF;

    SELECT string_agg(right(id::text, 2) || ' ' || status, ', ' ORDER BY id) INTO wrong
      FROM action
     WHERE (id IN ('00000000-0000-0000-0018-0000000000b5', '00000000-0000-0000-0018-0000000000b6',
                   '00000000-0000-0000-0018-0000000000b8')
            AND (status <> 'cancelled' OR result->'error'->>'code' <> 'failed_precondition'
                 OR result->'error'->'details'->>'reason' <> 'conversations_are_with_agents'))
        OR (id IN ('00000000-0000-0000-0018-0000000000b7', '00000000-0000-0000-0018-0000000000b9',
                   '00000000-0000-0000-0018-0000000000ba') AND status <> 'proposed');
    IF wrong IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  0018 up: proposals: %', wrong;
    END IF;
    IF (SELECT count(*) FROM event WHERE type = 'action.cancelled' AND subject_type = 'action' AND action_id = subject_id
          AND payload->>'reason' = 'conversations_are_with_agents'
          AND action_id IN ('00000000-0000-0000-0018-0000000000b5', '00000000-0000-0000-0018-0000000000b6',
                            '00000000-0000-0000-0018-0000000000b8')) <> 3
       OR (SELECT count(*) FROM event WHERE type = 'action.cancelled' AND course_id = '00000000-0000-0000-0018-000000000041') <> 3 THEN
        RAISE EXCEPTION 'FAIL  0018 up: the proposals cancelled are not in the feed, once each, or others are';
    END IF;

    -- What was written before counts as read, by both participants of
    -- every conversation with a message in it, closed or not; the one with
    -- none has no place for either.
    IF (SELECT count(*) FROM conversation_read r JOIN conversation c ON c.id = r.conversation_id
         WHERE c.course_id = '00000000-0000-0000-0018-000000000041' AND r.last_read_seq = 1
           AND r.member_id IN (c.opener_member_id, c.respondent_member_id)
           AND c.id IN ('00000000-0000-0000-0018-0000000000c1', '00000000-0000-0000-0018-0000000000c2',
                        '00000000-0000-0000-0018-0000000000c3', '00000000-0000-0000-0018-0000000000c4',
                        '00000000-0000-0000-0018-0000000000c5')) <> 10
       OR (SELECT count(*) FROM conversation_read WHERE course_id = '00000000-0000-0000-0018-000000000041') <> 10 THEN
        RAISE EXCEPTION 'FAIL  0018 up: what was written before is not read by both participants, or something else is';
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'conversation_respondent_is_agent')
       OR NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'course_member_person_ceiling') THEN
        RAISE EXCEPTION 'FAIL  0018 up: no trigger holds the rule';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_class WHERE relname = 'conversation_respondent_idx') THEN
        RAISE EXCEPTION 'FAIL  0018 up: no index lists an agent''s conversations';
    END IF;
END $chk$;
\echo 'PASS  0018 up closes the conversations with people, saying so, cancels what could only fail, lowers people''s seats and presets, counts what was written as read, and nothing else'

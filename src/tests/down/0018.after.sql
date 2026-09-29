-- AIshiteru Core — after 0018_conversations_with_agents.down.sql, in `make db-test-sql`
--
-- The refusals are gone, and what everyone had read, and with them nothing
-- else: the conversations the up closed stay closed, the proposals it
-- cancelled stay cancelled, and the seats and presets it lowered stay
-- lowered. The release before this one may open a conversation with a
-- person again, and give a person conversation_answer again.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_trigger WHERE tgname IN ('conversation_respondent_is_agent', 'course_member_person_ceiling'))
       OR EXISTS (SELECT 1 FROM pg_proc WHERE proname IN ('conversation_check_respondent', 'course_member_person_answers_nothing',
                                                          'conversation_read_check'))
       OR EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'conversation_read') THEN
        RAISE EXCEPTION 'FAIL  0018 down: a trigger, a function or the read state is still there';
    END IF;
    IF (SELECT count(*) FROM conversation WHERE status = 'closed' AND closed_reason = 'conversations_are_with_agents'
        AND id IN ('00000000-0000-0000-0018-0000000000c1', '00000000-0000-0000-0018-0000000000c2',
                   '00000000-0000-0000-0018-0000000000c5')) <> 3 THEN
        RAISE EXCEPTION 'FAIL  0018 down: a conversation the up closed was opened again';
    END IF;
    IF (SELECT count(*) FROM action WHERE status = 'cancelled'
        AND id IN ('00000000-0000-0000-0018-0000000000b5', '00000000-0000-0000-0018-0000000000b6',
                   '00000000-0000-0000-0018-0000000000b8')) <> 3 THEN
        RAISE EXCEPTION 'FAIL  0018 down: a proposal the up cancelled waits again';
    END IF;
    IF EXISTS (SELECT 1 FROM course_member WHERE perm_conversation_answer <> 'denied'
               AND id IN ('00000000-0000-0000-0018-000000000051', '00000000-0000-0000-0018-000000000052',
                          '00000000-0000-0000-0018-000000000057'))
       OR EXISTS (SELECT 1 FROM permission_preset WHERE perm_conversation_answer <> 'denied'
                  AND id IN ('00000000-0000-0000-0018-000000000091', '00000000-0000-0000-0018-000000000092')) THEN
        RAISE EXCEPTION 'FAIL  0018 down: a seat or a preset the up lowered was raised again';
    END IF;
    INSERT INTO conversation (course_id, opener_member_id, respondent_member_id)
    VALUES ('00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0018-000000000053', '00000000-0000-0000-0018-000000000051');
    UPDATE course_member SET perm_conversation_answer = 'autonomous' WHERE id = '00000000-0000-0000-0018-000000000057';
    IF NOT EXISTS (SELECT 1 FROM course_member WHERE id = '00000000-0000-0000-0018-000000000057' AND perm_conversation_answer = 'autonomous') THEN
        RAISE EXCEPTION 'FAIL  0018 down: a person''s seat is still written answering nothing';
    END IF;
END $chk$;
\echo 'PASS  0018 down with conversations it closed, proposals it cancelled and seats it lowered, which stay so'

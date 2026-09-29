-- AIshie Core — before 0018_conversations_with_agents.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with: the course of tests/up/0018,
-- its conversations with people closed and its people answering nothing.
-- While 0018 is in, the database opens no conversation with a person, and
-- writes a person's seat answering nothing whatever it is told, seating or
-- changing one; an agent's seat is written as it is told. Committed, so
-- that the down migration runs over it; the downs after it drop it with
-- everything else.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

DO $chk$
BEGIN
    BEGIN
        INSERT INTO conversation (course_id, opener_member_id, respondent_member_id)
        VALUES ('00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0018-000000000053', '00000000-0000-0000-0018-000000000051');
        RAISE EXCEPTION 'FAIL  0018: while it was in, the database opened a conversation with a person';
    EXCEPTION WHEN check_violation THEN
        NULL;
    END;
    UPDATE course_member SET perm_conversation_answer = 'autonomous'
     WHERE id IN ('00000000-0000-0000-0018-000000000051', '00000000-0000-0000-0018-000000000056');
    IF NOT EXISTS (SELECT 1 FROM course_member WHERE id = '00000000-0000-0000-0018-000000000051' AND perm_conversation_answer = 'denied')
       OR NOT EXISTS (SELECT 1 FROM course_member WHERE id = '00000000-0000-0000-0018-000000000056' AND perm_conversation_answer = 'autonomous') THEN
        RAISE EXCEPTION 'FAIL  0018: while it was in, a person''s seat was written answering, or an agent''s was not';
    END IF;
END $chk$;

-- 37 Mai, a person seated answering, as the release before would seat her
INSERT INTO actor (id, kind, display_name, created_by_actor_id) VALUES
    ('00000000-0000-0000-0018-000000000037', 'human', 'Mai', NULL);
INSERT INTO course_member (id, course_id, actor_id, role, added_by_actor_id, student_scope, assignment_scope, perm_conversation_answer)
VALUES ('00000000-0000-0000-0018-000000000057', '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0018-000000000037',
        'ta', '00000000-0000-0000-0018-000000000031', 'all', 'all', 'autonomous');
DO $chk$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM course_member WHERE id = '00000000-0000-0000-0018-000000000057' AND perm_conversation_answer = 'denied') THEN
        RAISE EXCEPTION 'FAIL  0018: while it was in, a person was seated answering';
    END IF;
END $chk$;

COMMIT;

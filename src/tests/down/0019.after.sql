-- AIshie Core — after 0019_conversation_drafts.down.sql, in `make db-test-sql`
--
-- The drafts are gone, and with them nothing else: the conversation whose
-- answer was being written stays as it was, its messages with it. The
-- release before this one writes no draft and reads none.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'conversation_draft') THEN
        RAISE EXCEPTION 'FAIL  0019 down: the drafts are still there';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM conversation WHERE id = '00000000-0000-0000-0018-0000000000c3' AND status = 'open')
       OR NOT EXISTS (SELECT 1 FROM conversation_message WHERE conversation_id = '00000000-0000-0000-0018-0000000000c3') THEN
        RAISE EXCEPTION 'FAIL  0019 down: the conversation of the draft, or what was written in it, is gone';
    END IF;
END $chk$;
\echo 'PASS  0019 down with a draft kept, which goes, and its conversation, which stays'

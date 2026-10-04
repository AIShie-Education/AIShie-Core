-- AIshie Core — after 0030_assignment_deletion.down.sql, in `make db-test-sql`
--
-- The guards are as 0029 left them: an event is append-only, a version is
-- never deleted, a submission handed in is never deleted and a draft may
-- be, as 0001 had it. The record of deletions, the queue of files and the
-- mark on an emptied action are gone, and so is what held them; the
-- emptied action stays empty, and the quiz deleted stays deleted.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
DECLARE
    left_over text;
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public'
               AND table_name IN ('assignment_deletion', 'blob_deletion'))
       OR EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'action'
                  AND column_name = 'redacted_by_action_id')
       OR EXISTS (SELECT 1 FROM pg_constraint WHERE conname IN ('action_redacted_by_fk', 'action_redacted_empty')) THEN
        RAISE EXCEPTION 'FAIL  0030 down: a table, a column or a constraint it added is still there';
    END IF;
    SELECT string_agg(proname, ', ') INTO left_over FROM pg_proc
    WHERE proname IN ('assignment_being_deleted', 'assignment_deletion_check_whole', 'event_guarded', 'grade_check_kept',
                      'document_check_kept', 'document_owner_assignment', 'assignment_check_kept');
    IF left_over IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  0030 down: the functions % are still there', left_over;
    END IF;
    SELECT string_agg(tgname, ', ') INTO left_over FROM pg_trigger
    WHERE tgname IN ('submission_no_truncate', 'grade_kept', 'grade_no_truncate', 'document_kept', 'document_no_truncate',
                     'assignment_kept', 'assignment_not_reused', 'assignment_no_truncate');
    IF left_over IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  0030 down: the triggers % are still there', left_over;
    END IF;
    SELECT string_agg(relname, ', ') INTO left_over FROM pg_class
    WHERE relname IN ('event_assignment_idx', 'grade_superseded_by_idx', 'submission_instructions_version_idx',
                      'grade_rubric_version_idx', 'action_course_idx', 'assignment_instructions_document_idx',
                      'assignment_rubric_document_idx', 'conversation_message_source_document_idx',
                      'conversation_message_source_version_idx');
    IF left_over IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  0030 down: the indexes % are still there', left_over;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger t JOIN pg_proc p ON p.oid = t.tgfoid
                   WHERE t.tgname = 'event_append_only' AND p.proname = 'reject_mutation') THEN
        RAISE EXCEPTION 'FAIL  0030 down: events are not append-only as 0001 made them';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM action WHERE id = '00000000-0000-0000-0030-0000000000b2' AND payload = '{}' AND result IS NULL)
       OR EXISTS (SELECT 1 FROM assignment WHERE id = '00000000-0000-0000-0030-000000000072')
       OR NOT EXISTS (SELECT 1 FROM action WHERE id = '00000000-0000-0000-0030-0000000000b3'
                      AND result->>'title' = 'Dosage quiz') THEN
        RAISE EXCEPTION 'FAIL  0030 down: the emptied action, the deletion''s own or the deleted quiz is not as it was left';
    END IF;
END $chk$;

BEGIN;
DO $chk$
DECLARE
    label text;
    stmt  text;
BEGIN
    -- A draft is deleted again, as 0001 let it be.
    INSERT INTO submission (id, assignment_id, course_id, student_member_id, attempt)
    VALUES ('00000000-0000-0000-0030-0000000000a3', '00000000-0000-0000-0020-000000000071', '00000000-0000-0000-0018-000000000041',
            '00000000-0000-0000-0018-000000000053', 9);
    DELETE FROM submission WHERE id = '00000000-0000-0000-0030-0000000000a3';
    FOR label, stmt IN VALUES
        ('an event is append-only', $q$DELETE FROM event$q$),
        ('a version is never deleted', $q$DELETE FROM document_version WHERE id = '00000000-0000-0000-0020-0000000000f1'$q$)
    LOOP
        BEGIN
            EXECUTE stmt;
            RAISE EXCEPTION 'FAIL  0030 down: % no longer holds', label;
        EXCEPTION WHEN restrict_violation THEN
            NULL;
        END;
    END LOOP;
END $chk$;
ROLLBACK;
\echo 'PASS  0030 down with an assignment deleted for good: the guards are as 0029 left them, and what was emptied stays empty'

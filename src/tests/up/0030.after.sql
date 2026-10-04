-- AIshie Core — after 0030_assignment_deletion.up.sql, in `make db-test-sql`
--
-- The migration made what a deletion needs, and skipped the two indexes
-- tests/up/0030.before.sql built first. Then NUR101's Care plan of
-- tests/up/0020, with Wei's draft and its two files (one of tests/up/0026,
-- with its rendition), is deleted for good, as assignment.delete deletes
-- it: refused outside the path, and once its record is written, deleted;
-- and a record of a deletion that leaves the assignment standing is
-- refused when it commits. Undone after: nothing that was there changes.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
DECLARE
    missing text;
BEGIN
    IF (SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'
        AND table_name IN ('assignment_deletion', 'blob_deletion')) <> 2
       OR NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'action'
                      AND column_name = 'redacted_by_action_id') THEN
        RAISE EXCEPTION 'FAIL  0030 up: no record of deletions, no queue of files, or no mark on an emptied action';
    END IF;
    SELECT string_agg(t, ', ') INTO missing
    FROM unnest(ARRAY['assignment_deletion_kept', 'assignment_deletion_no_truncate', 'assignment_deletion_whole',
                      'event_append_only', 'submission_frozen_after_submit', 'submission_no_truncate', 'grade_kept',
                      'grade_no_truncate', 'document_kept', 'document_no_truncate', 'document_version_append_only',
                      'assignment_kept', 'assignment_not_reused', 'assignment_no_truncate']) AS t
    WHERE NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = t AND NOT tgisinternal);
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  0030 up: no trigger %', missing;
    END IF;
    SELECT string_agg(i, ', ') INTO missing
    FROM unnest(ARRAY['event_assignment_idx', 'grade_superseded_by_idx', 'submission_instructions_version_idx',
                      'grade_rubric_version_idx', 'action_course_idx', 'assignment_instructions_document_idx',
                      'assignment_rubric_document_idx', 'conversation_message_source_document_idx',
                      'conversation_message_source_version_idx', 'blob_deletion_due_idx']) AS i
    WHERE NOT EXISTS (SELECT 1 FROM pg_class c JOIN pg_index x ON x.indexrelid = c.oid
                      WHERE c.relname = i AND x.indisvalid);
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  0030 up: no valid index %', missing;
    END IF;
END $chk$;

BEGIN;
-- b1 Lin's deletion of the Care plan
INSERT INTO action (id, actor_id, course_id, member_id, action_type, target_type, target_id, payload, payload_hash,
                    idempotency_key, authz_result, status, executed_at)
VALUES ('00000000-0000-0000-0030-0000000000b1', '00000000-0000-0000-0018-000000000031', '00000000-0000-0000-0018-000000000041',
        '00000000-0000-0000-0018-000000000051', 'assignment.delete', 'assignment', '00000000-0000-0000-0020-000000000071',
        '{}', repeat('0', 64), 'up30-b1', 'autonomous', 'executed', now());

DO $chk$
DECLARE
    label text;
    stmt  text;
BEGIN
    FOR label, stmt IN VALUES
        ('Wei''s draft is not deleted outside its assignment''s deletion',
         $q$DELETE FROM submission WHERE id = '00000000-0000-0000-0020-0000000000a1'$q$),
        ('nor is her file',
         $q$DELETE FROM document WHERE id = '00000000-0000-0000-0026-0000000000e8'$q$),
        ('nor the assignment',
         $q$DELETE FROM assignment WHERE id = '00000000-0000-0000-0020-000000000071'$q$),
        ('nor is the assignment recorded as deleted while it stands',
         $q$INSERT INTO assignment_deletion (assignment_id, course_id, title, was_published, action_id, deleted_by_actor_id,
                                             deleted_by_member_id, deleted_at, submissions, grades, files, documents, proposals, totals)
            VALUES ('00000000-0000-0000-0020-000000000071', '00000000-0000-0000-0018-000000000041', 'Care plan', true,
                    '00000000-0000-0000-0030-0000000000b1', '00000000-0000-0000-0018-000000000031',
                    '00000000-0000-0000-0018-000000000051', now(), 1, 0, 2, 0, 0, 0);
            SET CONSTRAINTS assignment_deletion_whole IMMEDIATE$q$)
    LOOP
        BEGIN
            EXECUTE stmt;
            RAISE EXCEPTION 'FAIL  0030 up: % — it was taken', label;
        EXCEPTION WHEN check_violation OR restrict_violation THEN
            NULL;
        END;
    END LOOP;
END $chk$;
SET CONSTRAINTS ALL DEFERRED;

-- The deletion, as assignment.delete makes it.
INSERT INTO assignment_deletion (assignment_id, course_id, title, was_published, action_id, deleted_by_actor_id,
                                 deleted_by_member_id, deleted_at, submissions, grades, files, documents, proposals, totals)
VALUES ('00000000-0000-0000-0020-000000000071', '00000000-0000-0000-0018-000000000041', 'Care plan', true,
        '00000000-0000-0000-0030-0000000000b1', '00000000-0000-0000-0018-000000000031', '00000000-0000-0000-0018-000000000051',
        now(), 1, 0, 2, 0, 0, 0);
INSERT INTO blob_deletion (storage_key, course_id, queued_by_action_id, queued_at, next_try_at)
SELECT f.storage_key, '00000000-0000-0000-0018-000000000041', '00000000-0000-0000-0030-0000000000b1', now(), now()
FROM document_version_file f
WHERE f.document_id IN ('00000000-0000-0000-0020-0000000000e5', '00000000-0000-0000-0026-0000000000e8');
UPDATE document_version SET body_md = NULL, purged_at = now(), purged_by_actor_id = '00000000-0000-0000-0018-000000000031',
                            purge_reason = 'assignment_deleted'
WHERE document_id IN ('00000000-0000-0000-0020-0000000000e5', '00000000-0000-0000-0026-0000000000e8') AND purged_at IS NULL;
UPDATE document SET published_version_id = NULL
WHERE id IN ('00000000-0000-0000-0020-0000000000e5', '00000000-0000-0000-0026-0000000000e8');
DELETE FROM document_version WHERE document_id IN ('00000000-0000-0000-0020-0000000000e5', '00000000-0000-0000-0026-0000000000e8');
DELETE FROM document WHERE id IN ('00000000-0000-0000-0020-0000000000e5', '00000000-0000-0000-0026-0000000000e8');
DELETE FROM submission WHERE assignment_id = '00000000-0000-0000-0020-000000000071';
DELETE FROM event WHERE assignment_id = '00000000-0000-0000-0020-000000000071';
DELETE FROM assignment WHERE id = '00000000-0000-0000-0020-000000000071';
SET CONSTRAINTS ALL IMMEDIATE;
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM submission WHERE id = '00000000-0000-0000-0020-0000000000a1')
       OR EXISTS (SELECT 1 FROM document WHERE id IN ('00000000-0000-0000-0020-0000000000e5', '00000000-0000-0000-0026-0000000000e8'))
       OR EXISTS (SELECT 1 FROM document_version_file WHERE id = '00000000-0000-0000-0026-0000000000d4')
       OR EXISTS (SELECT 1 FROM file_rendition r WHERE r.file_id = '00000000-0000-0000-0026-0000000000d4') THEN
        RAISE EXCEPTION 'FAIL  0030 up: the Care plan''s work is still there';
    END IF;
    IF (SELECT count(*) FROM blob_deletion WHERE storage_key IN ('up20/f8', 'documents/up26/d4')) <> 2 THEN
        RAISE EXCEPTION 'FAIL  0030 up: its files are not queued to leave the store';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM document WHERE id = '00000000-0000-0000-0020-0000000000e1')
       OR NOT EXISTS (SELECT 1 FROM assignment_deletion WHERE assignment_id = '00000000-0000-0000-0020-000000000071') THEN
        RAISE EXCEPTION 'FAIL  0030 up: what was not its went with it, or its record';
    END IF;
END $chk$;
ROLLBACK;
\echo 'PASS  0030 up skips the indexes built first, refuses a deletion outside its path, and deletes an assignment with its work inside it'

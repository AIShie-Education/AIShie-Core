-- AIshie Core — after 0026_file_renditions.down.sql, in `make db-test-sql`
--
-- The renditions are gone, done and claimed alike, and what queued and
-- held them; the files they were of stay as they were, and a file recorded
-- now, as the release before records one, is recorded and queued for
-- nothing.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'file_rendition') THEN
        RAISE EXCEPTION 'FAIL  0026 down: the renditions are still there';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_proc WHERE proname IN ('file_rendition_convertible', 'file_rendition_guarded',
                                                       'document_version_file_queue_rendition',
                                                       'conversation_attachment_queue_rendition'))
       OR EXISTS (SELECT 1 FROM pg_trigger WHERE tgname IN ('document_version_file_rendition_queued',
                                                            'conversation_attachment_rendition_queued')) THEN
        RAISE EXCEPTION 'FAIL  0026 down: a function or a trigger it added is still there';
    END IF;
    IF (SELECT count(*) FROM document_version_file WHERE id IN ('00000000-0000-0000-0026-0000000000d1',
                                                                '00000000-0000-0000-0026-0000000000d4')) <> 2
       OR NOT EXISTS (SELECT 1 FROM conversation_attachment WHERE id = '00000000-0000-0000-0026-0000000000a1') THEN
        RAISE EXCEPTION 'FAIL  0026 down: a file that had a rendition is gone';
    END IF;
END $chk$;

BEGIN;
INSERT INTO document_version (id, document_id, seq, storage_key, content_type, byte_size, author_member_id)
VALUES ('00000000-0000-0000-0026-0000000000fc', '00000000-0000-0000-0026-0000000000e9', 2, 'courses/down26/fc',
        'application/msword', 10, '00000000-0000-0000-0018-000000000051');
SET CONSTRAINTS ALL IMMEDIATE;
ROLLBACK;
\echo 'PASS  0026 down with renditions done and claimed, which go, and the files they were of, which stay'

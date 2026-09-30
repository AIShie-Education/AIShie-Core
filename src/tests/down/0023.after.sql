-- AIshie Core — after 0023_document_version_files.down.sql, in `make db-test-sql`
--
-- The files' rows are gone, and a version keeps its first file in its own
-- columns, where the release before reads it, with that file's text version
-- as the version's: the fourth version of Week 1 its slides', still
-- waiting, and not its handout's. The text versions are one to a version
-- again, queued as a version with a file is added.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'document_version_file') THEN
        RAISE EXCEPTION 'FAIL  0023 down: the files are still there';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_proc WHERE proname IN ('document_file_name', 'document_version_files_whole',
                                                       'document_version_text_one_file_at_a_time', 'document_version_forget_files')) THEN
        RAISE EXCEPTION 'FAIL  0023 down: a function of the files is still there';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM document_version WHERE id = '00000000-0000-0000-0022-0000000000a1'
                   AND storage_key = 'documents/down22/d1' AND content_type = 'application/pdf' AND byte_size = 100
                   AND body_md = 'Read the handout first.') THEN
        RAISE EXCEPTION 'FAIL  0023 down: the version lost its first file';
    END IF;
    IF (SELECT string_agg(status, ' ') FROM document_version_text WHERE version_id = '00000000-0000-0000-0022-0000000000a1')
       IS DISTINCT FROM 'pending' THEN
        RAISE EXCEPTION 'FAIL  0023 down: the version''s text is not its first file''s';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM document_version_text WHERE version_id = '00000000-0000-0000-0022-0000000000a2') THEN
        RAISE EXCEPTION 'FAIL  0023 down: the text of a version of one file is gone';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'document_version_text_queued')
       OR NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'document_version_text_purged')
       OR EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'document_version_text' AND column_name = 'file_id') THEN
        RAISE EXCEPTION 'FAIL  0023 down: text versions are not one to a version, as 0020 had them';
    END IF;
END $chk$;
\echo 'PASS  0023 down with a version of three files, which keeps its first, and that file''s text as its own'

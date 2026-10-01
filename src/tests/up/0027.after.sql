-- AIshie Core — after 0027_drop_deprecated.up.sql, in `make db-test-sql`
--
-- What 0023 and 0025 kept for the release before is gone: a version's own
-- file columns, an actor's site chat credential, the name a file was given
-- after its document, the refusal of two files' texts written at once, and
-- an agent made an mcp agent when it is registered naming no hosting, which
-- is now refused. Week 4 keeps its files, in order, and its program's
-- text; the version purged keeps who purged it, and no file. A version is
-- still text, files or both, its files numbered from 1, at commit.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
DECLARE
    got text;
BEGIN
    SELECT string_agg(table_name || '.' || column_name, ' ' ORDER BY table_name, column_name) INTO got
    FROM information_schema.columns
    WHERE table_schema = 'public'
      AND ((table_name = 'document_version' AND column_name IN ('storage_key', 'content_type', 'byte_size', 'checksum'))
           OR (table_name = 'actor' AND column_name = 'site_chat_credential_id'));
    IF got IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  0027 up: % still there', got;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_proc WHERE proname IN ('document_file_name', 'document_version_text_one_file_at_a_time',
                                                       'actor_hosting_by_default'))
       OR EXISTS (SELECT 1 FROM pg_trigger WHERE tgname IN ('document_version_text_one_file_at_a_time', 'actor_hosting_default'))
       OR EXISTS (SELECT 1 FROM pg_constraint WHERE conname IN ('actor_site_chat_is_agent', 'actor_site_chat_credential_fk',
                                                                 'credential_id_actor_key',
                                                                 'document_version_has_content', 'document_version_file_described')) THEN
        RAISE EXCEPTION 'FAIL  0027 up: something kept for the release before is still there';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'document_version_files_whole' AND tgqual IS NULL) THEN
        RAISE EXCEPTION 'FAIL  0027 up: a version is not checked whole at commit, whatever it holds';
    END IF;
    SELECT string_agg(f.position || ':' || f.filename || ':' || f.storage_key || ':' || coalesce(t.status, '-'), ' ' ORDER BY f.position)
      INTO got
    FROM document_version_file f
    LEFT JOIN document_version_text t ON t.file_id = f.id AND t.version_id = f.version_id
    WHERE f.version_id = '00000000-0000-0000-0027-0000000000f1';
    IF got IS DISTINCT FROM '1:slides.pdf:documents/up27/d1:pending 2:loops.py:documents/up27/d2:done' THEN
        RAISE EXCEPTION 'FAIL  0027 up: Week 4''s files are %', coalesce(got, 'none');
    END IF;
    IF NOT EXISTS (SELECT 1 FROM document_version WHERE id = '00000000-0000-0000-0027-0000000000f1'
                   AND body_md = 'Slides, and the program to run.' AND purged_at IS NULL)
       OR NOT EXISTS (SELECT 1 FROM document_version WHERE id = '00000000-0000-0000-0027-0000000000f2' AND body_md IS NULL
                      AND purged_by_actor_id = '00000000-0000-0000-0018-000000000031')
       OR EXISTS (SELECT 1 FROM document_version_file WHERE version_id = '00000000-0000-0000-0027-0000000000f2') THEN
        RAISE EXCEPTION 'FAIL  0027 up: a version changed';
    END IF;
END $chk$;

-- As the release before would write: an agent naming no hosting, and a
-- version with nothing in it.
DO $chk$
BEGIN
    BEGIN
        INSERT INTO actor (kind, display_name, owner_actor_id, created_by_actor_id)
        VALUES ('agent', 'Old style', '00000000-0000-0000-0025-000000000031', '00000000-0000-0000-0025-000000000031');
        RAISE EXCEPTION 'FAIL  0027 up: an agent was registered naming no hosting';
    EXCEPTION WHEN check_violation THEN
        NULL;
    END;
END $chk$;
BEGIN;
INSERT INTO document_version (id, document_id, seq, author_member_id)
VALUES ('00000000-0000-0000-0027-0000000000f9', '00000000-0000-0000-0027-0000000000e1', 9, '00000000-0000-0000-0018-000000000051');
DO $chk$
BEGIN
    BEGIN
        SET CONSTRAINTS ALL IMMEDIATE;
        RAISE EXCEPTION 'FAIL  0027 up: a version of neither text nor files was taken';
    EXCEPTION WHEN check_violation THEN
        NULL;
    END;
END $chk$;
ROLLBACK;
\echo 'PASS  0027 up drops what 0023 and 0025 kept for the release before, and keeps every version''s files and texts'

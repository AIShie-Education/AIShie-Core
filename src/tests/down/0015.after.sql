-- AIshie Core — after 0015_flexible_records.down.sql, in `make db-test-sql`
--
-- The override and the tombstone are gone, and versions are append-only
-- again; the total stays as it was worked out, and the purged version stays,
-- as empty text, with nothing of what was purged.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public'
               AND (table_name, column_name) IN (('grade', 'override_score'), ('grade', 'override_reason'),
                    ('grade', 'override_by_member_id'), ('grade', 'overridden_at'),
                    ('document', 'purged_at'), ('document', 'purged_by_actor_id'), ('document', 'purge_reason'),
                    ('document_version', 'purged_at'), ('document_version', 'purged_by_actor_id'),
                    ('document_version', 'purge_reason'))) THEN
        RAISE EXCEPTION 'FAIL  0015 down: a column it added is still there';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
               WHERE n.nspname = 'public' AND p.proname IN ('document_version_guarded', 'document_purge_kept')) THEN
        RAISE EXCEPTION 'FAIL  0015 down: a function it added is still there';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM grade WHERE id = '00000000-0000-0000-0015-0000000000d2' AND score = 89.5
                   AND superseded_by IS NULL AND posted_at IS NOT NULL) THEN
        RAISE EXCEPTION 'FAIL  0015 down: the total as worked out did not stay';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM document_version WHERE id = '00000000-0000-0000-0015-0000000000f1'
                   AND body_md = '' AND storage_key IS NULL AND checksum IS NULL) THEN
        RAISE EXCEPTION 'FAIL  0015 down: the purged version did not stay as empty text';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM document_version WHERE id = '00000000-0000-0000-0015-0000000000f2'
                   AND body_md = 'The notes, without the class list.') THEN
        RAISE EXCEPTION 'FAIL  0015 down: the version kept did not stay as it was';
    END IF;
    BEGIN
        UPDATE document_version SET body_md = 'back again' WHERE id = '00000000-0000-0000-0015-0000000000f1';
        RAISE EXCEPTION 'FAIL  0015 down: a version could be changed';
    EXCEPTION WHEN restrict_violation THEN
        NULL;
    END;
    BEGIN
        INSERT INTO document_version (document_id, seq, author_member_id)
        VALUES ('00000000-0000-0000-0015-0000000000e1', 3, '00000000-0000-0000-0015-000000000051');
        RAISE EXCEPTION 'FAIL  0015 down: a version with no text and no file was taken';
    EXCEPTION WHEN check_violation THEN
        NULL;
    END;
END $chk$;
\echo 'PASS  0015 down with an overridden total and a purged version'

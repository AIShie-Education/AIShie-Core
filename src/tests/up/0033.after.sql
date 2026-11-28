-- AIshie Core — after 0033_document_search_passages.up.sql, in make db-test-sql
-- Built-in FTS only, all three indexes, both source shapes, and same-
-- transaction deletion of every passage when its version is purged.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
DECLARE missing text;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public'
                   AND table_name = 'document_search_passages') THEN
        RAISE EXCEPTION 'FAIL  0033 up: no passage table';
    END IF;
    SELECT string_agg(i, ', ') INTO missing
    FROM unnest(ARRAY['document_search_passages_search_idx', 'document_search_passages_document_idx',
                      'document_search_passages_file_idx']) AS i
    WHERE NOT EXISTS (SELECT 1 FROM pg_class c JOIN pg_index x ON x.indexrelid = c.oid
                      WHERE c.relname = i AND x.indisvalid);
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  0033 up: no valid index %', missing;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_index x JOIN pg_class c ON c.oid = x.indexrelid
                   JOIN pg_am a ON a.oid = c.relam
                   WHERE c.relname = 'document_search_passages_search_idx' AND a.amname = 'gin') THEN
        RAISE EXCEPTION 'FAIL  0033 up: the FTS index is not GIN';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname IN ('pg_trgm', 'pg_bigm', 'pgroonga')) THEN
        RAISE EXCEPTION 'FAIL  0033 up: search requires an extension';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'document_version_search_passages_purged' AND NOT tgisinternal) THEN
        RAISE EXCEPTION 'FAIL  0033 up: no purge trigger';
    END IF;
END $chk$;

BEGIN;
INSERT INTO document_search_passages (id, course_id, document_id, version_id, file_id, text_source, text_revision,
                                      section_kind, section_no, part_no, content, search_text)
VALUES ('00000000-0000-0000-0033-0000000000b1', '00000000-0000-0000-0018-000000000041',
        '00000000-0000-0000-0033-0000000000e1', '00000000-0000-0000-0033-0000000000f1', NULL,
        'body_md', 1, 'body', 0, 1, 'Migration body', 'migration body'),
       ('00000000-0000-0000-0033-0000000000b2', '00000000-0000-0000-0018-000000000041',
        '00000000-0000-0000-0033-0000000000e1', '00000000-0000-0000-0033-0000000000f2',
        '00000000-0000-0000-0033-0000000000d1', 'ai', 4, 'slide', 1, 1, 'Migration slide', 'migration slide');
DO $chk$
BEGIN
    IF (SELECT count(*) FROM document_search_passages WHERE search @@ to_tsquery('simple', 'migration')) <> 2 THEN
        RAISE EXCEPTION 'FAIL  0033 up: the generated tsvector did not find both sources';
    END IF;
END $chk$;
UPDATE document_version
SET body_md = NULL, purged_at = now(), purged_by_actor_id = '00000000-0000-0000-0018-000000000031',
    purge_reason = 'Migration purge test'
WHERE id = '00000000-0000-0000-0033-0000000000f1';
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM document_search_passages WHERE version_id = '00000000-0000-0000-0033-0000000000f1')
       OR NOT EXISTS (SELECT 1 FROM document_search_passages WHERE version_id = '00000000-0000-0000-0033-0000000000f2') THEN
        RAISE EXCEPTION 'FAIL  0033 up: purging one body removed the wrong passages';
    END IF;
END $chk$;
UPDATE document_version
SET body_md = NULL, purged_at = now(), purged_by_actor_id = '00000000-0000-0000-0018-000000000031',
    purge_reason = 'Migration purge test'
WHERE id = '00000000-0000-0000-0033-0000000000f2';
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM document_search_passages WHERE version_id IN
               ('00000000-0000-0000-0033-0000000000f1', '00000000-0000-0000-0033-0000000000f2')) THEN
        RAISE EXCEPTION 'FAIL  0033 up: purging a file version left passages';
    END IF;
END $chk$;
ROLLBACK;
\echo 'PASS  0033 up uses built-in GIN FTS and purges body and file passages with their versions'

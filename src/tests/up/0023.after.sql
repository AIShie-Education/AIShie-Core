-- AIshie Core — after 0023_document_version_files.up.sql, in `make db-test-sql`
--
-- Every version with a file has it as its one file, at position 1, as its
-- own columns name it, and named after its document: the handout's name
-- made a name, with its type's extension; plan.pdf as it was. A purged
-- version, and a version of text alone, have none. Each text version is its
-- version's one file's, the rubric's staff text and the handout's, queued as
-- it was added, among them, and nothing else is queued.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
DECLARE
    names text;
    texts text;
BEGIN
    IF EXISTS (SELECT 1 FROM document_version v
               WHERE v.storage_key IS NOT NULL AND v.purged_at IS NULL
                 AND NOT EXISTS (SELECT 1 FROM document_version_file f
                                 WHERE f.version_id = v.id AND f.position = 1 AND f.storage_key = v.storage_key
                                   AND f.content_type = v.content_type AND f.byte_size = v.byte_size
                                   AND f.checksum IS NOT DISTINCT FROM v.checksum AND f.created_at = v.created_at
                                   AND f.document_id = v.document_id)) THEN
        RAISE EXCEPTION 'FAIL  0023 up: a version''s file was not recorded as its first';
    END IF;
    IF EXISTS (SELECT 1 FROM document_version_file f JOIN document_version v ON v.id = f.version_id
               WHERE v.storage_key IS NULL OR f.position <> 1) THEN
        RAISE EXCEPTION 'FAIL  0023 up: a version was given a file it did not have';
    END IF;
    SELECT string_agg(right(v.id::text, 2) || '=' || f.filename, ' | ' ORDER BY v.id) INTO names
    FROM document_version_file f JOIN document_version v ON v.id = f.version_id
    WHERE v.id IN ('00000000-0000-0000-0022-0000000000f1', '00000000-0000-0000-0020-0000000000f1',
                   '00000000-0000-0000-0020-0000000000f8');
    IF names IS DISTINCT FROM 'f1=Week 1.pdf | f8=plan.pdf | f1=Week 2 3 handout.docx' THEN
        RAISE EXCEPTION 'FAIL  0023 up: the files are named %', names;
    END IF;
    SELECT string_agg(right(t.version_id::text, 2) || ':' || t.status || coalesce(':' || t.body, ''), ' ' ORDER BY t.version_id) INTO texts
    FROM document_version_text t JOIN document_version_file f ON f.id = t.file_id AND f.version_id = t.version_id AND f.position = 1
    WHERE t.version_id::text LIKE '00000000-0000-0000-002%';
    IF texts IS DISTINCT FROM 'f2:pending f3:pending f4:done:## Criteria f1:pending' THEN
        RAISE EXCEPTION 'FAIL  0023 up: the text versions are %', coalesce(texts, 'none');
    END IF;
    IF (SELECT count(*) FROM document_version_text WHERE version_id::text LIKE '00000000-0000-0000-002%') <> 4 THEN
        RAISE EXCEPTION 'FAIL  0023 up: something else was queued';
    END IF;
END $chk$;
\echo 'PASS  0023 up records every version''s file as its one file, named after its document, and each text version as that file''s'

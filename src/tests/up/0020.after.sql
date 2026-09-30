-- AIshie Core — after 0020_document_text.up.sql, in `make db-test-sql`
--
-- The backfill: what is read now is queued, and nothing else. Week 1's
-- published version and its latest draft, and the rubric, each pending,
-- marked backfill, queued as of when the version was added; not the first
-- version of Week 1, which neither is, nor the archived slides, the purged
-- file, the text alone, Wei's file or the archived course's slides.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
DECLARE
    queued text;
BEGIN
    SELECT string_agg(right(version_id::text, 2), ' ' ORDER BY version_id) INTO queued
      FROM document_version_text WHERE version_id::text LIKE '00000000-0000-0000-0020-%';
    IF queued IS DISTINCT FROM 'f2 f3 f4' THEN
        RAISE EXCEPTION 'FAIL  0020 up: the backfill queued %, not f2 f3 f4', coalesce(queued, 'nothing');
    END IF;
    IF EXISTS (SELECT 1 FROM document_version_text t JOIN document_version v ON v.id = t.version_id
               JOIN document d ON d.id = v.document_id
               WHERE t.version_id::text LIKE '00000000-0000-0000-0020-%'
                 AND NOT (t.status = 'pending' AND t.backfill AND t.attempts = 0 AND t.revision = 1 AND t.body IS NULL
                          AND t.queued_at = v.created_at AND t.course_id = d.course_id AND t.document_id = d.id)) THEN
        RAISE EXCEPTION 'FAIL  0020 up: a backfilled text version is not pending as of its version';
    END IF;
    IF EXISTS (SELECT 1 FROM actor WHERE kind = 'service') THEN
        RAISE EXCEPTION 'FAIL  0020 up: a service was made';
    END IF;
END $chk$;
\echo 'PASS  0020 up queues the published and the latest versions with files, of documents and courses that are not archived, and nothing else'

-- AIshie Core — after 0026_file_renditions.up.sql, in `make db-test-sql`
--
-- Every Office or OpenDocument file there was is queued for its PDF, marked
-- backfill and dated as its file: the handout of tests/up/0023, Week 2's
-- slides, Wei's essay uploaded as bytes of no particular type, NUR102's
-- slides in a course that is archived, and the case study Wei sent; and
-- nothing else, not a PDF, a spreadsheet a browser called an Excel file,
-- notes or a photo. A file recorded afterwards is queued as it is, ahead of
-- the backfill, whatever kind of document it is of; a PDF is not.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
DECLARE
    got text;
BEGIN
    SELECT string_agg(coalesce(f.filename, a.filename) || ':' || r.status || ':' || r.backfill || ':' || r.attempts
                      || ':' || (r.queued_at = coalesce(f.created_at, a.created_at))
                      || ':' || (r.course_id = coalesce(d.course_id, a.course_id)),
                      ' | ' ORDER BY coalesce(f.filename, a.filename)) INTO got
    FROM file_rendition r
    LEFT JOIN document_version_file f ON f.id = r.file_id
    LEFT JOIN document d ON d.id = f.document_id
    LEFT JOIN conversation_attachment a ON a.id = r.attachment_id;
    IF got IS DISTINCT FROM 'Essay.DOCX:queued:true:0:true:true | Week 2 3 handout.docx:queued:true:0:true:true'
                            || ' | case study.rtf:queued:true:0:true:true | week1.odp:queued:true:0:true:true'
                            || ' | week1.pptx:queued:true:0:true:true' THEN
        RAISE EXCEPTION 'FAIL  0026 up: the renditions queued are %', coalesce(got, 'none');
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'document_version_file_rendition_queued')
       OR NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'conversation_attachment_rendition_queued')
       OR NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'file_rendition_guarded') THEN
        RAISE EXCEPTION 'FAIL  0026 up: no trigger queues a file, or holds a rendition';
    END IF;
END $chk$;

-- A file recorded now, a feedback file of Week 2's course, as any release
-- records one; undone after.
BEGIN;
INSERT INTO document (id, course_id, kind, title, status, submission_id)
VALUES ('00000000-0000-0000-0026-0000000000ea', '00000000-0000-0000-0018-000000000041', 'submission', 'more', 'active',
        '00000000-0000-0000-0020-0000000000a1');
INSERT INTO document_version (id, document_id, seq, storage_key, content_type, byte_size, author_member_id)
VALUES ('00000000-0000-0000-0026-0000000000fa', '00000000-0000-0000-0026-0000000000ea', 1, 'courses/up26/fa',
        'application/vnd.ms-excel', 10, '00000000-0000-0000-0018-000000000053');
INSERT INTO document_version (id, document_id, seq, storage_key, content_type, byte_size, author_member_id)
VALUES ('00000000-0000-0000-0026-0000000000fb', '00000000-0000-0000-0026-0000000000ea', 2, 'courses/up26/fb',
        'application/pdf', 10, '00000000-0000-0000-0018-000000000053');
SET CONSTRAINTS ALL IMMEDIATE;
DO $chk$
DECLARE
    got text;
BEGIN
    -- The release before writes the file in the version's own columns
    -- alone: recorded at commit as its one file, named after its document
    -- with its type's extension, and queued then.
    SELECT string_agg(f.filename || ':' || r.status || ':' || r.backfill, ' ') INTO got
    FROM file_rendition r JOIN document_version_file f ON f.id = r.file_id
    WHERE f.document_id = '00000000-0000-0000-0026-0000000000ea';
    IF got IS DISTINCT FROM 'more.xls:queued:false' THEN
        RAISE EXCEPTION 'FAIL  0026 up: a file recorded afterwards is queued as %', coalesce(got, 'nothing');
    END IF;
END $chk$;
ROLLBACK;
\echo 'PASS  0026 up queues every Office and OpenDocument file there is behind what comes after, and nothing else'

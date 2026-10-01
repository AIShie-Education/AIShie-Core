-- AIshie Core — before 0026_file_renditions.up.sql, in `make db-test-sql`
--
-- What the migration finds, beside the files of tests/up/0020 (PDFs) and
-- tests/up/0023 (the handout, a Word file whose type says more than its
-- type): Week 2, with slides, a spreadsheet a browser called an Excel file,
-- and notes; Wei's essay, uploaded as bytes of no particular type; NUR102's
-- slides as an OpenDocument presentation, in a course that is archived; and
-- a case study Wei sent Ho, as RTF, with a photo.
-- Committed, so that the migration runs over it; it stays, for the redo and
-- the down after it (tests/down/0026.*), and the downs after that drop it
-- with everything else.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

-- e9 Week 2, f1 its one version, and d1..d3 its files
INSERT INTO document (id, course_id, kind, title, status)
VALUES ('00000000-0000-0000-0026-0000000000e9', '00000000-0000-0000-0018-000000000041', 'material', 'Week 2', 'active');
INSERT INTO document_version (id, document_id, seq, storage_key, content_type, byte_size, author_member_id, created_at)
VALUES ('00000000-0000-0000-0026-0000000000f1', '00000000-0000-0000-0026-0000000000e9', 1, 'documents/up26/d1',
        'application/vnd.openxmlformats-officedocument.presentationml.presentation', 100,
        '00000000-0000-0000-0018-000000000051', '2026-09-10 09:00:00+00');
INSERT INTO document_version_file (id, version_id, document_id, position, filename, storage_key, content_type, byte_size,
                                   created_at) VALUES
    ('00000000-0000-0000-0026-0000000000d1', '00000000-0000-0000-0026-0000000000f1', '00000000-0000-0000-0026-0000000000e9', 1,
     'week1.pptx', 'documents/up26/d1', 'application/vnd.openxmlformats-officedocument.presentationml.presentation', 100,
     '2026-09-10 09:00:00+00'),
    ('00000000-0000-0000-0026-0000000000d2', '00000000-0000-0000-0026-0000000000f1', '00000000-0000-0000-0026-0000000000e9', 2,
     'vitals.csv', 'documents/up26/d2', 'application/vnd.ms-excel', 10, '2026-09-10 09:00:00+00'),
    ('00000000-0000-0000-0026-0000000000d3', '00000000-0000-0000-0026-0000000000f1', '00000000-0000-0000-0026-0000000000e9', 3,
     'notes.txt', 'documents/up26/d3', 'text/plain', 10, '2026-09-10 09:00:00+00');

-- e8 Wei's essay, on her draft of tests/up/0020, and f2 its one version, d4 its file
INSERT INTO document (id, course_id, kind, title, status, submission_id)
VALUES ('00000000-0000-0000-0026-0000000000e8', '00000000-0000-0000-0018-000000000041', 'submission', 'essay.docx', 'active',
        '00000000-0000-0000-0020-0000000000a1');
INSERT INTO document_version (id, document_id, seq, storage_key, content_type, byte_size, author_member_id, created_at)
VALUES ('00000000-0000-0000-0026-0000000000f2', '00000000-0000-0000-0026-0000000000e8', 1, 'documents/up26/d4',
        'application/octet-stream', 50, '00000000-0000-0000-0018-000000000053', '2026-09-11 09:00:00+00');
INSERT INTO document_version_file (id, version_id, document_id, position, filename, storage_key, content_type, byte_size,
                                   created_at)
VALUES ('00000000-0000-0000-0026-0000000000d4', '00000000-0000-0000-0026-0000000000f2', '00000000-0000-0000-0026-0000000000e8', 1,
        'Essay.DOCX', 'documents/up26/d4', 'application/octet-stream', 50, '2026-09-11 09:00:00+00');

-- f3 NUR102's slides, a second version, and d5 its file
INSERT INTO document_version (id, document_id, seq, storage_key, content_type, byte_size, author_member_id, created_at)
VALUES ('00000000-0000-0000-0026-0000000000f3', '00000000-0000-0000-0020-0000000000e7', 2, 'documents/up26/d5',
        'application/vnd.oasis.opendocument.presentation', 70, '00000000-0000-0000-0020-000000000055', '2025-09-08 09:00:00+00');
INSERT INTO document_version_file (id, version_id, document_id, position, filename, storage_key, content_type, byte_size,
                                   created_at)
VALUES ('00000000-0000-0000-0026-0000000000d5', '00000000-0000-0000-0026-0000000000f3', '00000000-0000-0000-0020-0000000000e7', 1,
        'week1.odp', 'documents/up26/d5', 'application/vnd.oasis.opendocument.presentation', 70, '2025-09-08 09:00:00+00');

-- a1 the case study and a2 the photo, on Wei's question to Ho
INSERT INTO conversation_attachment (id, message_id, conversation_id, course_id, position, filename, storage_key, content_type,
                                     byte_size, created_at)
SELECT '00000000-0000-0000-0026-0000000000a1', m.id, m.conversation_id, m.course_id, 1, 'case study.rtf',
       'conversations/up26/a1', 'text/rtf', 30, m.created_at
FROM conversation_message m WHERE m.id = '00000000-0000-0000-0018-000000000c21';
INSERT INTO conversation_attachment (id, message_id, conversation_id, course_id, position, filename, storage_key, content_type,
                                     byte_size, created_at)
SELECT '00000000-0000-0000-0026-0000000000a2', m.id, m.conversation_id, m.course_id, 2, 'wound.png',
       'conversations/up26/a2', 'image/png', 30, m.created_at
FROM conversation_message m WHERE m.id = '00000000-0000-0000-0018-000000000c21';

COMMIT;

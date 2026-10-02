-- AIshie Core — after 0029_message_sources.up.sql, in `make db-test-sql`
--
-- An answer says what it relied on: the tutor answers Wei's question in the
-- conversation of tests/up/0018, relying on Week 4 of tests/up/0027 (page 3
-- of its slides) and on Week 2 of tests/up/0026 (its version, no file).
-- What it names is held to the answer's course and to the version's files;
-- a question names none, nor says it relied on none, and nor does an answer
-- that said nothing of its sources; a purged version is no source, nor a
-- student's work.
-- When Week 4's version is purged, its source stays and names no file. Undone
-- after: nothing that was there changes.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public'
                   AND table_name = 'conversation_message_source')
       OR (SELECT count(*) FROM pg_trigger WHERE tgname IN ('conversation_message_source_with_its_answer',
                                                            'conversation_message_source_append_only',
                                                            'conversation_message_source_no_truncate')) <> 3 THEN
        RAISE EXCEPTION 'FAIL  0029 up: no table of sources, or not every trigger that holds it';
    END IF;
    IF EXISTS (SELECT 1 FROM conversation_message WHERE sources_stated) THEN
        RAISE EXCEPTION 'FAIL  0029 up: a message written before says what it relied on';
    END IF;
END $chk$;

BEGIN;
-- c32 the tutor's answer to c31
INSERT INTO conversation_message (id, conversation_id, course_id, seq, author_member_id, in_reply_to_message_id, body,
                                  created_by_action_id, sources_stated)
VALUES ('00000000-0000-0000-0029-000000000c32', '00000000-0000-0000-0018-0000000000c3', '00000000-0000-0000-0018-000000000041',
        (SELECT max(seq) + 1 FROM conversation_message WHERE conversation_id = '00000000-0000-0000-0018-0000000000c3'),
        '00000000-0000-0000-0018-000000000056', '00000000-0000-0000-0018-000000000c31', 'A plan of the care a patient needs.',
        '00000000-0000-0000-0018-0000000000b3', true);
INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id, file_id, file_version_id, page)
VALUES ('00000000-0000-0000-0029-000000000c32', '00000000-0000-0000-0018-000000000041', 1,
        '00000000-0000-0000-0027-0000000000e1', '00000000-0000-0000-0027-0000000000f1',
        '00000000-0000-0000-0027-0000000000d1', '00000000-0000-0000-0027-0000000000f1', 3);
INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id)
VALUES ('00000000-0000-0000-0029-000000000c32', '00000000-0000-0000-0018-000000000041', 2,
        '00000000-0000-0000-0026-0000000000e9', '00000000-0000-0000-0026-0000000000f1');

-- c33 an answer to c31 that says nothing of its sources, as the release
-- before writes every answer
INSERT INTO conversation_message (id, conversation_id, course_id, seq, author_member_id, in_reply_to_message_id, body,
                                  created_by_action_id)
VALUES ('00000000-0000-0000-0029-000000000c33', '00000000-0000-0000-0018-0000000000c3', '00000000-0000-0000-0018-000000000041',
        (SELECT max(seq) + 1 FROM conversation_message WHERE conversation_id = '00000000-0000-0000-0018-0000000000c3'),
        '00000000-0000-0000-0018-000000000056', '00000000-0000-0000-0018-000000000c31', 'A plan, again.',
        '00000000-0000-0000-0018-0000000000b3');

DO $chk$
DECLARE
    label text;
    stmt  text;
BEGIN
    FOR label, stmt IN VALUES
        ('a question names no sources',
         $q$INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id)
            VALUES ('00000000-0000-0000-0018-000000000c31', '00000000-0000-0000-0018-000000000041', 1,
                    '00000000-0000-0000-0026-0000000000e9', '00000000-0000-0000-0026-0000000000f1')$q$),
        ('an answer that said nothing of its sources names none',
         $q$INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id)
            VALUES ('00000000-0000-0000-0029-000000000c33', '00000000-0000-0000-0018-000000000041', 1,
                    '00000000-0000-0000-0026-0000000000e9', '00000000-0000-0000-0026-0000000000f1')$q$),
        ('only an answer says what it relied on',
         $q$INSERT INTO conversation_message (conversation_id, course_id, seq, author_member_id, body, created_by_action_id,
                                              sources_stated)
            VALUES ('00000000-0000-0000-0018-0000000000c3', '00000000-0000-0000-0018-000000000041', 99,
                    '00000000-0000-0000-0018-000000000053', 'And a plan?', '00000000-0000-0000-0018-0000000000b3', true)$q$),
        ('a purged version is relied on by nothing',
         $q$INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id)
            VALUES ('00000000-0000-0000-0029-000000000c32', '00000000-0000-0000-0018-000000000041', 3,
                    '00000000-0000-0000-0027-0000000000e1', '00000000-0000-0000-0027-0000000000f2')$q$),
        ('a student''s work is no source',
         $q$INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id)
            VALUES ('00000000-0000-0000-0029-000000000c32', '00000000-0000-0000-0018-000000000041', 3,
                    '00000000-0000-0000-0026-0000000000e8', '00000000-0000-0000-0026-0000000000f2')$q$),
        ('a source is written with its answer',
         $q$INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id, created_at)
            VALUES ('00000000-0000-0000-0029-000000000c32', '00000000-0000-0000-0018-000000000041', 3,
                    '00000000-0000-0000-0026-0000000000e9', '00000000-0000-0000-0026-0000000000f1', now() + interval '1 minute')$q$),
        ('a file is its source''s version''s',
         $q$INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id, file_id, file_version_id)
            VALUES ('00000000-0000-0000-0029-000000000c32', '00000000-0000-0000-0018-000000000041', 3,
                    '00000000-0000-0000-0026-0000000000e9', '00000000-0000-0000-0026-0000000000f1',
                    '00000000-0000-0000-0027-0000000000d1', '00000000-0000-0000-0026-0000000000f1')$q$),
        ('a page or a slide, not both',
         $q$INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id, file_id, file_version_id,
                                                     page, slide)
            VALUES ('00000000-0000-0000-0029-000000000c32', '00000000-0000-0000-0018-000000000041', 3,
                    '00000000-0000-0000-0026-0000000000e9', '00000000-0000-0000-0026-0000000000f1',
                    '00000000-0000-0000-0026-0000000000d1', '00000000-0000-0000-0026-0000000000f1', 1, 1)$q$),
        ('at most 20 to an answer',
         $q$INSERT INTO conversation_message_source (message_id, course_id, position, document_id, version_id)
            VALUES ('00000000-0000-0000-0029-000000000c32', '00000000-0000-0000-0018-000000000041', 21,
                    '00000000-0000-0000-0026-0000000000e9', '00000000-0000-0000-0026-0000000000f1')$q$),
        ('a source is kept as it was written',
         $q$UPDATE conversation_message_source SET page = 4 WHERE message_id = '00000000-0000-0000-0029-000000000c32'$q$),
        ('and names its file until the file goes',
         $q$UPDATE conversation_message_source SET file_id = NULL, file_version_id = NULL
            WHERE message_id = '00000000-0000-0000-0029-000000000c32'$q$),
        ('and is never deleted',
         $q$DELETE FROM conversation_message_source WHERE message_id = '00000000-0000-0000-0029-000000000c32'$q$)
    LOOP
        BEGIN
            EXECUTE stmt;
            RAISE EXCEPTION 'FAIL  0029 up: % — it was taken', label;
        EXCEPTION WHEN check_violation OR foreign_key_violation OR restrict_violation THEN
            NULL;
        END;
    END LOOP;
END $chk$;

-- Week 4's version is purged: its files go, and its source names none.
UPDATE document_version SET body_md = NULL, purged_at = now(), purged_by_actor_id = '00000000-0000-0000-0018-000000000031',
                            purge_reason = 'The slides named a patient.'
WHERE id = '00000000-0000-0000-0027-0000000000f1';
DO $chk$
DECLARE
    got text;
BEGIN
    SELECT string_agg(position || ':' || version_id || ':' || coalesce(file_id::text, '-') || ':'
                      || coalesce(file_version_id::text, '-') || ':' || coalesce(page::text, '-'), ' ' ORDER BY position) INTO got
    FROM conversation_message_source WHERE message_id = '00000000-0000-0000-0029-000000000c32';
    IF got IS DISTINCT FROM '1:00000000-0000-0000-0027-0000000000f1:-:-:3 2:00000000-0000-0000-0026-0000000000f1:-:-:-' THEN
        RAISE EXCEPTION 'FAIL  0029 up: after the purge the sources are %', coalesce(got, 'none');
    END IF;
END $chk$;
ROLLBACK;
\echo 'PASS  0029 up keeps an answer''s sources, held to its course and its versions'' files, and clears a purged file'

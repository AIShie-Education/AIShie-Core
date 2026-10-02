-- AIshie Core — after 0027_drop_deprecated.down.sql, in `make db-test-sql`
--
-- What the release before reads is back. Each version's own columns name
-- its first file: Week 5's handout, Week 4's slides; a version of text
-- alone names none, and neither does Week 4's purged version, which no
-- longer says what its file was. Each runtime agent's site chat credential
-- names the runtime token it holds that is not revoked: the coach's second,
-- and the tutor's of tests/up/0025; an mcp agent's names none. And the
-- rules the release before relies on hold again: a version it writes with
-- its file in its own columns alone is given it as its one file, named
-- after its document, and an agent it registers naming no hosting is an
-- mcp agent.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
DECLARE
    got text;
BEGIN
    SELECT string_agg(right(id::text, 2) || '=' || coalesce(storage_key, '-') || ':' || coalesce(content_type, '-') || ':'
                      || coalesce(byte_size::text, '-') || ':' || coalesce(checksum, '-'), ' ' ORDER BY id) INTO got
    FROM document_version WHERE id::text LIKE '00000000-0000-0000-0027-%';
    IF got IS DISTINCT FROM 'f1=documents/up27/d1:application/pdf:100:sha256:27d1 f2=-:-:-:-'
                            || ' f3=documents/down27/d4:application/vnd.openxmlformats-officedocument.wordprocessingml.document:200:sha256:27d4'
                            || ' f4=-:-:-:-' THEN
        RAISE EXCEPTION 'FAIL  0027 down: the versions'' own columns are %', got;
    END IF;
    SELECT string_agg(right(id::text, 2) || '=' || coalesce(right(site_chat_credential_id::text, 2), '-'), ' ' ORDER BY id) INTO got
    FROM actor WHERE id IN ('00000000-0000-0000-0025-000000000035', '00000000-0000-0000-0025-000000000038',
                            '00000000-0000-0000-0027-00000000003a', '00000000-0000-0000-0027-00000000003b');
    IF got IS DISTINCT FROM '35=c1 38=- 3a=c2 3b=-' THEN
        RAISE EXCEPTION 'FAIL  0027 down: the site chat credentials are %', got;
    END IF;
    IF (SELECT count(*) FROM pg_proc WHERE proname IN ('document_file_name', 'document_version_text_one_file_at_a_time',
                                                       'actor_hosting_by_default')) <> 3
       OR (SELECT count(*) FROM pg_trigger WHERE tgname IN ('document_version_text_one_file_at_a_time', 'actor_hosting_default')) <> 2
       OR (SELECT count(*) FROM pg_constraint WHERE conname IN ('actor_site_chat_is_agent', 'actor_site_chat_credential_fk',
                                                                'credential_id_actor_key',
                                                                'document_version_has_content', 'document_version_file_described',
                                                                'document_version_storage_key_key', 'document_version_purged_empty')) <> 7 THEN
        RAISE EXCEPTION 'FAIL  0027 down: what the release before relies on is not all back';
    END IF;
    IF (SELECT count(*) FROM document_version_file WHERE version_id = '00000000-0000-0000-0027-0000000000f3') <> 2 THEN
        RAISE EXCEPTION 'FAIL  0027 down: Week 5 lost a file';
    END IF;
END $chk$;

-- As the release before writes them.
BEGIN;
INSERT INTO document_version (id, document_id, seq, storage_key, content_type, byte_size, author_member_id)
VALUES ('00000000-0000-0000-0027-0000000000f5', '00000000-0000-0000-0027-0000000000e2', 3, 'courses/down27/f5', 'application/pdf',
        10, '00000000-0000-0000-0018-000000000051');
INSERT INTO actor (id, kind, display_name, owner_actor_id, created_by_actor_id)
VALUES ('00000000-0000-0000-0027-00000000003c', 'agent', 'Old style', '00000000-0000-0000-0025-000000000031',
        '00000000-0000-0000-0025-000000000031');
SET CONSTRAINTS ALL IMMEDIATE;
DO $chk$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM document_version_file WHERE version_id = '00000000-0000-0000-0027-0000000000f5' AND position = 1
                   AND filename = 'Week 5.pdf' AND storage_key = 'courses/down27/f5')
       OR NOT EXISTS (SELECT 1 FROM actor WHERE id = '00000000-0000-0000-0027-00000000003c' AND hosting = 'mcp') THEN
        RAISE EXCEPTION 'FAIL  0027 down: what the release before writes is not taken as it was';
    END IF;
END $chk$;
ROLLBACK;
\echo 'PASS  0027 down puts back each version''s first file in its own columns and each runtime agent''s site chat credential, and the rules the release before relies on'

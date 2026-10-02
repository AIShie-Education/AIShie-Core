-- AIshie Core — before 0027_drop_deprecated.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with, written as this release writes
-- it: Week 5, a version of a handout and a program with text beside them,
-- and one of text alone; Mei's coach, a runtime agent, whose runtime holds
-- its second token, the first revoked; and her editor, an mcp agent with a
-- token of hers. Committed, so that the down migration runs over it.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

-- e2 Week 5 · f3 its version of d4 and d5 · f4 its version of text alone
INSERT INTO document (id, course_id, kind, title, status)
VALUES ('00000000-0000-0000-0027-0000000000e2', '00000000-0000-0000-0018-000000000041', 'material', 'Week 5', 'active');
INSERT INTO document_version (id, document_id, seq, body_md, author_member_id, created_at) VALUES
    ('00000000-0000-0000-0027-0000000000f3', '00000000-0000-0000-0027-0000000000e2', 1, 'Read the handout first.',
     '00000000-0000-0000-0018-000000000051', '2026-10-01 09:00:00+00'),
    ('00000000-0000-0000-0027-0000000000f4', '00000000-0000-0000-0027-0000000000e2', 2, 'The handout, in a few words.',
     '00000000-0000-0000-0018-000000000051', '2026-10-01 10:00:00+00');
INSERT INTO document_version_file (id, version_id, document_id, position, filename, storage_key, content_type, byte_size,
                                   checksum, created_at) VALUES
    ('00000000-0000-0000-0027-0000000000d4', '00000000-0000-0000-0027-0000000000f3', '00000000-0000-0000-0027-0000000000e2', 1,
     'handout.docx', 'documents/down27/d4', 'application/vnd.openxmlformats-officedocument.wordprocessingml.document', 200,
     'sha256:27d4', '2026-10-01 09:00:00+00'),
    ('00000000-0000-0000-0027-0000000000d5', '00000000-0000-0000-0027-0000000000f3', '00000000-0000-0000-0027-0000000000e2', 2,
     'loops.py', 'documents/down27/d5', 'text/x-python', 30, NULL, '2026-10-01 09:00:00+00');

-- 3a Mei's coach, a runtime agent · 3b her editor, an mcp agent
-- · c1 the coach's first runtime token, revoked · c2 its second · c3 the editor's token
INSERT INTO actor (id, kind, display_name, owner_actor_id, created_by_actor_id, hosting) VALUES
    ('00000000-0000-0000-0027-00000000003a', 'agent', 'Coach', '00000000-0000-0000-0025-000000000031',
     '00000000-0000-0000-0025-000000000031', 'runtime'),
    ('00000000-0000-0000-0027-00000000003b', 'agent', 'Editor', '00000000-0000-0000-0025-000000000031',
     '00000000-0000-0000-0025-000000000031', 'mcp');
INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, label, issued_by_actor_id, issued_to_service, revoked_at) VALUES
    ('00000000-0000-0000-0027-0000000000c1', '00000000-0000-0000-0027-00000000003a', 'api_token', 'h', 'down27c1', 'agent runtime',
     NULL, 'agent_runtime', '2026-09-30 00:00:00+00'),
    ('00000000-0000-0000-0027-0000000000c2', '00000000-0000-0000-0027-00000000003a', 'api_token', 'h', 'down27c2', 'agent runtime',
     NULL, 'agent_runtime', NULL),
    ('00000000-0000-0000-0027-0000000000c3', '00000000-0000-0000-0027-00000000003b', 'api_token', 'h', 'down27c3', 'editor',
     '00000000-0000-0000-0025-000000000031', NULL, NULL);

COMMIT;

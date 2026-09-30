-- AIshie Core — before 0020_document_text.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with: the transcription service, its
-- credential live and one revoked, working on Week 1's draft; Week 1's
-- published version transcribed; the rubric's text edited by Lin. Committed,
-- so that the down migration runs over it.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

-- 3a the service · 1c1 its credential · 1c2 one revoked
INSERT INTO actor (id, kind, display_name, service_scope, created_by_actor_id)
VALUES ('00000000-0000-0000-0020-00000000003a', 'service', 'Transcription', 'document_text', '00000000-0000-0000-0018-000000000031');
INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, label, issued_by_actor_id, revoked_at) VALUES
    ('00000000-0000-0000-0020-0000000001c1', '00000000-0000-0000-0020-00000000003a', 'service', 'h', 'down20svc001', 'runtime',
     '00000000-0000-0000-0018-000000000031', NULL),
    ('00000000-0000-0000-0020-0000000001c2', '00000000-0000-0000-0020-00000000003a', 'service', 'h', 'down20svc002', 'old runtime',
     '00000000-0000-0000-0018-000000000031', '2026-09-01 00:00:00+00');
UPDATE document_version_text
SET status = 'working', lease_id = gen_random_uuid(), claimed_until = now() + interval '10 minutes',
    claimed_by_credential_id = '00000000-0000-0000-0020-0000000001c1', claimed_at = now(), attempts = 1
WHERE version_id = '00000000-0000-0000-0020-0000000000f3';
UPDATE document_version_text
SET status = 'done', body = '## Page 1', source = 'ai', model = 'A model', produced_at = now(), pages = 1, revision = 2
WHERE version_id = '00000000-0000-0000-0020-0000000000f2';
UPDATE document_version_text
SET status = 'done', body = '## Page 1', source = 'staff', edited_by_member_id = '00000000-0000-0000-0018-000000000051',
    edited_at = now(), revision = 2
WHERE version_id = '00000000-0000-0000-0020-0000000000f4';
-- b1 the service's action, which names it
INSERT INTO action (id, actor_id, course_id, action_type, target_type, target_id, payload, payload_hash,
                    idempotency_key, authz_result, status, executed_at)
VALUES ('00000000-0000-0000-0020-0000000000b1', '00000000-0000-0000-0020-00000000003a', '00000000-0000-0000-0018-000000000041',
        'document_text.complete', 'document_version', '00000000-0000-0000-0020-0000000000f2', '{}', repeat('0', 64),
        'down20-b1', 'autonomous', 'executed', now());

COMMIT;

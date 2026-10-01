-- AIshie Core — before 0026_file_renditions.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with: the renditions of
-- tests/up/0026, Week 2's slides done, their PDF in the file store, and
-- Wei's essay claimed by a credential. Committed, so that the down
-- migration runs over it.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

-- c1 a credential the claim was made with
INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, label)
VALUES ('00000000-0000-0000-0026-0000000000c1', '00000000-0000-0000-0018-000000000036', 'api_token', 'h', 'down26c1', 'claims');
UPDATE file_rendition
SET status = 'done', storage_key = 'renditions/down26/d1', byte_size = 1234, checksum = 'sha256:26', page_count = 12,
    produced_at = now(), attempts = 1
WHERE file_id = '00000000-0000-0000-0026-0000000000d1';
UPDATE file_rendition
SET status = 'claimed', lease_id = gen_random_uuid(), claimed_until = now() + interval '10 minutes',
    claimed_by_credential_id = '00000000-0000-0000-0026-0000000000c1', claimed_at = now(), attempts = 1
WHERE file_id = '00000000-0000-0000-0026-0000000000d4';

COMMIT;

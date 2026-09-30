-- AIshie Core — before 0022_sso_providers.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with: two providers Lin set up, one
-- switched on, and Ho's identity linked at it, and another of Wei's linked at
-- the operator's provider. Committed, so that the down migration runs over
-- it.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

INSERT INTO sso_provider (id, display_name, issuer, client_id, client_secret_sealed, client_secret_hint, subject_claim,
                          enabled, position, created_by_actor_id, updated_by_actor_id) VALUES
    ('hainanu-cas', 'HAINANU CAS', 'https://cas.hainanu.example/oidc', 'aishie', 'v1.0123456789abcdef.' || repeat('A', 60),
     '…abcd', 'sub', true, 1, '00000000-0000-0000-0018-000000000031', '00000000-0000-0000-0018-000000000031'),
    ('google', 'Google', 'https://accounts.google.com', 'aishie.apps', 'v1.0123456789abcdef.' || repeat('B', 60),
     '…', 'sub', false, 2, '00000000-0000-0000-0018-000000000031', '00000000-0000-0000-0018-000000000032');
-- 1c1 Ho at hainanu-cas · 1c2 Wei at the operator's provider
INSERT INTO credential (id, actor_id, kind, provider, subject) VALUES
    ('00000000-0000-0000-0023-0000000001c1', '00000000-0000-0000-0018-000000000032', 'sso', 'hainanu-cas', '20230007'),
    ('00000000-0000-0000-0023-0000000001c2', '00000000-0000-0000-0018-000000000033', 'sso', 'polyu-adfs', 'wei@polyu.edu.hk');

COMMIT;

-- AIshie Core — after 0020_document_text.down.sql, in `make db-test-sql`
--
-- The text versions are gone, and the versions they were of stay. The
-- service stays, as its action names it, as an agent nobody owns,
-- suspended, its credentials revoked API tokens: the one revoked before
-- keeps its date.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'document_version_text') THEN
        RAISE EXCEPTION 'FAIL  0020 down: the text versions are still there';
    END IF;
    IF (SELECT count(*) FROM document_version WHERE id::text LIKE '00000000-0000-0000-0020-%') <> 10 THEN
        RAISE EXCEPTION 'FAIL  0020 down: a version went with its text';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM actor WHERE id = '00000000-0000-0000-0020-00000000003a'
                   AND kind = 'agent' AND status = 'suspended' AND owner_actor_id IS NULL)
       OR NOT EXISTS (SELECT 1 FROM action WHERE id = '00000000-0000-0000-0020-0000000000b1') THEN
        RAISE EXCEPTION 'FAIL  0020 down: the service is not an agent, suspended, and still named by its action';
    END IF;
    IF EXISTS (SELECT 1 FROM credential WHERE actor_id = '00000000-0000-0000-0020-00000000003a'
               AND (kind <> 'api_token' OR revoked_at IS NULL))
       OR NOT EXISTS (SELECT 1 FROM credential WHERE id = '00000000-0000-0000-0020-0000000001c2'
                      AND revoked_at = '2026-09-01 00:00:00+00') THEN
        RAISE EXCEPTION 'FAIL  0020 down: the service''s credentials are not revoked API tokens as they were revoked';
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'actor' AND column_name = 'service_scope') THEN
        RAISE EXCEPTION 'FAIL  0020 down: actor still has service_scope';
    END IF;
END $chk$;
\echo 'PASS  0020 down with text versions and a service, which becomes a suspended agent whose credentials are revoked'

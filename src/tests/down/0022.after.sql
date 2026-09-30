-- AIshie Core — after 0022_sso_providers.down.sql, in `make db-test-sql`
--
-- The providers the site set up are gone, and nothing else: the identities
-- linked at them stay as they were, naming them, and so does the one linked
-- at the operator's provider, which the release before offers alone.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'sso_provider') THEN
        RAISE EXCEPTION 'FAIL  0022 down: the providers are still there';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'sso_provider_id_fixed') THEN
        RAISE EXCEPTION 'FAIL  0022 down: the providers'' trigger function is still there';
    END IF;
    IF (SELECT count(*) FROM credential
        WHERE id IN ('00000000-0000-0000-0023-0000000001c1', '00000000-0000-0000-0023-0000000001c2') AND revoked_at IS NULL) <> 2 THEN
        RAISE EXCEPTION 'FAIL  0022 down: an identity linked at a provider is gone or revoked';
    END IF;
END $chk$;
\echo 'PASS  0022 down with providers set up, which go, and identities linked at them, which stay'

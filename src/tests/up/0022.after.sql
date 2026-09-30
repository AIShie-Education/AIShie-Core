-- AIshie Core — after 0022_sso_providers.up.sql, in `make db-test-sql`
--
-- No provider is set up by the migration: the table is new and empty, and
-- whoever signed in through the operator's provider before still has their
-- identity, as it was. A provider's id is fixed from the start.

\set ON_ERROR_STOP 1
\set QUIET 1
DO $chk$
BEGIN
    IF EXISTS (SELECT 1 FROM sso_provider) THEN
        RAISE EXCEPTION 'FAIL  0022 up: a provider was set up';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'sso_provider_id_fixed') THEN
        RAISE EXCEPTION 'FAIL  0022 up: a provider''s id may change';
    END IF;
    IF (SELECT count(*) FROM credential WHERE kind = 'sso') <> (SELECT count(*) FROM credential WHERE kind = 'sso' AND provider IS NOT NULL) THEN
        RAISE EXCEPTION 'FAIL  0022 up: an identity lost its provider';
    END IF;
END $chk$;
\echo 'PASS  0022 up sets up no provider, leaves every identity as it was, and fixes a provider''s id'

-- AIshie Core — migration 0022 (down)
-- Reverts 0022_sso_providers.up.sql.
--
-- Lost: every identity provider the site's administrators set up, with its
-- sealed client secret. Nobody signs in through one of them any more; the
-- operator's provider (OIDC_ISSUER), in the environment, is untouched, and
-- is the one the release before offers. The identities linked at a provider
-- that goes stay in credential, as they were, naming it: an administrator
-- who sets it up again under the same id finds them there, and so does a
-- sign-in through it. The action log keeps what was done to each provider,
-- without its secret, which was never recorded.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TABLE IF EXISTS sso_provider;
DROP FUNCTION IF EXISTS sso_provider_id_fixed();

COMMIT;

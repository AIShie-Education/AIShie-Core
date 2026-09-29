-- AIshiteru Core — migration 0017 (down)
-- Reverts 0017_api_tokens_for_agents.up.sql.
--
-- Lost: only the refusal. The release before this one issues a person an
-- API token again when asked, and gives an agent a password, an invitation
-- or an identity at a provider. Kept: every credential as it stands, the
-- ones the up revoked among them, which stay revoked: nothing says which of
-- them anyone still wants, and whoever does is issued a new one.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TRIGGER IF EXISTS credential_fits_actor_kind ON credential;
DROP FUNCTION IF EXISTS credential_check_actor_kind();

COMMIT;

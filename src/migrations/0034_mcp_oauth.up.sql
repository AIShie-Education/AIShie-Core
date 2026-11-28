-- AIshie Core — migration 0034 (up)
-- Agent-actor OAuth 2.0 Client Credentials for MCP, PostgreSQL 13+.
--
-- These are machine credentials, not human consent or delegated authorization.
-- Every client belongs to one agent. Secrets and opaque access tokens are kept
-- only as SHA-256 hashes; there is no refresh token or registration endpoint.
-- The previous release knows neither table and is unaffected by a rollback.

BEGIN;

SET LOCAL lock_timeout = '10s';

CREATE TABLE oauth_client (
    id                  uuid        PRIMARY KEY,
    agent_actor_id      uuid        NOT NULL REFERENCES actor (id) ON DELETE CASCADE,
    label               text        NOT NULL,
    secret_hash         text        NOT NULL,
    scopes              text[]      NOT NULL DEFAULT ARRAY['mcp.read'],
    created_by_actor_id uuid        REFERENCES actor (id) ON DELETE SET NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    rotated_at          timestamptz,
    revoked_at          timestamptz,
    CONSTRAINT oauth_client_label_valid CHECK (btrim(label) <> '' AND char_length(label) <= 200),
    CONSTRAINT oauth_client_secret_hashed CHECK (left(secret_hash, 7) = 'sha256:' AND char_length(secret_hash) = 71),
    CONSTRAINT oauth_client_scopes_valid CHECK (
        cardinality(scopes) > 0 AND scopes <@ ARRAY['mcp.read', 'mcp.write']::text[]
    )
);

CREATE INDEX oauth_client_agent_idx ON oauth_client (agent_actor_id, created_at DESC, id);

CREATE TABLE oauth_access_token (
    id              uuid        PRIMARY KEY,
    client_id       uuid        NOT NULL REFERENCES oauth_client (id) ON DELETE CASCADE,
    agent_actor_id  uuid        NOT NULL REFERENCES actor (id) ON DELETE CASCADE,
    token_hash      text        NOT NULL UNIQUE,
    scopes          text[]      NOT NULL,
    resource        text        NOT NULL DEFAULT '/mcp',
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    revoked_at      timestamptz,
    last_used_at    timestamptz,
    CONSTRAINT oauth_access_token_hashed CHECK (left(token_hash, 7) = 'sha256:' AND char_length(token_hash) = 71),
    CONSTRAINT oauth_access_token_scopes_valid CHECK (
        cardinality(scopes) > 0 AND scopes <@ ARRAY['mcp.read', 'mcp.write']::text[]
    ),
    CONSTRAINT oauth_access_token_resource_mcp CHECK (resource = '/mcp'),
    CONSTRAINT oauth_access_token_lifetime_valid CHECK (
        expires_at > created_at AND expires_at <= created_at + interval '10 minutes'
    )
);

CREATE INDEX oauth_access_token_client_idx ON oauth_access_token (client_id) WHERE revoked_at IS NULL;
CREATE INDEX oauth_access_token_agent_idx ON oauth_access_token (agent_actor_id, created_at DESC, id);
CREATE INDEX oauth_access_token_expiry_idx ON oauth_access_token (expires_at) WHERE revoked_at IS NULL;

COMMIT;

-- AIshie Core — migration 0022 (up)
-- Identity providers administrators set up from the front end.
-- Design reference: docs/schema.md §2.1 (Single sign-on). PostgreSQL 13+.
--
-- Single sign-on was one OpenID Connect provider, which the server's
-- operator sets in its environment (OIDC_ISSUER and the rest). Now the
-- site's administrators — root and the platform's administrators — add
-- providers of their own, and change, switch off and remove them, with the
-- sso.* tools; each is a row here, read at every sign-in, so that a change
-- takes effect on every instance without a restart. The operator's provider
-- stays where it is, in the environment, read-only to administrators, and
-- wins over a row with its name.
--
-- A provider is known by its id, a short name of lower-case letters, digits
-- and hyphens: what credential.provider records for an identity linked at it
-- (actor.link_sso), and what a sign-in starts with. It is the table's key,
-- as the name credential.provider holds, and it never changes: a provider
-- renamed would leave the identities linked at it behind. No foreign key
-- joins the two: the operator's provider has no row, an identity may be
-- linked before its provider is set up, and one linked at a provider since
-- removed is kept, revoked, for the record.
--
-- Its client secret is kept sealed (package secrets: AES-256-GCM under the
-- server's SECRETS_KEY, bound to the provider's id), never in the clear:
-- client_secret_sealed is the envelope, which names the key that sealed it,
-- and client_secret_hint is what may be shown of the secret, its last four
-- characters at most. version counts the row's changes, for a write made
-- over the version its writer read (If-Match); who last changed it, and
-- when, are kept beside it, and the action log keeps every change.
--
-- The previous release keeps working while this goes in. It reads no row
-- here: it offers the operator's provider alone, and signs in only through
-- it, as before.

BEGIN;

SET LOCAL lock_timeout = '10s';

-- scopes always include openid; subject_claim is the claim an account is
-- known by (credential.subject), sub unless the provider says otherwise
-- (ADFS: upn). email_claim names the claim holding the person's email, read
-- only to link an identity to the account with that email when the
-- provider vouches for it (link_by_email, off by default), and only within
-- allowed_email_domains. position orders the providers on the sign-in page.
CREATE TABLE sso_provider (
    id                    text        PRIMARY KEY,
    display_name          text        NOT NULL,
    issuer                text        NOT NULL,
    client_id             text        NOT NULL,
    client_secret_sealed  text        NOT NULL,
    client_secret_hint    text        NOT NULL,
    scopes                text[]      NOT NULL DEFAULT '{openid,profile,email}',
    subject_claim         text        NOT NULL DEFAULT 'sub',
    email_claim           text,
    allowed_email_domains text[]      NOT NULL DEFAULT '{}',
    link_by_email         boolean     NOT NULL DEFAULT false,
    enabled               boolean     NOT NULL DEFAULT false,
    position              integer     NOT NULL DEFAULT 0,
    version               integer     NOT NULL DEFAULT 1,
    created_by_actor_id   uuid        NOT NULL REFERENCES actor (id),
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_by_actor_id   uuid        NOT NULL REFERENCES actor (id),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT sso_provider_id_valid           CHECK (id ~ '^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$'),
    -- The server holds a name to 64 characters; the database, whatever its
    -- encoding, to the bytes that many take at most.
    CONSTRAINT sso_provider_display_name_valid CHECK (
        display_name <> '' AND octet_length(display_name) <= 256 AND display_name = btrim(display_name)
        AND display_name !~ '[\x01-\x1f\x7f]'
    ),
    CONSTRAINT sso_provider_issuer_valid       CHECK (issuer ~ '^https?://[^\s]+$' AND octet_length(issuer) <= 500),
    CONSTRAINT sso_provider_client_id_valid    CHECK (
        client_id <> '' AND octet_length(client_id) <= 500 AND client_id = btrim(client_id)
    ),
    -- Sealed, never the secret itself: the envelope, v1, the id of the key
    -- that sealed it, and the nonce and ciphertext in base64url.
    CONSTRAINT sso_provider_secret_sealed      CHECK (client_secret_sealed ~ '^v[0-9]+\.[0-9a-f]{16}\.[A-Za-z0-9_-]{39,}$'),
    -- A client secret is printable ASCII (RFC 6749, VSCHAR), and its hint an
    -- ellipsis and its last four characters at most.
    CONSTRAINT sso_provider_secret_hint_valid  CHECK (client_secret_hint ~ '^…[ -~]{0,4}$'),
    CONSTRAINT sso_provider_scopes_valid       CHECK ('openid' = ANY (scopes) AND cardinality(scopes) <= 20),
    CONSTRAINT sso_provider_claims_valid       CHECK (
        subject_claim <> '' AND octet_length(subject_claim) <= 200
        AND (email_claim IS NULL OR (email_claim <> '' AND octet_length(email_claim) <= 200))
    ),
    CONSTRAINT sso_provider_domains_valid      CHECK (
        cardinality(allowed_email_domains) <= 50 AND allowed_email_domains::text = lower(allowed_email_domains::text)
    ),
    CONSTRAINT sso_provider_link_by_email      CHECK (
        NOT link_by_email OR (email_claim IS NOT NULL AND cardinality(allowed_email_domains) > 0)
    ),
    CONSTRAINT sso_provider_version_valid      CHECK (version >= 1)
);

-- The id is what identities linked at the provider name: it never changes.
CREATE FUNCTION sso_provider_id_fixed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id THEN
        RAISE EXCEPTION 'sso_provider %: a provider''s id never changes; the identities linked at it name it', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER sso_provider_id_fixed
    BEFORE UPDATE OF id ON sso_provider
    FOR EACH ROW EXECUTE FUNCTION sso_provider_id_fixed();

COMMIT;

-- AIshie Core — migration 0020 (up)
-- Text versions of documents, and the site service that writes them.
-- Design reference: docs/schema.md §2.1 (Services), §2.4 (Text versions).
-- PostgreSQL 13+.
--
-- A version of a course's material, instructions or rubric that has a file
-- has a text version: the file transcribed into Markdown by a model the site
-- chooses, which every reader of the version may read, and its staff may
-- correct. A program outside Core writes it: the runtime's transcriber,
-- which takes versions waiting for their text from a queue, reads their
-- files and writes the text back. It is a site service, not a member of any
-- course, and it is given an identity of its own here: an actor of kind
-- 'service', whose service_scope says what it is for and so which tools it
-- may call, and which calls nothing else. It holds credentials of kind
-- 'service' and nothing else, and it is seated nowhere. Nothing grants on
-- kind; the tools gate on service_scope, as the platform tools gate on
-- platform_role. The refusals below read kind, as the refusals of ownership
-- do.
--
-- The text version is recorded pending in the transaction that adds the
-- version, by the database, whichever release adds it, and is deleted with
-- its file when the version is purged: it is what the file said, as text.
--
-- What is there already is queued once, here, behind every upload that
-- comes after it (backfill): the published version and the latest version
-- of every course document that is not archived, in every course that is not
-- archived. Earlier versions, and those of archived documents and courses,
-- are not transcribed unless someone asks for them to be.
--
-- The previous release keeps working while this goes in. It adds versions,
-- which are queued as any are, and purges them, which deletes their text;
-- it knows no service and reads no text version, and the service's
-- credentials, under a scheme of their own, are not tokens to it.

BEGIN;

-- Waits for a busy table rather than queueing every call behind it; a
-- deploy that times out here is run again.
SET LOCAL lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- Services: actors that are programs of the site's, each for one thing
-- ---------------------------------------------------------------------------

-- A service has no email, no login ID, no platform role, no owner and no
-- site chat: it signs in nowhere and answers nobody. One service actor for
-- each scope; its credentials are issued, replaced and revoked by the
-- platform's administrators.
ALTER TABLE actor ADD COLUMN service_scope text;
ALTER TABLE actor
    DROP CONSTRAINT actor_kind_valid,
    ADD CONSTRAINT actor_kind_valid CHECK (kind IN ('human', 'agent', 'system', 'service')),
    ADD CONSTRAINT actor_service_scope_valid CHECK (service_scope IS NULL OR service_scope IN ('document_text')),
    ADD CONSTRAINT actor_service_is_scoped CHECK ((kind = 'service') = (service_scope IS NOT NULL)),
    ADD CONSTRAINT actor_service_holds_no_account CHECK (kind <> 'service' OR (email IS NULL AND platform_role IS NULL));
CREATE UNIQUE INDEX actor_service_scope_key ON actor (service_scope) WHERE service_scope IS NOT NULL;

-- A service credential is a bearer token like an API token, found by its
-- prefix and checked against its hash, under a scheme of its own.
ALTER TABLE credential
    DROP CONSTRAINT credential_kind_valid,
    DROP CONSTRAINT credential_token_lookup,
    ADD CONSTRAINT credential_kind_valid
        CHECK (kind IN ('password', 'sso', 'api_token', 'session', 'invite', 'service')),
    ADD CONSTRAINT credential_token_lookup
        CHECK (kind NOT IN ('api_token', 'session', 'invite', 'service') OR token_prefix IS NOT NULL);

-- 0017's rule, and a service's: it holds service credentials and nothing
-- else, and nobody else holds one.
CREATE OR REPLACE FUNCTION credential_check_actor_kind() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    holder text;
BEGIN
    -- A revocation, or anything else done to a row that stays revoked,
    -- opens nothing.
    IF TG_OP = 'UPDATE' AND NEW.revoked_at IS NOT NULL THEN
        RETURN NEW;
    END IF;
    SELECT kind INTO holder FROM actor WHERE id = NEW.actor_id;
    IF holder = 'human' AND NEW.kind = 'api_token' THEN
        RAISE EXCEPTION 'actor % is a person, who holds no API token: API tokens are for agents', NEW.actor_id
            USING ERRCODE = 'check_violation';
    END IF;
    IF holder = 'agent' AND NEW.kind IN ('password', 'invite', 'sso', 'session') THEN
        RAISE EXCEPTION 'actor % is an agent, which holds API tokens only and never signs in: no % credential', NEW.actor_id, NEW.kind
            USING ERRCODE = 'check_violation';
    END IF;
    IF (holder = 'service') <> (NEW.kind = 'service') THEN
        RAISE EXCEPTION 'a service holds service credentials only, and only a service holds one: actor % is a %, the credential a %',
            NEW.actor_id, holder, NEW.kind
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

-- A service is seated in no course: it is outside them all, and its tools
-- take no seat.
CREATE FUNCTION course_member_reject_service() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM actor WHERE id = NEW.actor_id AND kind = 'service') THEN
        RAISE EXCEPTION 'actor % is a service, which is seated in no course', NEW.actor_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER course_member_not_a_service
    BEFORE INSERT OR UPDATE OF actor_id ON course_member
    FOR EACH ROW EXECUTE FUNCTION course_member_reject_service();

-- ---------------------------------------------------------------------------
-- Text versions
-- ---------------------------------------------------------------------------

-- Target of document_version_text's course key.
ALTER TABLE document ADD CONSTRAINT document_id_course_key UNIQUE (id, course_id);

-- One per version with a file of a course's material, instructions or
-- rubric. status is where it stands: pending, waiting to be claimed;
-- working, claimed by the service until claimed_until (lease_id is the
-- claim's); done, with its text; failed or skipped, saying why (reason).
-- source says whose the text is: ai, the service's, which says what model
-- made it and when (model, produced_at); staff, a member's edit, which says
-- who and when (edited_by_member_id, edited_at) and which the service never
-- writes over. revision counts the changes to the text, so that an edit can
-- be made against the text it was made from, and a text read in parts is
-- read of one revision. attempts counts the claims since it was last queued;
-- backfill marks what was queued by this migration, which the queue takes
-- after everything else. claimed_by_credential_id and claimed_at are the
-- last claim's, kept after it ends.
CREATE TABLE document_version_text (
    version_id               uuid        PRIMARY KEY,
    document_id              uuid        NOT NULL,
    course_id                uuid        NOT NULL,
    status                   text        NOT NULL DEFAULT 'pending',
    body                     text,
    source                   text,
    pages                    integer,
    model                    text,
    reason                   text,
    revision                 integer     NOT NULL DEFAULT 1,
    attempts                 integer     NOT NULL DEFAULT 0,
    backfill                 boolean     NOT NULL DEFAULT false,
    queued_at                timestamptz NOT NULL DEFAULT now(),
    lease_id                 uuid,
    claimed_until            timestamptz,
    claimed_by_credential_id uuid        REFERENCES credential (id),
    claimed_at               timestamptz,
    produced_at              timestamptz,
    edited_by_member_id      uuid,
    edited_at                timestamptz,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (version_id, document_id) REFERENCES document_version (id, document_id),
    FOREIGN KEY (document_id, course_id) REFERENCES document (id, course_id),
    FOREIGN KEY (course_id, edited_by_member_id) REFERENCES course_member (course_id, id),
    CONSTRAINT document_version_text_status_valid
        CHECK (status IN ('pending', 'working', 'done', 'failed', 'skipped')),
    CONSTRAINT document_version_text_source_valid CHECK (source IS NULL OR source IN ('ai', 'staff')),
    -- There is a text exactly when it is done, and it says whose it is.
    CONSTRAINT document_version_text_done_has_body
        CHECK ((status = 'done') = (body IS NOT NULL) AND (body IS NULL) = (source IS NULL)),
    -- At most 2 MiB of it.
    CONSTRAINT document_version_text_body_size CHECK (body IS NULL OR octet_length(body) BETWEEN 1 AND 2097152),
    CONSTRAINT document_version_text_ai_made
        CHECK (source IS DISTINCT FROM 'ai' OR (model IS NOT NULL AND produced_at IS NOT NULL)),
    CONSTRAINT document_version_text_staff_edited
        CHECK ((source IS NOT DISTINCT FROM 'staff') = (edited_by_member_id IS NOT NULL)
               AND (edited_by_member_id IS NULL) = (edited_at IS NULL)),
    CONSTRAINT document_version_text_model_length CHECK (model IS NULL OR char_length(model) BETWEEN 1 AND 200),
    CONSTRAINT document_version_text_pages_valid CHECK (pages IS NULL OR pages BETWEEN 1 AND 100000),
    -- Failed and skipped say why; nothing else does.
    CONSTRAINT document_version_text_reason_given
        CHECK ((status IN ('failed', 'skipped')) = (reason IS NOT NULL)
               AND (reason IS NULL OR char_length(reason) BETWEEN 1 AND 500)),
    -- A claim holds a lease until it ends, by whichever credential made it.
    CONSTRAINT document_version_text_lease_held
        CHECK ((status = 'working') = (lease_id IS NOT NULL)
               AND (lease_id IS NULL) = (claimed_until IS NULL)
               AND (status <> 'working' OR (claimed_by_credential_id IS NOT NULL AND claimed_at IS NOT NULL))),
    CONSTRAINT document_version_text_counts_valid CHECK (revision >= 1 AND attempts >= 0)
);
-- The queue: what waits, uploads first and the backfill after them.
CREATE INDEX document_version_text_queue_idx ON document_version_text (backfill, queued_at)
    WHERE status IN ('pending', 'working');
CREATE INDEX document_version_text_document_idx ON document_version_text (document_id);
-- What a credential holds, released when it is revoked.
CREATE INDEX document_version_text_claimed_idx ON document_version_text (claimed_by_credential_id)
    WHERE status = 'working';

-- A text version is of a version with a file, of a course's material,
-- instructions or rubric, not purged; it stays the text of that version,
-- in its course, and is deleted only with its version's file.
CREATE FUNCTION document_version_text_guarded() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF NOT EXISTS (SELECT 1 FROM document_version WHERE id = OLD.version_id AND purged_at IS NOT NULL) THEN
            RAISE EXCEPTION 'the text version of % goes only when the version is purged', OLD.version_id
                USING ERRCODE = 'restrict_violation';
        END IF;
        RETURN OLD;
    END IF;
    IF TG_OP = 'UPDATE' THEN
        IF (NEW.version_id, NEW.document_id, NEW.course_id, NEW.created_at)
           IS DISTINCT FROM (OLD.version_id, OLD.document_id, OLD.course_id, OLD.created_at) THEN
            RAISE EXCEPTION 'a text version stays the text of its version, as it was made'
                USING ERRCODE = 'restrict_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM document_version v
                   JOIN document d ON d.id = v.document_id
                   WHERE v.id = NEW.version_id AND v.storage_key IS NOT NULL AND v.purged_at IS NULL
                     AND d.kind IN ('material', 'instructions', 'rubric')) THEN
        RAISE EXCEPTION 'version % has no file of a course''s material, instructions or rubric to transcribe', NEW.version_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER document_version_text_guarded
    BEFORE INSERT OR UPDATE OR DELETE ON document_version_text
    FOR EACH ROW EXECUTE FUNCTION document_version_text_guarded();

CREATE TRIGGER document_version_text_no_truncate
    BEFORE TRUNCATE ON document_version_text
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- Queued as the version is added, in its transaction, whichever release
-- adds it.
CREATE FUNCTION document_version_queue_text() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO document_version_text (version_id, document_id, course_id)
    SELECT NEW.id, d.id, d.course_id
    FROM document d
    WHERE d.id = NEW.document_id AND d.kind IN ('material', 'instructions', 'rubric');
    RETURN NULL;
END;
$$;

CREATE TRIGGER document_version_text_queued
    AFTER INSERT ON document_version
    FOR EACH ROW WHEN (NEW.storage_key IS NOT NULL)
    EXECUTE FUNCTION document_version_queue_text();

-- Deleted as the version is purged: the text is the file's, and goes with it.
CREATE FUNCTION document_version_forget_text() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM document_version_text WHERE version_id = NEW.id;
    RETURN NULL;
END;
$$;

CREATE TRIGGER document_version_text_purged
    AFTER UPDATE OF purged_at ON document_version
    FOR EACH ROW WHEN (OLD.purged_at IS NULL AND NEW.purged_at IS NOT NULL)
    EXECUTE FUNCTION document_version_forget_text();

-- The backfill: what is read now, queued behind every upload to come, the
-- newest first.
INSERT INTO document_version_text (version_id, document_id, course_id, backfill, queued_at)
SELECT v.id, d.id, d.course_id, true, v.created_at
FROM document d
JOIN course c ON c.id = d.course_id
JOIN document_version v ON v.document_id = d.id
WHERE d.kind IN ('material', 'instructions', 'rubric') AND d.status = 'active' AND d.purged_at IS NULL
  AND c.status <> 'archived'
  AND v.storage_key IS NOT NULL AND v.purged_at IS NULL
  AND (v.id = d.published_version_id
       OR v.seq = (SELECT max(w.seq) FROM document_version w WHERE w.document_id = d.id));

COMMIT;

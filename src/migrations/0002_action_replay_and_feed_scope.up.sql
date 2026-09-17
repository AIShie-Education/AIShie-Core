-- AIshiteru Core — migration 0002 (up)
-- What the tool layer needs from the database before it can be written:
-- replayable idempotency, a feed that can be scope-filtered in SQL, login
-- sessions, and the part of the action status table a CHECK can express.
--
-- Resolves three of the open questions in docs/schema.md §7. PostgreSQL 13+.

BEGIN;

-- ---------------------------------------------------------------------------
-- action: idempotent replay
-- ---------------------------------------------------------------------------

-- The key alone says "this call was seen before". The hash says whether it
-- was the same call. Same key and same hash replays the stored result; same
-- key and a different hash is a client bug and is refused, instead of telling
-- the caller that a request it never made succeeded.
--
-- Hex SHA-256 over the tool name and the canonical JSON of the arguments; the
-- rule ("ais-canon-1") lives in internal/canon. Only the server computes it.
ALTER TABLE action ADD COLUMN payload_hash text;

-- Rows from before this migration get a hash of their stored payload. It is
-- not the canonical hash, so a replay of such a key is refused rather than
-- answered. That is the safe direction, and new deployments have no such rows.
UPDATE action
   SET payload_hash = encode(sha256(convert_to(action_type || E'\n' || payload::text, 'UTF8')), 'hex')
 WHERE payload_hash IS NULL;

ALTER TABLE action
    ALTER COLUMN payload_hash SET NOT NULL,
    ADD CONSTRAINT action_payload_hash_valid CHECK (payload_hash ~ '^[0-9a-f]{64}$');

-- What the call returned, so a replay can return it again. Also holds
-- {"error": ...} for a failed or cancelled action and {"decision": ...} for a
-- rejected one. Secrets (a freshly issued token) are stripped before storing.
ALTER TABLE action ADD COLUMN result jsonb;

-- ---------------------------------------------------------------------------
-- action: the status table, as far as a CHECK can take it
-- ---------------------------------------------------------------------------

-- The transitions stay application logic. What can be said about a row at
-- rest is said here:
--   * denied authorization and denied status go together, both ways;
--   * only a confirm_required action is ever proposed, rejected or cancelled,
--     and only such an action has a decider;
--   * only a pending_review action that executed can be in a review state.
ALTER TABLE action ADD CONSTRAINT action_status_matches_authz CHECK (
        (authz_result = 'denied') = (status = 'denied')
    AND (status NOT IN ('proposed', 'rejected', 'cancelled') OR authz_result = 'confirm_required')
    AND (decided_by_member_id IS NULL OR authz_result = 'confirm_required')
    AND (review_state = 'none' OR (authz_result = 'pending_review' AND status = 'executed'))
);

ALTER TABLE action ADD CONSTRAINT action_executed_at_consistent CHECK (
    (status = 'executed') = (executed_at IS NOT NULL)
);

-- ---------------------------------------------------------------------------
-- event: whose it is, so the feed can be filtered in SQL
-- ---------------------------------------------------------------------------

-- A feed must never show a student, or a tutor listed for one student,
-- anything about another. Scope is checked against these two columns exactly
-- as authorize() checks a target: null means the event belongs to no student
-- (or no assignment) and scope does not apply. Filled when the event is
-- written and, event being append-only, never changed.
ALTER TABLE event
    ADD COLUMN student_member_id uuid REFERENCES course_member (id),
    ADD COLUMN assignment_id     uuid REFERENCES assignment (id);

-- ---------------------------------------------------------------------------
-- credential: login sessions
-- ---------------------------------------------------------------------------

-- A browser session is a short-lived token minted at login, so it is a
-- credential like the others: looked up by prefix, checked against a hash,
-- ended by revoked_at. One verification path, no session table.
ALTER TABLE credential
    DROP CONSTRAINT credential_kind_valid,
    DROP CONSTRAINT credential_token_lookup,
    ADD CONSTRAINT credential_kind_valid
        CHECK (kind IN ('password', 'sso', 'api_token', 'session')),
    ADD CONSTRAINT credential_token_lookup
        CHECK (kind NOT IN ('api_token', 'session') OR token_prefix IS NOT NULL),
    -- An API token may be long-lived. A session may not.
    ADD CONSTRAINT credential_session_expires
        CHECK (kind <> 'session' OR expires_at IS NOT NULL);

-- ---------------------------------------------------------------------------
-- course_member: the expiry sweep
-- ---------------------------------------------------------------------------

-- authorize() already ignores an expired member on every call; the sweep only
-- makes the removal visible and cancels that member's pending proposals.
CREATE INDEX course_member_expiry_idx ON course_member (expires_at)
    WHERE status <> 'removed' AND expires_at IS NOT NULL;

COMMIT;

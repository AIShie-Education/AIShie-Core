-- AIshiteru Core — migration 0006 (up)
-- Who issued a token. An administrator reviewing an agent's tokens needs to
-- know whose each one is, so that a token that leaks can be traced and
-- revoked alone. PostgreSQL 13+.

BEGIN;

-- Null for rows written before this column, for credentials that are not
-- issued (a password, a session, a linked identity), and for a token made
-- from the command line, which no actor issues.
ALTER TABLE credential
    ADD COLUMN issued_by_actor_id uuid REFERENCES actor (id);

COMMIT;

-- AIshiteru Core — before 0014_agent_owner_fixed.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with: an agent a person owns, and one
-- registered with no owner, neither of which may change hands while 0014 is
-- in. Committed, so that the down migration runs over it; the downs after it
-- drop it with everything else.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

INSERT INTO actor (id, kind, display_name, created_by_actor_id) VALUES
    ('00000000-0000-0000-0014-000000000031', 'human', 'Sato', NULL),
    ('00000000-0000-0000-0014-000000000032', 'human', 'Ken',  NULL),
    ('00000000-0000-0000-0014-000000000035', 'agent', 'Lab bot', NULL);
INSERT INTO actor (id, kind, display_name, owner_actor_id, created_by_actor_id) VALUES
    ('00000000-0000-0000-0014-000000000036', 'agent', 'Sato''s helper', '00000000-0000-0000-0014-000000000031',
     '00000000-0000-0000-0014-000000000031');

DO $chk$
BEGIN
    BEGIN
        UPDATE actor SET owner_actor_id = '00000000-0000-0000-0014-000000000032'
         WHERE id = '00000000-0000-0000-0014-000000000036';
        RAISE EXCEPTION 'FAIL  0014: an agent changed hands while 0014 was in';
    EXCEPTION WHEN restrict_violation THEN
        NULL;
    END;
END $chk$;

COMMIT;

-- AIshiteru Core — before 0011_site_chat.down.sql, in `make db-test-sql`
--
-- What the down migration has to deal with: an agent whose runtime declared
-- site chat with a token of its own. Committed, so that the down migration
-- runs over it; the downs after it drop it with everything else.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

INSERT INTO actor (id, kind, display_name, created_by_actor_id) VALUES
    ('00000000-0000-0000-0011-000000000031', 'human', 'Sato',  NULL);
INSERT INTO actor (id, kind, display_name, owner_actor_id, created_by_actor_id) VALUES
    ('00000000-0000-0000-0011-000000000036', 'agent', 'Course tutor', '00000000-0000-0000-0011-000000000031',
     '00000000-0000-0000-0011-000000000031');
-- c1 the runtime's token
INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, issued_by_actor_id) VALUES
    ('00000000-0000-0000-0011-0000000000c1', '00000000-0000-0000-0011-000000000036', 'api_token', 'h', 'down0011',
     '00000000-0000-0000-0011-000000000031');
UPDATE actor SET site_chat_credential_id = '00000000-0000-0000-0011-0000000000c1'
 WHERE id = '00000000-0000-0000-0011-000000000036';

COMMIT;

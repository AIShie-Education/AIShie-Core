-- AIshie Core — before 0025_agent_hosting.up.sql, in `make db-test-sql`
--
-- What the migration finds: Mei's agents, each run another way. Her tutor,
-- whose runtime declared site chat with a token of its own, beside her
-- laptop's token and one revoked long ago; her helper, whose runtime's
-- token was revoked, and which keeps another; her bot, whose runtime's token
-- expired; her script, which never declared and holds a token; and an
-- agent nobody owns, whose runtime declared. Committed, so that the
-- migration runs over it; it stays, for the redo and the down after it
-- (tests/down/0025.*), and the downs after that drop it with everything
-- else.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

-- 31 Mei · 35 her tutor · 36 her helper · 37 her bot · 38 her script · 39 an agent nobody owns
INSERT INTO actor (id, kind, display_name, created_by_actor_id) VALUES
    ('00000000-0000-0000-0025-000000000031', 'human', 'Mei', NULL);
INSERT INTO actor (id, kind, display_name, owner_actor_id, created_by_actor_id) VALUES
    ('00000000-0000-0000-0025-000000000035', 'agent', 'Tutor',  '00000000-0000-0000-0025-000000000031', '00000000-0000-0000-0025-000000000031'),
    ('00000000-0000-0000-0025-000000000036', 'agent', 'Helper', '00000000-0000-0000-0025-000000000031', '00000000-0000-0000-0025-000000000031'),
    ('00000000-0000-0000-0025-000000000037', 'agent', 'Bot',    '00000000-0000-0000-0025-000000000031', '00000000-0000-0000-0025-000000000031'),
    ('00000000-0000-0000-0025-000000000038', 'agent', 'Script', '00000000-0000-0000-0025-000000000031', '00000000-0000-0000-0025-000000000031'),
    ('00000000-0000-0000-0025-000000000039', 'agent', 'Enrolment bot', NULL, '00000000-0000-0000-0025-000000000031');

-- c1 the tutor's runtime's · c2 Mei's laptop's · c3 revoked long ago · c4 the helper's runtime's, revoked
-- · c5 the helper's other · c6 the bot's runtime's, expired · c7 the script's · c8 the enrolment bot's runtime's
INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, label, issued_by_actor_id, expires_at, revoked_at) VALUES
    ('00000000-0000-0000-0025-0000000000c1', '00000000-0000-0000-0025-000000000035', 'api_token', 'h', 'up25c1', 'runtime',
     '00000000-0000-0000-0025-000000000031', NULL, NULL),
    ('00000000-0000-0000-0025-0000000000c2', '00000000-0000-0000-0025-000000000035', 'api_token', 'h', 'up25c2', 'laptop',
     '00000000-0000-0000-0025-000000000031', NULL, NULL),
    ('00000000-0000-0000-0025-0000000000c3', '00000000-0000-0000-0025-000000000035', 'api_token', 'h', 'up25c3', 'old',
     '00000000-0000-0000-0025-000000000031', NULL, '2026-01-01 00:00:00+00'),
    ('00000000-0000-0000-0025-0000000000c4', '00000000-0000-0000-0025-000000000036', 'api_token', 'h', 'up25c4', 'runtime',
     '00000000-0000-0000-0025-000000000031', NULL, '2026-02-01 00:00:00+00'),
    ('00000000-0000-0000-0025-0000000000c5', '00000000-0000-0000-0025-000000000036', 'api_token', 'h', 'up25c5', 'editor',
     '00000000-0000-0000-0025-000000000031', NULL, NULL),
    ('00000000-0000-0000-0025-0000000000c6', '00000000-0000-0000-0025-000000000037', 'api_token', 'h', 'up25c6', 'runtime',
     '00000000-0000-0000-0025-000000000031', now() - interval '1 day', NULL),
    ('00000000-0000-0000-0025-0000000000c7', '00000000-0000-0000-0025-000000000038', 'api_token', 'h', 'up25c7', 'script',
     '00000000-0000-0000-0025-000000000031', NULL, NULL),
    ('00000000-0000-0000-0025-0000000000c8', '00000000-0000-0000-0025-000000000039', 'api_token', 'h', 'up25c8', 'runtime',
     NULL, NULL, NULL);
UPDATE actor SET site_chat_credential_id = '00000000-0000-0000-0025-0000000000c1' WHERE id = '00000000-0000-0000-0025-000000000035';
UPDATE actor SET site_chat_credential_id = '00000000-0000-0000-0025-0000000000c4' WHERE id = '00000000-0000-0000-0025-000000000036';
UPDATE actor SET site_chat_credential_id = '00000000-0000-0000-0025-0000000000c6' WHERE id = '00000000-0000-0000-0025-000000000037';
UPDATE actor SET site_chat_credential_id = '00000000-0000-0000-0025-0000000000c8' WHERE id = '00000000-0000-0000-0025-000000000039';

COMMIT;

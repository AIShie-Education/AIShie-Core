-- AIshie Core — before 0017_api_tokens_for_agents.up.sql, in `make db-test-sql`
--
-- What the migration finds: people with API tokens beside their passwords,
-- sessions, identities and invitations, root among them with the token
-- bootstrap gave it; and agents with tokens beside a password, a session,
-- an identity and an invitation, none of which it may keep. Committed, so
-- that the migration runs over it; it stays, for the redo and the down
-- after it (tests/down/0017.after.sql), and the downs after that drop it
-- with everything else.

\set ON_ERROR_STOP 1
\set QUIET 1
BEGIN;

-- 131 Jun, root · 132 Mei · 133 Lan, invited and not yet signed in
-- 135 Mei's helper, an agent she owns · 136 an enrolment bot nobody owns, with an email
INSERT INTO actor (id, kind, display_name, email, platform_role, created_by_actor_id) VALUES
    ('00000000-0000-0000-0017-000000000131', 'human', 'Jun', 'jun@up0017.example', 'root', NULL);
INSERT INTO actor (id, kind, display_name, email, login_id, created_by_actor_id) VALUES
    ('00000000-0000-0000-0017-000000000132', 'human', 'Mei', 'mei@up0017.example', 'UP0017-MEI', '00000000-0000-0000-0017-000000000131'),
    ('00000000-0000-0000-0017-000000000133', 'human', 'Lan', 'lan@up0017.example', NULL,         '00000000-0000-0000-0017-000000000131');
INSERT INTO actor (id, kind, display_name, email, owner_actor_id, created_by_actor_id) VALUES
    ('00000000-0000-0000-0017-000000000135', 'agent', 'Mei''s helper', NULL, '00000000-0000-0000-0017-000000000132',
     '00000000-0000-0000-0017-000000000132'),
    ('00000000-0000-0000-0017-000000000136', 'agent', 'enrolment bot', 'bot@up0017.example', NULL,
     '00000000-0000-0000-0017-000000000131');

-- Jun: 1a1 the token bootstrap gave root · 1a2 his password
INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, label) VALUES
    ('00000000-0000-0000-0017-0000000001a1', '00000000-0000-0000-0017-000000000131', 'api_token', 'h', 'up17root0001', 'bootstrap');
INSERT INTO credential (id, actor_id, kind, secret_hash) VALUES
    ('00000000-0000-0000-0017-0000000001a2', '00000000-0000-0000-0017-000000000131', 'password', '$argon2id$stand-in');
-- Mei: 1b1 a token for her script · 1b2 one revoked long ago, which keeps its date
-- · 1b3 one past its expiry, never revoked · 1b4 her password · 1b5 her session · 1b6 her identity
INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, label, issued_by_actor_id, expires_at, revoked_at) VALUES
    ('00000000-0000-0000-0017-0000000001b1', '00000000-0000-0000-0017-000000000132', 'api_token', 'h', 'up17mei00001', 'script',
     '00000000-0000-0000-0017-000000000132', NULL, NULL),
    ('00000000-0000-0000-0017-0000000001b2', '00000000-0000-0000-0017-000000000132', 'api_token', 'h', 'up17mei00002', 'laptop',
     '00000000-0000-0000-0017-000000000132', NULL, '2026-01-01 00:00:00+00'),
    ('00000000-0000-0000-0017-0000000001b3', '00000000-0000-0000-0017-000000000132', 'api_token', 'h', 'up17mei00003', 'last term',
     '00000000-0000-0000-0017-000000000131', now() - interval '1 day', NULL);
INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, expires_at) VALUES
    ('00000000-0000-0000-0017-0000000001b4', '00000000-0000-0000-0017-000000000132', 'password', '$argon2id$stand-in', NULL, NULL),
    ('00000000-0000-0000-0017-0000000001b5', '00000000-0000-0000-0017-000000000132', 'session',  'h', 'up17meisess1', now() + interval '12 hours');
INSERT INTO credential (id, actor_id, kind, provider, subject) VALUES
    ('00000000-0000-0000-0017-0000000001b6', '00000000-0000-0000-0017-000000000132', 'sso', 'school-adfs', 'mei@up0017.example');
-- Lan: 1c1 the invitation she has not taken up
INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, expires_at, issued_by_actor_id) VALUES
    ('00000000-0000-0000-0017-0000000001c1', '00000000-0000-0000-0017-000000000133', 'invite', 'h', 'up17laninv01',
     now() + interval '7 days', '00000000-0000-0000-0017-000000000131');
-- Mei's helper: 1d1 its runtime's token · 1d2 one Mei issued it · 1d3 a password it set itself
-- · 1d4 the session it signed in with · 1d5 an identity someone linked to it
INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, label, issued_by_actor_id) VALUES
    ('00000000-0000-0000-0017-0000000001d1', '00000000-0000-0000-0017-000000000135', 'api_token', 'h', 'up17help0001', 'runtime',
     '00000000-0000-0000-0017-000000000132'),
    ('00000000-0000-0000-0017-0000000001d2', '00000000-0000-0000-0017-000000000135', 'api_token', 'h', 'up17help0002', 'laptop',
     '00000000-0000-0000-0017-000000000132');
INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, expires_at) VALUES
    ('00000000-0000-0000-0017-0000000001d3', '00000000-0000-0000-0017-000000000135', 'password', '$argon2id$stand-in', NULL, NULL),
    ('00000000-0000-0000-0017-0000000001d4', '00000000-0000-0000-0017-000000000135', 'session',  'h', 'up17helpsess', now() + interval '12 hours');
INSERT INTO credential (id, actor_id, kind, provider, subject) VALUES
    ('00000000-0000-0000-0017-0000000001d5', '00000000-0000-0000-0017-000000000135', 'sso', 'school-adfs', 'helper@up0017.example');
-- The enrolment bot: 1e1 its token · 1e2 an invitation to its email
INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, label) VALUES
    ('00000000-0000-0000-0017-0000000001e1', '00000000-0000-0000-0017-000000000136', 'api_token', 'h', 'up17bot00001', 'sync');
INSERT INTO credential (id, actor_id, kind, secret_hash, token_prefix, expires_at, issued_by_actor_id) VALUES
    ('00000000-0000-0000-0017-0000000001e2', '00000000-0000-0000-0017-000000000136', 'invite', 'h', 'up17botinv01',
     now() + interval '7 days', '00000000-0000-0000-0017-000000000131');

COMMIT;

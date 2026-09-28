-- AIshiteru Core — migration 0012 (up)
-- Join links. Someone in a course makes a link to it, which students open —
-- typically by scanning the QR code the front end shows of it in class — to
-- be seated as students there and then: signed in, at once; with no account,
-- by registering through the link, which makes a person and seats them in one
-- transaction. Nothing else registers a person on their own: there is no open
-- sign-up. Every link lives ten minutes from when it is made, whoever makes
-- it: long enough for a room to scan it, too short for a photograph of the
-- screen passed around afterwards to be a way in. The link is a token, shown
-- once to whoever made it; only its hash is kept, beside a prefix that finds
-- the row. It seats with the authority of whoever made it, which is asked
-- again at every join. Core sends no email, so an email given when
-- registering through a link is recorded as nobody's word but the person's
-- own. Design reference: docs/schema.md §2.1, §2.2. PostgreSQL 13+.
--
-- The previous release keeps working while this goes in. The table is new,
-- and the release before neither reads nor writes it, nor
-- course_member.join_link_id, which is nullable. actor.email_verified is NOT
-- NULL with a default of true, which a person the previous release registers
-- takes, as every person before was registered by an administrator.

BEGIN;

-- Waits for a busy table rather than queueing every call behind it; a
-- deploy that times out here is run again.
SET LOCAL lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- actor: whether anyone but the person vouches for their email
-- ---------------------------------------------------------------------------

-- true: an administrator gave the email (actor.register, actor.invite_new,
-- actor.update), as every email before this migration was; or there is none.
-- false: the person typed it, registering through a join link, and Core,
-- which sends no email, has not checked it. Only a person registers so, with
-- an email: the CHECK reads kind to refuse, as actor_owned_is_agent (0007)
-- does; nothing that grants reads it. An administrator who sets the email
-- vouches for it (actor.update), and it is true again.
ALTER TABLE actor
    ADD COLUMN email_verified boolean NOT NULL DEFAULT true,
    ADD CONSTRAINT actor_unverified_email_is_a_persons
        CHECK (email_verified OR (kind = 'human' AND email IS NOT NULL));

-- ---------------------------------------------------------------------------
-- course_join_link: a way into a course, as a student, for whoever holds it
-- ---------------------------------------------------------------------------

-- The token is aisjoin_<prefix>_<secret>, made like an API token: token_prefix
-- finds the row, and secret_hash, the token's SHA-256, is compared in
-- constant time. The token itself is never kept; the CHECK holds that what
-- is kept is a hash.
--
-- It seats a student (role; a CHECK, so that another role is a decision
-- made in a migration) with the levels and scope of preset_id, the course's
-- student preset as member.add finds it when the link is made, and with the
-- authority of created_by_member_id: the seat is held to that member's own
-- when the link is made and at every join, and the link seats nobody once it
-- is not. It works until expires_at, ten minutes after it was made, always
-- (the CHECK: a longer life is a decision made in a migration); for
-- max_uses seats, when that is set, which uses counts, and never past it;
-- for an email in allowed_email_domains, when that is set; and until it is
-- revoked, which records who revoked it. Who made and who revoked it are
-- seats of its course, as whoever acts inside a course is.
CREATE TABLE course_join_link (
    id                    uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    course_id             uuid        NOT NULL REFERENCES course (id),
    token_prefix          text        NOT NULL,
    secret_hash           text        NOT NULL,
    role                  text        NOT NULL DEFAULT 'student',
    preset_id             uuid        NOT NULL REFERENCES permission_preset (id),
    created_by_member_id  uuid        NOT NULL,
    expires_at            timestamptz NOT NULL,
    max_uses              integer,
    uses                  integer     NOT NULL DEFAULT 0,
    allowed_email_domains text[],
    revoked_at            timestamptz,
    revoked_by_member_id  uuid,
    created_at            timestamptz NOT NULL DEFAULT now(),
    UNIQUE (token_prefix),
    -- Target of the composite foreign key from course_member, and what a
    -- course's links are listed by.
    UNIQUE (course_id, id),
    CONSTRAINT course_join_link_creator_fk
        FOREIGN KEY (course_id, created_by_member_id) REFERENCES course_member (course_id, id),
    CONSTRAINT course_join_link_revoker_fk
        FOREIGN KEY (course_id, revoked_by_member_id) REFERENCES course_member (course_id, id),
    CONSTRAINT course_join_link_secret_hashed CHECK (secret_hash ~ '^sha256:[0-9a-f]{64}$'),
    CONSTRAINT course_join_link_role_valid    CHECK (role = 'student'),
    -- Subtracting is immutable, as adding an interval to a timestamptz is not.
    CONSTRAINT course_join_link_lives_ten_minutes CHECK (expires_at - created_at = interval '10 minutes'),
    CONSTRAINT course_join_link_max_uses_valid CHECK (max_uses IS NULL OR max_uses > 0),
    -- Counting a use is one UPDATE under the link's lock; this is what makes
    -- one past the last impossible whatever the application does.
    CONSTRAINT course_join_link_uses_counted  CHECK (uses >= 0 AND (max_uses IS NULL OR uses <= max_uses)),
    CONSTRAINT course_join_link_revocation_recorded CHECK ((revoked_at IS NULL) = (revoked_by_member_id IS NULL)),
    -- Null is every domain. A list names at least one, and at most 20.
    CONSTRAINT course_join_link_domains_valid CHECK (
        allowed_email_domains IS NULL
        OR (cardinality(allowed_email_domains) BETWEEN 1 AND 20 AND array_position(allowed_email_domains, NULL) IS NULL))
);

-- ---------------------------------------------------------------------------
-- course_member: the link a seat was taken through
-- ---------------------------------------------------------------------------

-- Set on a seat a person took through a join link, so that whoever manages
-- the course sees who joined through which link; null for every other seat.
-- The composite key holds the link to the seat's own course.
--
-- NOT VALID: every existing row holds null, which passes. Validating would
-- scan every seat under the lock this migration holds, which stops every
-- write to the table. The key holds for every row written from now on.
ALTER TABLE course_member
    ADD COLUMN join_link_id uuid;
ALTER TABLE course_member
    ADD CONSTRAINT course_member_join_link_fk
        FOREIGN KEY (course_id, join_link_id) REFERENCES course_join_link (course_id, id) NOT VALID;
CREATE INDEX course_member_join_link_idx ON course_member (join_link_id) WHERE join_link_id IS NOT NULL;

COMMIT;

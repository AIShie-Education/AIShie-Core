-- AIshiteru Core — migration 0007 (up)
-- Agents a person owns, the delegate seats they act from, and three new
-- permissions. Design reference: docs/schema.md §2.1, §2.2. PostgreSQL 13+.
--
-- Two rules come in with it:
--
--   * An agent a person owns acts only as that person's delegate. Every seat
--     it holds that is not removed has a principal, its owner's seat in the
--     same course, and it never holds more than that seat does: the levels,
--     the reach and the life of the principal cap its own, and it is live
--     only while the principal is. Owning an agent, and holding its tokens,
--     gives nobody more than their own seat.
--   * The previous release keeps working while this one goes in: every
--     column is new and nullable, or defaults to 'denied', and nothing the
--     previous release writes is refused unless it would seat an agent
--     someone owns, which only this release makes, and refuses to seat so
--     itself.
--
-- Backfill, a one-time decision of policy recorded here and in
-- docs/schema.md §2.2: every seat that is not removed, and every
-- department's own preset, is given the three new levels of the built-in
-- preset of the same roster role (student, ta, instructor, observer); an
-- assistant keeps 'denied'. Role is read as a fact of the roster, by a
-- migration, never by authorization. Instructors, who seat nearly everyone,
-- get the most; everyone else at most what instructors hold, so that
-- "nobody hands out more than they hold" goes on holding for the seats as
-- they stand, and nobody who manages a department's presets is locked out
-- of the new permissions. A seat the previous release adds between this
-- migration and the cut-over to the new release is given 'denied'.

BEGIN;

-- Waits for a busy table rather than queueing every call behind it; a
-- deploy that times out here is run again.
SET LOCAL lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- actor: who owns an agent, and who suspended an actor
-- ---------------------------------------------------------------------------

-- An owner is a person. Agents do not own agents, and the system actor owns
-- nothing: the trigger below reads the owner's kind, as the credential
-- trigger of 0004 reads the system actor's, to refuse. Nothing that grants
-- reads it. An agent someone owns holds no platform role: owning it, and
-- holding its tokens, gives nobody more than their own seat, and the
-- previous release sets no owner and gives a role only at registration.
--
-- suspended_by_actor_id says who made the suspension in force, and is read
-- only while status is 'suspended'. Null there means "not the owner's":
-- a suspension made before this migration, or by the previous release, is
-- an administrator's, and only an administrator lifts it. No CHECK ties it
-- to status, so that the previous release's suspend and reactivate go on
-- working; actor_suspension_cleared below clears it whenever an actor is
-- made active again, by either release, so that a later suspension by the
-- previous release cannot inherit an owner's name.
ALTER TABLE actor
    ADD COLUMN owner_actor_id        uuid REFERENCES actor (id),
    ADD COLUMN suspended_by_actor_id uuid REFERENCES actor (id),
    ADD CONSTRAINT actor_not_own_owner    CHECK (owner_actor_id <> id),
    ADD CONSTRAINT actor_owned_is_agent   CHECK (owner_actor_id IS NULL OR kind = 'agent'),
    ADD CONSTRAINT actor_owned_holds_no_platform_role CHECK (owner_actor_id IS NULL OR platform_role IS NULL);
CREATE INDEX actor_owner_idx ON actor (owner_actor_id) WHERE owner_actor_id IS NOT NULL;

CREATE FUNCTION actor_owner_is_person() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.owner_actor_id IS NOT NULL
       AND NOT EXISTS (SELECT 1 FROM actor WHERE id = NEW.owner_actor_id AND kind = 'human') THEN
        RAISE EXCEPTION 'actor % cannot own actor %: an owner is a person', NEW.owner_actor_id, NEW.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER actor_owner_valid
    BEFORE INSERT OR UPDATE OF owner_actor_id ON actor
    FOR EACH ROW EXECUTE FUNCTION actor_owner_is_person();

CREATE FUNCTION actor_clear_suspender() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status = 'active' THEN
        NEW.suspended_by_actor_id := NULL;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER actor_suspension_cleared
    BEFORE UPDATE OF status ON actor
    FOR EACH ROW EXECUTE FUNCTION actor_clear_suspender();

-- ---------------------------------------------------------------------------
-- course_member: delegate seats
-- ---------------------------------------------------------------------------

-- A delegate seat names its principal: the seat, in the same course, of the
-- person who owns the delegate's actor. The composite key keeps the two in
-- one course.
--
-- A delegate answers its principal alone, unless its seat says it answers
-- the course (answers_course): a course's own question-answering agent,
-- brought in by someone who manages the course's members, whom the students
-- it is within may ask as well (docs/schema.md §2.8). It is chosen when the
-- seat is made, counts only while the principal still manages the members,
-- and only a delegate's seat carries it. The previous release writes no
-- delegate seats, and its seats take the default.
ALTER TABLE course_member
    ADD COLUMN principal_member_id uuid,
    ADD COLUMN answers_course boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT course_member_principal_fk
        FOREIGN KEY (course_id, principal_member_id) REFERENCES course_member (course_id, id),
    ADD CONSTRAINT course_member_not_own_principal CHECK (principal_member_id <> id),
    ADD CONSTRAINT course_member_answers_course_is_delegate CHECK (NOT answers_course OR principal_member_id IS NOT NULL);
CREATE INDEX course_member_principal_idx ON course_member (principal_member_id)
    WHERE principal_member_id IS NOT NULL;

-- For a seat that is not removed: the actor has an owner exactly when the
-- seat has a principal; the principal is the owner's seat; and a principal
-- is nobody's delegate, so there are no chains. A removed seat is history
-- and is left as it was. Authorization checks the same match on every call
-- (authz.ForActor), because an owner can change after a seat was taken —
-- in a course archived at the time — and the seat must then stop counting.
CREATE FUNCTION course_member_check_principal() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    owner     uuid;
    principal course_member;
BEGIN
    IF NEW.status = 'removed' THEN
        RETURN NEW;
    END IF;
    SELECT owner_actor_id INTO owner FROM actor WHERE id = NEW.actor_id;
    IF (owner IS NULL) <> (NEW.principal_member_id IS NULL) THEN
        RAISE EXCEPTION 'seat %: an agent someone owns is seated as their delegate, and nobody else is', NEW.id
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.principal_member_id IS NOT NULL THEN
        SELECT * INTO principal FROM course_member WHERE id = NEW.principal_member_id;
        IF principal.actor_id IS DISTINCT FROM owner THEN
            RAISE EXCEPTION 'seat %: its principal is not the seat of its actor''s owner', NEW.id
                USING ERRCODE = 'check_violation';
        END IF;
        IF principal.principal_member_id IS NOT NULL THEN
            RAISE EXCEPTION 'seat %: its principal is itself a delegate', NEW.id
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER course_member_principal_valid
    BEFORE INSERT OR UPDATE OF principal_member_id, actor_id, status ON course_member
    FOR EACH ROW EXECUTE FUNCTION course_member_check_principal();

-- ---------------------------------------------------------------------------
-- Three permissions, on both tables, in this order
-- ---------------------------------------------------------------------------

-- agent_delegate: bring an agent you own into the course as your delegate.
-- conversation_ask: open conversations, and write in those you opened.
-- conversation_answer: be addressed, and answer; the level is the autonomy
-- of the answers.
ALTER TABLE course_member
    ADD COLUMN perm_agent_delegate      autonomy_level NOT NULL DEFAULT 'denied',
    ADD COLUMN perm_conversation_ask    autonomy_level NOT NULL DEFAULT 'denied',
    ADD COLUMN perm_conversation_answer autonomy_level NOT NULL DEFAULT 'denied';
ALTER TABLE permission_preset
    ADD COLUMN perm_agent_delegate      autonomy_level NOT NULL DEFAULT 'denied',
    ADD COLUMN perm_conversation_ask    autonomy_level NOT NULL DEFAULT 'denied',
    ADD COLUMN perm_conversation_answer autonomy_level NOT NULL DEFAULT 'denied';

-- ---------------------------------------------------------------------------
-- The built-in presets as they stand, and the backfill. Last.
-- ---------------------------------------------------------------------------

-- The built-ins already seeded take the new levels. The two new built-ins,
-- delegate and course_tutor, are inserted by src/seed/presets.sql alone,
-- which a deploy runs after this.
UPDATE permission_preset p
   SET perm_agent_delegate = v.agent_delegate, perm_conversation_ask = v.ask, perm_conversation_answer = v.answer
  FROM (VALUES
        ('student',    'confirm_required'::autonomy_level, 'autonomous'::autonomy_level, 'denied'::autonomy_level),
        ('observer',   'denied',                           'denied',                     'denied'),
        ('ta',         'confirm_required',                 'autonomous',                 'denied'),
        ('instructor', 'autonomous',                       'autonomous',                 'autonomous'),
        ('tutor',      'denied',                           'denied',                     'autonomous'),
        ('grader',     'denied',                           'denied',                     'denied')
       ) AS v (name, agent_delegate, ask, answer)
 WHERE p.dept_id IS NULL AND p.name = v.name;

-- The levels of the built-in preset of each roster role, written out rather
-- than read from the built-ins, which a fresh installation has not seeded
-- yet and an old one may have edited.
UPDATE course_member m
   SET perm_agent_delegate = v.agent_delegate, perm_conversation_ask = v.ask, perm_conversation_answer = v.answer
  FROM (VALUES
        ('student',    'confirm_required'::autonomy_level, 'autonomous'::autonomy_level, 'denied'::autonomy_level),
        ('observer',   'denied',                           'denied',                     'denied'),
        ('ta',         'confirm_required',                 'autonomous',                 'denied'),
        ('instructor', 'autonomous',                       'autonomous',                 'autonomous')
       ) AS v (role, agent_delegate, ask, answer)
 WHERE m.status <> 'removed' AND m.role = v.role;

UPDATE permission_preset p
   SET perm_agent_delegate = v.agent_delegate, perm_conversation_ask = v.ask, perm_conversation_answer = v.answer
  FROM (VALUES
        ('student',    'confirm_required'::autonomy_level, 'autonomous'::autonomy_level, 'denied'::autonomy_level),
        ('observer',   'denied',                           'denied',                     'denied'),
        ('ta',         'confirm_required',                 'autonomous',                 'denied'),
        ('instructor', 'autonomous',                       'autonomous',                 'autonomous')
       ) AS v (role, agent_delegate, ask, answer)
 WHERE p.dept_id IS NOT NULL AND p.role = v.role;

COMMIT;

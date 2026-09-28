-- AIshiteru Core — migration 0013 (up)
-- The permission member_invite: making a course's join links (0012), and
-- listing and revoking them. It is not member_manage, which seats whoever the
-- manager names with whatever the manager holds: a link seats whoever holds
-- it, unseen, typically a room scanning a QR code, and whether a seat may
-- hand that out is a decision of its own. Design reference: docs/schema.md
-- §2.2. PostgreSQL 13+.
--
-- The previous release keeps working while this goes in. The columns are new,
-- NOT NULL with a default of 'denied', which a seat or a preset the previous
-- release adds takes; it neither reads nor writes them, and makes no links.

BEGIN;

-- Waits for a busy table rather than queueing every call behind it; a
-- deploy that times out here is run again.
SET LOCAL lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- The permission, on both tables, after the others
-- ---------------------------------------------------------------------------

ALTER TABLE course_member
    ADD COLUMN perm_member_invite autonomy_level NOT NULL DEFAULT 'denied';
ALTER TABLE permission_preset
    ADD COLUMN perm_member_invite autonomy_level NOT NULL DEFAULT 'denied';

-- ---------------------------------------------------------------------------
-- The levels it starts at: a one-time decision, recorded here and in
-- docs/schema.md §2.2
-- ---------------------------------------------------------------------------

-- A seat held by a person, not removed: its member_manage level. Whoever
-- seats students one by one may seat them by a link, as freely: nobody gains
-- a way into the course they did not have, and nobody who had one loses it.
--
-- A seat held by an agent, delegate or not, and any other that is not a
-- person's: denied, whatever it manages. A link is handed to a room, unseen,
-- and an agent hands one out only once someone who manages the course's
-- members has decided it may (member.update_perms), as with any permission;
-- a delegate's is capped by its principal's in any case. This reads
-- actor.kind, as a migration may: authorization never does. An agent that
-- manages members and seats others with a preset that now carries
-- member_invite (the built-in instructor) names member_invite denied when it
-- does, or is given it: nobody grants more than they hold.
--
-- A removed seat is history, and is left denied.
UPDATE course_member m
   SET perm_member_invite = m.perm_member_manage
  FROM actor a
 WHERE a.id = m.actor_id AND a.kind = 'human' AND m.status <> 'removed'
   AND m.perm_member_manage <> 'denied';

-- A preset: denied for a student's (role student) and for every preset an
-- agent is seated with (role assistant: the built-ins tutor, grader,
-- delegate and course_tutor, and a department's own for agents), whatever
-- it manages; for any other — instructor, ta, observer — its member_manage
-- level. A preset carries no actor, and its role is what says whom it is
-- for. The built-ins come out as src/seed/presets.sql makes them on a fresh
-- installation, where this finds none yet: instructor autonomous, every
-- other denied.
UPDATE permission_preset
   SET perm_member_invite = perm_member_manage
 WHERE role NOT IN ('student', 'assistant') AND perm_member_manage <> 'denied';

COMMIT;

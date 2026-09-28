-- name: GetActor :one
-- The whole row, for showing an actor. Authorization uses GetActorForAuthz,
-- which leaves kind out on purpose.
SELECT id, kind, display_name, email, status, platform_role, created_by_actor_id, created_at,
       owner_actor_id, suspended_by_actor_id, site_chat_credential_id, email_verified
FROM actor
WHERE id = $1;

-- name: GetActorForShare :one
-- The same, FOR SHARE, for a write that acts on who owns the actor, or on
-- whether it may be seated, and does not change the row: issuing an owned
-- agent a token, seating it, taking it out of a course. An owner's change
-- (actor.set_owner) locks the row first, FOR NO KEY UPDATE, so either it
-- waits for the write, and its revocations and its count of seats then see
-- what the write did, or the write waits for it and reads the owner it
-- made. The foreign keys to the row take only KEY SHARE, which neither
-- conflicts with.
SELECT id, kind, display_name, email, status, platform_role, created_by_actor_id, created_at,
       owner_actor_id, suspended_by_actor_id, site_chat_credential_id, email_verified
FROM actor
WHERE id = $1
FOR SHARE;

-- name: GetActorByEmail :one
SELECT id, status FROM actor WHERE lower(email) = lower($1);

-- name: InsertActor :exec
INSERT INTO actor (id, kind, display_name, email, status, platform_role, created_by_actor_id, created_at, owner_actor_id)
VALUES ($1, $2, $3, $4, 'active', $5, $6, $7, sqlc.narg(owner_actor_id));

-- name: CountRootActors :one
SELECT count(*) FROM actor WHERE platform_role = 'root';

-- name: GetSystemActor :one
SELECT id FROM actor WHERE kind = 'system' ORDER BY created_at LIMIT 1;

-- name: ListMembershipsForActor :many
SELECT m.id AS member_id, m.course_id, c.code, c.section, c.title, c.status AS course_status,
       m.role, m.status, m.expires_at, m.student_scope, m.assignment_scope, m.principal_member_id, m.answers_course
FROM course_member m
JOIN course c ON c.id = m.course_id
WHERE m.actor_id = $1 AND m.status <> 'removed'
ORDER BY c.code, c.section, m.id;

-- name: LockOwnerForAgents :exec
-- An owner's agents are counted against the limit one creation, or one
-- reactivation, at a time. NO KEY UPDATE, not UPDATE: the call's own action
-- row, and every other action row naming the owner, holds KEY SHARE on this
-- row through its foreign key, and FOR UPDATE would wait for all of them,
-- and deadlock with another call of the owner's doing the same.
SELECT 1 FROM actor WHERE id = $1 FOR NO KEY UPDATE;

-- name: CountActiveAgentsOf :one
-- The agents a person owns that are not suspended: what the limit counts.
SELECT count(*) FROM actor WHERE owner_actor_id = $1 AND status <> 'suspended';

-- name: ListAgentsOf :many
-- A person's agents, oldest first, with what their owner needs to see at a
-- glance: when one last used a token that still works, how many seats it
-- holds that count now, how many requests of the owner's to seat it wait
-- for a decision, and whether it takes conversations in the site now, by
-- the rule of SiteChatOf.
SELECT a.id, a.display_name, a.status, a.suspended_by_actor_id, a.created_at, seen.last_used_at AS last_seen_at,
       (a.status = 'active'
        AND (a.owner_actor_id IS NULL OR EXISTS (SELECT 1 FROM actor o WHERE o.id = a.owner_actor_id AND o.status = 'active'))
        AND EXISTS (SELECT 1 FROM credential sc
                     WHERE sc.id = a.site_chat_credential_id AND sc.actor_id = a.id AND sc.revoked_at IS NULL
                       AND (sc.expires_at IS NULL OR sc.expires_at > sqlc.arg(now))))::bool AS site_chat,
       (SELECT count(*) FROM course_member m
         WHERE m.actor_id = a.id AND m.status = 'active'
           AND (m.expires_at IS NULL OR m.expires_at > sqlc.arg(now))) AS live_seats,
       (SELECT count(*) FROM action x
         WHERE x.target_type = 'actor' AND x.target_id = a.id AND x.action_type = 'member.add_delegate'
           AND x.status = 'proposed' AND x.actor_id = a.owner_actor_id) AS pending_requests
FROM actor a
LEFT JOIN LATERAL (
    SELECT c.last_used_at FROM credential c
    WHERE c.actor_id = a.id AND c.kind = 'api_token' AND c.revoked_at IS NULL AND c.last_used_at IS NOT NULL
      AND (c.expires_at IS NULL OR c.expires_at > sqlc.arg(now))
    ORDER BY c.last_used_at DESC LIMIT 1) seen ON true
WHERE a.owner_actor_id = sqlc.arg(owner_actor_id)
ORDER BY a.id;

-- name: AgentLastSeen :many
-- When an agent last used a token that still works: no row if never.
SELECT c.last_used_at FROM credential c
WHERE c.actor_id = sqlc.arg(actor_id) AND c.kind = 'api_token' AND c.revoked_at IS NULL AND c.last_used_at IS NOT NULL
  AND (c.expires_at IS NULL OR c.expires_at > sqlc.arg(now))
ORDER BY c.last_used_at DESC LIMIT 1;

-- name: ListSeatsOfActor :many
-- Every seat an actor holds that is not removed, with its course and the
-- name of the preset it was copied from.
SELECT m.id AS member_id, m.course_id, c.code, c.section, c.title, c.status AS course_status,
       m.status, m.expires_at, m.student_scope, m.assignment_scope, m.principal_member_id, m.answers_course,
       p.name AS preset_name
FROM course_member m
JOIN course c ON c.id = m.course_id
LEFT JOIN permission_preset p ON p.id = m.preset_id
WHERE m.actor_id = $1 AND m.status <> 'removed'
ORDER BY c.code, c.section, m.id;

-- name: ListDelegateRequestsFor :many
-- The proposals of its owner's to seat an agent as their delegate that wait
-- for a decision. Only the owner's: an owner changed since is not shown what
-- the one before asked for (actor.set_owner cancels those anyway).
SELECT x.id, x.course_id, c.code, c.section, c.title, x.created_at
FROM action x
JOIN course c ON c.id = x.course_id
JOIN actor a ON a.id = x.target_id
WHERE x.target_type = 'actor' AND x.target_id = $1 AND x.action_type = 'member.add_delegate' AND x.status = 'proposed'
  AND x.actor_id = a.owner_actor_id
ORDER BY x.id;

-- name: LockDelegateRequestsFor :many
-- Every proposal to seat an agent that waits for a decision, locked, for an
-- owner's change to cancel. One a decision holds already is passed over:
-- the decision is waiting for the agent's row, which the owner's change
-- holds, and finds the agent is no longer the proposer's once it has it.
SELECT x.id, x.course_id
FROM action x
WHERE x.target_type = 'actor' AND x.target_id = $1 AND x.action_type = 'member.add_delegate' AND x.status = 'proposed'
ORDER BY x.id
FOR UPDATE SKIP LOCKED;

-- name: SuspendAgentByOwner :execrows
UPDATE actor SET status = 'suspended', suspended_by_actor_id = sqlc.arg(owner_actor_id)
WHERE id = sqlc.arg(id) AND owner_actor_id = sqlc.arg(owner_actor_id) AND status = 'active';

-- name: ReactivateAgentByOwner :execrows
-- Only a suspension the owner made: one an administrator made, or one made
-- before this was recorded, is an administrator's to lift.
UPDATE actor SET status = 'active', suspended_by_actor_id = NULL
WHERE id = sqlc.arg(id) AND owner_actor_id = sqlc.arg(owner_actor_id) AND status = 'suspended'
  AND suspended_by_actor_id = sqlc.arg(owner_actor_id);

-- name: LockAgentForOwnerChange :exec
-- The agent's row, for the rest of an owner's change, before anything about
-- it is looked at. NO KEY UPDATE, as LockOwnerForAgents, not UPDATE: every
-- action row naming the agent holds KEY SHARE on it through its foreign key.
-- The writes that act on who owns it read it FOR SHARE (GetActorForShare),
-- and wait for this, or this for them.
SELECT 1 FROM actor WHERE id = $1 FOR NO KEY UPDATE;

-- name: SetActorOwner :exec
UPDATE actor SET owner_actor_id = sqlc.narg(owner_actor_id) WHERE id = sqlc.arg(id);

-- name: CountSeatsInOpenCourses :one
-- Seats an actor holds, not removed, in courses that are not archived: while
-- there is one, its owner does not change.
SELECT count(*) FROM course_member m JOIN course c ON c.id = m.course_id
WHERE m.actor_id = $1 AND m.status <> 'removed' AND c.status <> 'archived';

-- name: RevokeAllCredentials :exec
-- Every credential an actor has, of every kind: tokens and sessions, a
-- password, an invitation, a linked identity. When an agent changes hands,
-- whoever owned it before may hold any of them: a token issued, a password
-- set through one, an invitation waiting.
UPDATE credential SET revoked_at = $2
WHERE actor_id = $1 AND revoked_at IS NULL;

-- name: LookupActorByEmail :one
-- The one person or agent a whole email address belongs to, in any case,
-- and whether they can sign in, as GetActorView says it. Never the system
-- actor. There is no partial match: this finds someone whose address the
-- caller already has, and lists nobody.
SELECT a.id, a.kind, a.display_name, a.status,
       EXISTS (SELECT 1 FROM credential p WHERE p.actor_id = a.id AND p.kind = 'password' AND p.revoked_at IS NULL) AS has_password,
       EXISTS (SELECT 1 FROM credential s WHERE s.actor_id = a.id AND s.kind = 'sso' AND s.revoked_at IS NULL) AS has_sso,
       i.expires_at AS invite_expires_at
FROM actor a
LEFT JOIN credential i ON i.actor_id = a.id AND i.kind = 'invite' AND i.revoked_at IS NULL
WHERE a.kind <> 'system' AND lower(a.email) = lower(sqlc.arg(email));

-- name: InvitableBy :one
-- What decides whether a department administrator (issuer) may invite a
-- person (actor), and whether that invitation may still be taken up: an
-- invitation is the keys to the account, so the account must reach nothing
-- beyond what the issuer administers. One row of facts; the rule is the
-- caller's. seats_outside counts the person's live seats in courses outside
-- the departments the issuer administers, and beneath them, as ListCourses
-- finds those.
WITH RECURSIVE mine (id) AS (
    SELECT da.dept_id FROM department_admin da
    WHERE da.actor_id = sqlc.arg(issuer_id)::uuid AND da.removed_at IS NULL
  UNION
    SELECT d.id FROM department d JOIN mine ON d.parent_id = mine.id
)
SELECT a.kind = 'human' AS is_person,
       EXISTS (SELECT 1 FROM credential c WHERE c.actor_id = a.id AND c.kind IN ('password', 'sso') AND c.revoked_at IS NULL) AS can_sign_in,
       (a.platform_role IS NOT NULL)::bool AS holds_role,
       EXISTS (SELECT 1 FROM department_admin x WHERE x.actor_id = a.id AND x.removed_at IS NULL) AS administers,
       EXISTS (SELECT 1 FROM actor o WHERE o.owner_actor_id = a.id) AS owns_agents,
       (SELECT count(*) FROM course_member m JOIN course c ON c.id = m.course_id
        WHERE m.actor_id = a.id AND m.status <> 'removed' AND c.dept_id NOT IN (SELECT id FROM mine)) AS seats_outside,
       EXISTS (SELECT 1 FROM actor i WHERE i.id = sqlc.arg(issuer_id)::uuid AND i.platform_role IN ('root', 'admin')) AS issuer_platform
FROM actor a
WHERE a.id = sqlc.arg(actor_id)::uuid;

-- name: SiteChatOf :many
-- Whether each of the given actors takes conversations in the site now
-- (docs/schema.md §2.8): an agent does while the credential with which a
-- program that runs it declared so (me.site_chat) is live, neither revoked
-- nor expired, the agent is active, and its owner, if it has one, is
-- active. A person or the system actor never does; agent says which is
-- which, so that a view can leave people out. ListAgentsOf and
-- ListRespondentCandidates hold the same rule: a change to one is a change
-- to all three.
SELECT a.id, (a.kind = 'agent')::bool AS agent,
       (a.status = 'active'
        AND (a.owner_actor_id IS NULL OR EXISTS (SELECT 1 FROM actor o WHERE o.id = a.owner_actor_id AND o.status = 'active'))
        AND EXISTS (SELECT 1 FROM credential sc
                     WHERE sc.id = a.site_chat_credential_id AND sc.actor_id = a.id AND sc.revoked_at IS NULL
                       AND (sc.expires_at IS NULL OR sc.expires_at > sqlc.arg(now))))::bool AS site_chat
FROM actor a
WHERE a.id = ANY(sqlc.arg(actor_ids)::uuid[]);

-- name: SetSiteChat :exec
-- The credential an agent calls with declares that it takes conversations
-- in the site, in place of any that did before; null, that it takes none.
-- The key holds a credential to the agent's own (actor_site_chat_credential_fk).
UPDATE actor SET site_chat_credential_id = sqlc.narg(credential_id) WHERE id = sqlc.arg(id);

-- name: EndSiteChatByOwner :exec
-- Its owner switches it off: only while they are its owner, since an
-- owner's change (actor.set_owner) holds the row, and revokes every
-- credential it has, which ends it anyway.
UPDATE actor SET site_chat_credential_id = NULL
WHERE id = sqlc.arg(id) AND owner_actor_id = sqlc.arg(owner_actor_id);

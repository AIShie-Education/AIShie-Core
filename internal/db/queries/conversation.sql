-- Conversations (docs/schema.md §2.8). Who may address whom is decided in
-- Go, by one function (tools.addressing), from the seats as authorization
-- reads them; these queries find candidates and write rows, and every list
-- among them is limited in SQL to what the caller may list: their own
-- conversations, and those they oversee within their scope.

-- name: InsertConversation :exec
INSERT INTO conversation (id, course_id, opener_member_id, respondent_member_id, title, created_at)
VALUES ($1, $2, $3, $4, sqlc.narg(title), $5);

-- name: GetConversationInCourse :one
SELECT * FROM conversation WHERE id = $1 AND course_id = $2;

-- name: TouchConversation :execrows
-- The first half of writing a message: who spoke last, WHERE the
-- conversation is open. It takes the conversation's row lock, so a close
-- waits for the message or the message finds it closed (no row), and two
-- messages in one conversation are written one after the other.
UPDATE conversation SET last_message_at = sqlc.arg(at), last_author_member_id = sqlc.arg(author_member_id)
WHERE id = sqlc.arg(id) AND status = 'open';

-- name: InsertConversationMessage :one
-- The second half, under the lock TouchConversation took: the next seq in
-- this conversation.
INSERT INTO conversation_message (id, conversation_id, course_id, seq, author_member_id, in_reply_to_message_id, body,
                                  created_by_action_id, created_at)
VALUES (sqlc.arg(id), sqlc.arg(conversation_id), sqlc.arg(course_id),
        (SELECT coalesce(max(x.seq), 0) + 1 FROM conversation_message x WHERE x.conversation_id = sqlc.arg(conversation_id)),
        sqlc.arg(author_member_id), sqlc.narg(in_reply_to_message_id), sqlc.arg(body), sqlc.arg(created_by_action_id), sqlc.arg(created_at))
RETURNING seq;

-- name: CloseConversation :execrows
UPDATE conversation SET status = 'closed', closed_reason = sqlc.narg(reason)
WHERE id = $1 AND status = 'open';

-- name: CloseConversationsOf :many
-- Seats that are removed — one, and its delegates' with it — take part in
-- no conversation any more. Whoever removes them holds the seat FOR UPDATE,
-- and its delegates' rows are updated already, which a call writing in one
-- of these waits for (it takes both participants' seats before the
-- conversation). The conversations are locked in id order, so that two
-- removals whose seats share conversations take them in one order.
UPDATE conversation SET status = 'closed', closed_reason = 'seat_removed'
WHERE id IN (SELECT x.id FROM conversation x
             WHERE x.status = 'open'
               AND (x.opener_member_id = ANY(sqlc.arg(member_ids)::uuid[]) OR x.respondent_member_id = ANY(sqlc.arg(member_ids)::uuid[]))
             ORDER BY x.id
             FOR UPDATE)
RETURNING id;

-- name: GetConversationMessage :one
-- A message with what its conversation says about who may act on it.
SELECT m.id, m.conversation_id, m.author_member_id, m.in_reply_to_message_id,
       c.opener_member_id, c.respondent_member_id
FROM conversation_message m
JOIN conversation c ON c.id = m.conversation_id
WHERE m.id = $1 AND m.course_id = $2;

-- name: LatestOpenerMessage :one
-- The opener's newest message: the one an answer is to answer.
SELECT m.id, m.seq
FROM conversation_message m
JOIN conversation c ON c.id = m.conversation_id
WHERE m.conversation_id = $1 AND m.author_member_id = c.opener_member_id
ORDER BY m.seq DESC
LIMIT 1;

-- name: AnsweredSince :one
-- Whether the respondent has written since a seq: an answer to the opener's
-- latest message is there already. Asked under the conversation's row lock,
-- which every message is written under.
SELECT EXISTS (
    SELECT 1 FROM conversation_message m
    JOIN conversation c ON c.id = m.conversation_id
    WHERE m.conversation_id = sqlc.arg(conversation_id) AND m.author_member_id = c.respondent_member_id
      AND m.seq > sqlc.arg(after_seq))::bool AS answered;

-- name: AnswerPendingFor :one
-- Whether an answer of the member's to one question waits for a decision.
SELECT EXISTS (
    SELECT 1 FROM action a
    WHERE a.target_type = 'conversation' AND a.target_id = sqlc.arg(conversation_id)::uuid
      AND a.action_type = 'conversation.answer' AND a.status = 'proposed' AND a.member_id = sqlc.arg(member_id)::uuid
      AND a.payload->>'in_reply_to_message_id' = sqlc.arg(in_reply_to_message_id)::uuid::text)::bool AS pending;

-- name: InsertRetraction :execrows
INSERT INTO conversation_message_retraction (message_id, course_id, retracted_by_member_id, created_by_action_id, reason, created_at)
VALUES ($1, $2, $3, $4, sqlc.narg(reason), $5)
ON CONFLICT (message_id) DO NOTHING;

-- name: ListConversationMessagesAfter :many
-- Oldest first, after a seq.
SELECT m.id, m.seq, m.author_member_id, m.in_reply_to_message_id, m.body, m.created_at,
       r.created_at AS retracted_at, r.retracted_by_member_id, r.reason AS retraction_reason
FROM conversation_message m
LEFT JOIN conversation_message_retraction r ON r.message_id = m.id
WHERE m.conversation_id = $1 AND m.seq > sqlc.arg(after_seq)
ORDER BY m.seq
LIMIT sqlc.arg(max_rows);

-- name: ListConversationMessagesBefore :many
-- Newest first, before a seq: the tail of a conversation, turned round by
-- the caller.
SELECT m.id, m.seq, m.author_member_id, m.in_reply_to_message_id, m.body, m.created_at,
       r.created_at AS retracted_at, r.retracted_by_member_id, r.reason AS retraction_reason
FROM conversation_message m
LEFT JOIN conversation_message_retraction r ON r.message_id = m.id
WHERE m.conversation_id = $1 AND m.seq < sqlc.arg(before_seq)
ORDER BY m.seq DESC
LIMIT sqlc.arg(max_rows);

-- name: ConversationDetails :many
-- What the views show of each conversation: its two participants, whether a
-- reply to the opener's newest message waits for a decision, that message,
-- and when a message in it was last retracted. last_seen_at is an agent's:
-- when it last used a token that still works. A reply waiting for a decision
-- about an older message is not waited for: approving it can only fail,
-- since the conversation has moved on.
SELECT c.id, c.course_id, c.title, c.status, c.closed_reason, c.created_at, c.last_message_at, c.last_author_member_id,
       c.opener_member_id, oa.display_name AS opener_name, oa.kind AS opener_kind,
       c.respondent_member_id, r.actor_id AS respondent_actor_id, ra.display_name AS respondent_name, ra.kind AS respondent_kind,
       r.role AS respondent_role,
       r.status AS respondent_status, r.expires_at AS respondent_expires_at,
       r.principal_member_id AS respondent_principal_member_id, own.display_name AS respondent_owner_name,
       seen.last_used_at AS respondent_last_seen_at, pending.id AS pending_reply_action_id,
       latest.id AS latest_opener_message_id, retracted.created_at AS last_retracted_at
FROM conversation c
JOIN course_member o ON o.id = c.opener_member_id
JOIN actor oa ON oa.id = o.actor_id
JOIN course_member r ON r.id = c.respondent_member_id
JOIN actor ra ON ra.id = r.actor_id
LEFT JOIN actor own ON own.id = ra.owner_actor_id
LEFT JOIN LATERAL (
    SELECT cr.last_used_at FROM credential cr
    WHERE ra.kind = 'agent' AND cr.actor_id = ra.id AND cr.kind = 'api_token' AND cr.revoked_at IS NULL
      AND cr.last_used_at IS NOT NULL AND (cr.expires_at IS NULL OR cr.expires_at > sqlc.arg(now))
    ORDER BY cr.last_used_at DESC LIMIT 1) seen ON true
LEFT JOIN conversation_message latest ON latest.id = (
    SELECT m.id FROM conversation_message m
    WHERE m.conversation_id = c.id AND m.author_member_id = c.opener_member_id
    ORDER BY m.seq DESC LIMIT 1)
LEFT JOIN conversation_message_retraction retracted ON retracted.message_id = (
    SELECT x.message_id FROM conversation_message_retraction x
    JOIN conversation_message xm ON xm.id = x.message_id
    WHERE xm.conversation_id = c.id
    ORDER BY x.created_at DESC LIMIT 1)
LEFT JOIN action pending ON pending.id = (
    SELECT a.id FROM action a
    WHERE a.target_type = 'conversation' AND a.target_id = c.id AND a.action_type = 'conversation.answer'
      AND a.status = 'proposed' AND a.member_id = c.respondent_member_id
      AND a.payload->>'in_reply_to_message_id' = latest.id::text
    ORDER BY a.id DESC LIMIT 1)
WHERE c.id = ANY(sqlc.arg(ids)::uuid[])
ORDER BY c.id;

-- name: ListConversationIDs :many
-- The conversations a member may list, paged by id: those it opened, those
-- addressed to it, and, for someone who decides actions, those whose opener
-- is within its student scope (and its principal's, for a delegate), in SQL.
-- state is a ConversationView state, or open; a reply waits for approval
-- only if it answers the opener's newest message (ConversationDetails).
SELECT c.id
FROM conversation c
WHERE c.course_id = $1 AND c.id > sqlc.arg(after)
  AND ( (sqlc.arg(as_opener)::bool AND c.opener_member_id = sqlc.arg(member_id))
     OR (sqlc.arg(as_respondent)::bool AND c.respondent_member_id = sqlc.arg(member_id))
     OR (sqlc.arg(as_overseer)::bool
         AND (sqlc.arg(student_all)::bool OR EXISTS (
               SELECT 1 FROM member_student_scope x WHERE x.member_id = sqlc.arg(member_id) AND x.student_member_id = c.opener_member_id))
         AND (sqlc.arg(principal_student_all)::bool OR EXISTS (
               SELECT 1 FROM member_student_scope px WHERE px.member_id = sqlc.arg(principal_id) AND px.student_member_id = c.opener_member_id))) )
  AND ( sqlc.narg(state)::text IS NULL
     OR (sqlc.narg(state)::text = 'open' AND c.status = 'open')
     OR sqlc.narg(state)::text = (CASE
            WHEN c.status = 'closed' THEN 'closed'
            WHEN EXISTS (SELECT 1 FROM action a
                          WHERE a.target_type = 'conversation' AND a.target_id = c.id AND a.action_type = 'conversation.answer'
                            AND a.status = 'proposed' AND a.member_id = c.respondent_member_id
                            AND a.payload->>'in_reply_to_message_id' = (
                                SELECT m.id::text FROM conversation_message m
                                WHERE m.conversation_id = c.id AND m.author_member_id = c.opener_member_id
                                ORDER BY m.seq DESC LIMIT 1)) THEN 'reply_pending_approval'
            WHEN c.last_author_member_id = c.opener_member_id THEN 'awaiting_answer'
            ELSE 'answered' END) )
ORDER BY c.id
LIMIT sqlc.arg(max_rows);

-- name: ListMyConversations :many
-- The conversations the given seats opened, newest activity first — its
-- last message, or its opening while it has none — after a
-- (last_activity_at, id) cursor, both descending. The seats are the
-- caller's own that count now, which me.conversations works out before
-- this, as authorization would: nothing here reads anyone else's.
SELECT c.id, c.opener_member_id, coalesce(c.last_message_at, c.created_at)::timestamptz AS last_activity_at
FROM conversation c
WHERE c.opener_member_id = ANY(sqlc.arg(member_ids)::uuid[])
  AND (sqlc.narg(after_at)::timestamptz IS NULL
       OR (coalesce(c.last_message_at, c.created_at), c.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY coalesce(c.last_message_at, c.created_at) DESC, c.id DESC
LIMIT sqlc.arg(max_rows);

-- name: ListInboxConversationIDs :many
-- Open conversations addressed to a seat in which the opener spoke last,
-- the opener's newest message is not retracted, and no answer of the seat's
-- to that message waits for a decision, the longest waiting first, after a
-- (last_message_at, id) cursor. The opener's seat, its actor and, for a
-- delegate, its principal's seat must be live: whatever cannot count again
-- by itself is left out here, so that it does not stand for good in front
-- of what can be answered. Whether each opener may still address the seat
-- is for the caller to say, in Go.
SELECT c.id, c.last_message_at
FROM conversation c
JOIN course_member o ON o.id = c.opener_member_id
JOIN actor oa ON oa.id = o.actor_id AND oa.status = 'active'
LEFT JOIN course_member op ON op.id = o.principal_member_id
JOIN LATERAL (
    SELECT m.id FROM conversation_message m
    WHERE m.conversation_id = c.id AND m.author_member_id = c.opener_member_id
    ORDER BY m.seq DESC LIMIT 1) latest ON true
WHERE c.respondent_member_id = sqlc.arg(member_id) AND c.status = 'open'
  AND c.last_author_member_id = c.opener_member_id
  AND o.status = 'active' AND (o.expires_at IS NULL OR o.expires_at > sqlc.arg(now))
  AND (o.principal_member_id IS NULL
       OR (op.status = 'active' AND (op.expires_at IS NULL OR op.expires_at > sqlc.arg(now))))
  AND NOT EXISTS (SELECT 1 FROM conversation_message_retraction x WHERE x.message_id = latest.id)
  AND NOT EXISTS (SELECT 1 FROM action a
                   WHERE a.target_type = 'conversation' AND a.target_id = c.id AND a.action_type = 'conversation.answer'
                     AND a.status = 'proposed' AND a.member_id = sqlc.arg(member_id)
                     AND a.payload->>'in_reply_to_message_id' = latest.id::text)
  AND (sqlc.narg(after_at)::timestamptz IS NULL
       OR (c.last_message_at, c.id) > (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY c.last_message_at, c.id
LIMIT sqlc.arg(max_rows);

-- name: ListRespondentCandidates :many
-- The seats that might answer a caller: agents' seats, live, held by an
-- active actor, with conversation_answer not denied on the row, and, for a
-- delegate, either the caller's own or one that answers the course, whose
-- principal's row holds member_manage. Never a person's: conversations are
-- with agents. An agent's seat only while the agent takes conversations in
-- the site (docs/schema.md §2.8), by the rule of SiteChatOf, its status
-- asked above: one operated from an external tool is asked there, not here.
-- kind is read to leave out, never to let in. Which of them the caller may
-- address is decided in Go (tools.addressing), which this only narrows to
-- what it could accept: every student's own agent answers, and only its
-- principal. Unpaged: what is left is a course's agents, and the caller's
-- own agents, a handful; max_rows bounds them anyway.
SELECT m.id, a.display_name, a.kind, m.role, m.principal_member_id, m.answers_course, own.display_name AS owner_name,
       seen.last_used_at AS last_seen_at
FROM course_member m
JOIN actor a ON a.id = m.actor_id
LEFT JOIN actor own ON own.id = a.owner_actor_id
LEFT JOIN course_member p ON p.id = m.principal_member_id
LEFT JOIN LATERAL (
    SELECT cr.last_used_at FROM credential cr
    WHERE a.kind = 'agent' AND cr.actor_id = a.id AND cr.kind = 'api_token' AND cr.revoked_at IS NULL
      AND cr.last_used_at IS NOT NULL AND (cr.expires_at IS NULL OR cr.expires_at > sqlc.arg(now))
    ORDER BY cr.last_used_at DESC LIMIT 1) seen ON true
WHERE m.course_id = $1 AND m.id <> sqlc.arg(caller_member_id)
  AND m.status = 'active' AND (m.expires_at IS NULL OR m.expires_at > sqlc.arg(now))
  AND m.perm_conversation_answer <> 'denied' AND a.status = 'active'
  AND (m.principal_member_id IS NULL OR m.principal_member_id = sqlc.arg(caller_member_id)
       OR (m.answers_course AND p.perm_member_manage <> 'denied'))
  AND a.kind = 'agent'
  AND (a.owner_actor_id IS NULL OR EXISTS (SELECT 1 FROM actor o WHERE o.id = a.owner_actor_id AND o.status = 'active'))
  AND EXISTS (SELECT 1 FROM credential sc
               WHERE sc.id = a.site_chat_credential_id AND sc.actor_id = a.id AND sc.revoked_at IS NULL
                 AND (sc.expires_at IS NULL OR sc.expires_at > sqlc.arg(now)))
ORDER BY m.id
LIMIT sqlc.arg(max_rows);

-- name: ListStudentScopesOf :many
SELECT member_id, student_member_id FROM member_student_scope WHERE member_id = ANY(sqlc.arg(member_ids)::uuid[]);

-- name: ListAssignmentScopesOf :many
SELECT member_id, assignment_id FROM member_assignment_scope WHERE member_id = ANY(sqlc.arg(member_ids)::uuid[]);

-- Exporting conversations for audit (docs/schema.md §2.8, Exporting
-- conversations for audit). An administrator's export reads the
-- conversations its filters choose, whoever took part in them and whatever
-- anyone may read of them now: a retracted message with its text, marked
-- retracted, and the answers and questions proposed in them that were never
-- posted. Who may export what is the Admin gate's, in Go; these queries
-- read what an export holds, as of the moment it was made.
--
-- The filters, the same in each query that chooses conversations: a course
-- (course_id), or a department and everything beneath it (within_dept_id);
-- one person or agent taking part, as the opener or the respondent
-- (participant_actor_id); and a span of time (from_at, before_at), which
-- keeps the conversations opened in it, or with a message written or an
-- answer or question proposed in it, and of those, what was written and
-- proposed in it. as_of is when the export was made: nothing written after
-- it is in it.

-- name: ExportSize :one
-- How much an export would hold: its conversations, their messages and the
-- answers and questions proposed in them, each with the bytes of its text.
WITH RECURSIVE within (id, n) AS (
    SELECT d.id, 1 FROM department d WHERE d.id = sqlc.narg(within_dept_id)::uuid
  UNION ALL
    SELECT d.id, within.n + 1 FROM department d JOIN within ON d.parent_id = within.id WHERE within.n < 16
), chosen AS (
    SELECT c.id
    FROM conversation c
    JOIN course co ON co.id = c.course_id
    JOIN course_member o ON o.id = c.opener_member_id
    JOIN course_member r ON r.id = c.respondent_member_id
    WHERE c.created_at <= sqlc.arg(as_of)
      AND (sqlc.narg(course_id)::uuid IS NULL OR c.course_id = sqlc.narg(course_id)::uuid)
      AND (sqlc.narg(within_dept_id)::uuid IS NULL OR co.dept_id IN (SELECT id FROM within))
      AND (sqlc.narg(participant_actor_id)::uuid IS NULL
           OR o.actor_id = sqlc.narg(participant_actor_id)::uuid OR r.actor_id = sqlc.narg(participant_actor_id)::uuid)
      AND ((c.created_at >= coalesce(sqlc.narg(from_at)::timestamptz, '-infinity')
            AND c.created_at < coalesce(sqlc.narg(before_at)::timestamptz, 'infinity'))
           OR c.id IN (SELECT m.conversation_id FROM conversation_message m
                       WHERE m.created_at >= coalesce(sqlc.narg(from_at)::timestamptz, '-infinity')
                         AND m.created_at < coalesce(sqlc.narg(before_at)::timestamptz, 'infinity')
                         AND m.created_at <= sqlc.arg(as_of))
           OR EXISTS (SELECT 1 FROM action a
                      WHERE a.target_type = 'conversation' AND a.target_id = c.id
                        AND a.action_type IN ('conversation.answer', 'conversation.ask')
                        AND a.status IN ('proposed', 'rejected', 'changes_requested', 'cancelled')
                        AND a.created_at >= coalesce(sqlc.narg(from_at)::timestamptz, '-infinity')
                        AND a.created_at < coalesce(sqlc.narg(before_at)::timestamptz, 'infinity')
                        AND a.created_at <= sqlc.arg(as_of)))
)
SELECT (SELECT count(*) FROM chosen)::bigint AS conversations,
       w.messages, w.message_bytes, p.proposals, p.proposal_bytes
FROM (SELECT count(*)::bigint AS messages, coalesce(sum(octet_length(m.body)), 0)::bigint AS message_bytes
      FROM conversation_message m
      WHERE m.conversation_id IN (SELECT id FROM chosen)
        AND m.created_at >= coalesce(sqlc.narg(from_at)::timestamptz, '-infinity')
        AND m.created_at < coalesce(sqlc.narg(before_at)::timestamptz, 'infinity')
        AND m.created_at <= sqlc.arg(as_of)) w,
     (SELECT count(*)::bigint AS proposals, coalesce(sum(octet_length(a.payload->>'body')), 0)::bigint AS proposal_bytes
      FROM action a
      WHERE a.target_type = 'conversation' AND a.target_id IN (SELECT id FROM chosen)
        AND a.action_type IN ('conversation.answer', 'conversation.ask') AND a.status IN ('proposed', 'rejected', 'changes_requested', 'cancelled')
        AND a.created_at >= coalesce(sqlc.narg(from_at)::timestamptz, '-infinity')
        AND a.created_at < coalesce(sqlc.narg(before_at)::timestamptz, 'infinity')
        AND a.created_at <= sqlc.arg(as_of)) p;

-- name: ListExportConversations :many
-- The conversations an export holds, paged by id, each with its course, its
-- two participants — who they are, and for an agent someone owns, who owns
-- it — and, once it is closed, when: the news of its closing says, however
-- it was closed.
WITH RECURSIVE within (id, n) AS (
    SELECT d.id, 1 FROM department d WHERE d.id = sqlc.narg(within_dept_id)::uuid
  UNION ALL
    SELECT d.id, within.n + 1 FROM department d JOIN within ON d.parent_id = within.id WHERE within.n < 16
)
SELECT c.id, c.course_id, co.code AS course_code, co.section AS course_section, co.title AS course_title,
       co.dept_id AS course_dept_id, co.term_id AS course_term_id,
       c.title, c.status, c.closed_reason, c.created_at, c.last_message_at, closed.occurred_at AS closed_at,
       c.opener_member_id, o.actor_id AS opener_actor_id, oa.display_name AS opener_name, oa.kind AS opener_kind,
       o.role AS opener_role,
       c.respondent_member_id, r.actor_id AS respondent_actor_id, ra.display_name AS respondent_name,
       ra.kind AS respondent_kind, r.role AS respondent_role, r.principal_member_id AS respondent_principal_member_id,
       r.answers_course AS respondent_answers_course, ra.owner_actor_id AS respondent_owner_actor_id,
       own.display_name AS respondent_owner_name
FROM conversation c
JOIN course co ON co.id = c.course_id
JOIN course_member o ON o.id = c.opener_member_id
JOIN actor oa ON oa.id = o.actor_id
JOIN course_member r ON r.id = c.respondent_member_id
JOIN actor ra ON ra.id = r.actor_id
LEFT JOIN actor own ON own.id = ra.owner_actor_id
LEFT JOIN event closed ON c.status = 'closed' AND closed.seq = (
    SELECT e.seq FROM event e
    WHERE e.subject_type = 'conversation' AND e.subject_id = c.id AND e.type = 'conversation.closed'
    ORDER BY e.seq DESC LIMIT 1)
WHERE c.id > sqlc.arg(after)
  AND c.created_at <= sqlc.arg(as_of)
  AND (sqlc.narg(course_id)::uuid IS NULL OR c.course_id = sqlc.narg(course_id)::uuid)
  AND (sqlc.narg(within_dept_id)::uuid IS NULL OR co.dept_id IN (SELECT id FROM within))
  AND (sqlc.narg(participant_actor_id)::uuid IS NULL
       OR o.actor_id = sqlc.narg(participant_actor_id)::uuid OR r.actor_id = sqlc.narg(participant_actor_id)::uuid)
  AND ((c.created_at >= coalesce(sqlc.narg(from_at)::timestamptz, '-infinity')
        AND c.created_at < coalesce(sqlc.narg(before_at)::timestamptz, 'infinity'))
       OR c.id IN (SELECT m.conversation_id FROM conversation_message m
                   WHERE m.created_at >= coalesce(sqlc.narg(from_at)::timestamptz, '-infinity')
                     AND m.created_at < coalesce(sqlc.narg(before_at)::timestamptz, 'infinity')
                     AND m.created_at <= sqlc.arg(as_of))
       OR EXISTS (SELECT 1 FROM action a
                  WHERE a.target_type = 'conversation' AND a.target_id = c.id
                    AND a.action_type IN ('conversation.answer', 'conversation.ask')
                    AND a.status IN ('proposed', 'rejected', 'changes_requested', 'cancelled')
                    AND a.created_at >= coalesce(sqlc.narg(from_at)::timestamptz, '-infinity')
                    AND a.created_at < coalesce(sqlc.narg(before_at)::timestamptz, 'infinity')
                    AND a.created_at <= sqlc.arg(as_of)))
ORDER BY c.id
LIMIT sqlc.arg(max_rows);

-- name: ListExportMessages :many
-- What was written in the given conversations, in the span of time, as of
-- the export, a page at a time: conversation by conversation, in the order
-- the conversations were listed, and in each in the order it was written.
-- A retracted message comes with its text, as it is kept, and with its
-- retraction: when, by whom and why.
SELECT m.id, m.conversation_id, m.seq, m.author_member_id, am.actor_id AS author_actor_id,
       aa.display_name AS author_name, aa.kind AS author_kind, am.role AS author_role,
       m.in_reply_to_message_id, m.body, m.created_at, m.created_by_action_id, m.sources_stated,
       x.created_at AS retracted_at, x.retracted_by_member_id, xm.actor_id AS retracted_by_actor_id,
       xa.display_name AS retracted_by_name, xa.kind AS retracted_by_kind, xm.role AS retracted_by_role,
       x.reason AS retraction_reason,
       x.created_by_action_id AS retraction_action_id
FROM conversation_message m
JOIN course_member am ON am.id = m.author_member_id
JOIN actor aa ON aa.id = am.actor_id
LEFT JOIN conversation_message_retraction x ON x.message_id = m.id AND x.created_at <= sqlc.arg(as_of)
LEFT JOIN course_member xm ON xm.id = x.retracted_by_member_id
LEFT JOIN actor xa ON xa.id = xm.actor_id
WHERE m.conversation_id = ANY(sqlc.arg(conversation_ids)::uuid[])
  AND (m.conversation_id, m.seq) > (sqlc.arg(after_conversation_id)::uuid, sqlc.arg(after_seq)::int)
  AND m.created_at >= coalesce(sqlc.narg(from_at)::timestamptz, '-infinity')
  AND m.created_at < coalesce(sqlc.narg(before_at)::timestamptz, 'infinity')
  AND m.created_at <= sqlc.arg(as_of)
ORDER BY m.conversation_id, m.seq
LIMIT sqlc.arg(max_rows);

-- name: ListExportProposals :many
-- The answers and questions proposed in the given conversations, in the
-- span of time, as of the export, that were never posted: waiting for a
-- decision, rejected, sent back for changes, or cancelled (withdrawn,
-- expired, or their proposer's seat gone). What each said is its payload's;
-- of the files it named, their names alone, never the upload tokens it
-- names them by. Why it was rejected, what to change, or why it was
-- cancelled is its result's. An answer's sources are as it named them, ids
-- alone; null when it did not say.
SELECT a.id, a.target_id AS conversation_id, a.action_type, a.status, a.created_at,
       a.member_id AS proposer_member_id, pm.actor_id AS proposer_actor_id, pa.display_name AS proposer_name,
       pa.kind AS proposer_kind, pm.role AS proposer_role,
       a.decided_at, a.decided_by_member_id, dm.actor_id AS decided_by_actor_id, da.display_name AS decided_by_name,
       da.kind AS decided_by_kind, dm.role AS decided_by_role,
       coalesce(a.payload->>'body', '')::text AS body,
       coalesce(a.payload->>'in_reply_to_message_id', '')::text AS in_reply_to_message_id,
       coalesce((SELECT array_agg(coalesce(f.value->>'filename', '') ORDER BY f.ordinality)
                 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(a.payload->'attachments') = 'array'
                                                THEN a.payload->'attachments' ELSE '[]'::jsonb END) WITH ORDINALITY f),
                '{}')::text[] AS attachment_filenames,
       (CASE WHEN jsonb_typeof(a.payload->'sources') = 'array' THEN a.payload->'sources' ELSE 'null'::jsonb END)::jsonb AS sources,
       coalesce(a.result->'decision'->>'reason', a.result->'error'->'details'->>'reason', '')::text AS reason
FROM action a
LEFT JOIN course_member pm ON pm.id = a.member_id
LEFT JOIN actor pa ON pa.id = pm.actor_id
LEFT JOIN course_member dm ON dm.id = a.decided_by_member_id
LEFT JOIN actor da ON da.id = dm.actor_id
WHERE a.target_type = 'conversation' AND a.target_id = ANY(sqlc.arg(conversation_ids)::uuid[])
  AND a.action_type IN ('conversation.answer', 'conversation.ask') AND a.status IN ('proposed', 'rejected', 'changes_requested', 'cancelled')
  AND a.created_at >= coalesce(sqlc.narg(from_at)::timestamptz, '-infinity')
  AND a.created_at < coalesce(sqlc.narg(before_at)::timestamptz, 'infinity')
  AND a.created_at <= sqlc.arg(as_of)
ORDER BY a.target_id, a.id;

-- name: GetExport :one
-- An export, by its id, which is its action's: who made it, over what, and
-- when it was carried out.
SELECT id, actor_id, payload, status, executed_at, result
FROM action
WHERE id = $1 AND action_type = 'conversation.export';

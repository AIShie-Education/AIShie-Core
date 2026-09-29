-- name: LockEventStream :exec
-- Held until the transaction ends. See events.Flush for why.
SELECT pg_advisory_xact_lock(sqlc.arg(namespace)::int4, sqlc.arg(stream)::int4);

-- name: InsertEvent :one
INSERT INTO event (type, course_id, action_id, subject_type, subject_id, student_member_id, assignment_id, payload)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING seq;

-- name: NotifyWake :exec
-- Tells every Core listening on the channel (package wake) what this
-- transaction wrote, once it commits: PostgreSQL sends a notification only
-- then, and never for a transaction, or a savepoint, rolled back. One per row
-- given: a course, the type of the event, its seq, and, for news of a
-- conversation (conversation_ids) or of a proposal to write in one
-- (action_ids, whose target is the conversation), the conversation and its
-- two participants. The nil UUID stands for none. Each is a few hundred
-- bytes, well under the 8000 a notification may carry.
SELECT pg_notify(sqlc.arg(channel)::text, json_strip_nulls(json_build_object(
         'course_id', k.course_id, 'kind', t.kind, 'seq', s.seq,
         'conversation_id', c.id, 'opener_member_id', c.opener_member_id,
         'respondent_member_id', c.respondent_member_id))::text)
FROM unnest(sqlc.arg(course_ids)::uuid[]) WITH ORDINALITY AS k(course_id, n)
JOIN unnest(sqlc.arg(kinds)::text[]) WITH ORDINALITY AS t(kind, n) ON t.n = k.n
JOIN unnest(sqlc.arg(seqs)::bigint[]) WITH ORDINALITY AS s(seq, n) ON s.n = k.n
JOIN unnest(sqlc.arg(conversation_ids)::uuid[]) WITH ORDINALITY AS v(conversation_id, n) ON v.n = k.n
JOIN unnest(sqlc.arg(action_ids)::uuid[]) WITH ORDINALITY AS x(action_id, n) ON x.n = k.n
LEFT JOIN action a ON a.id = x.action_id AND a.target_type = 'conversation'
LEFT JOIN conversation c
       ON c.id = coalesce(nullif(v.conversation_id, '00000000-0000-0000-0000-000000000000'::uuid), a.target_id)
ORDER BY k.n;

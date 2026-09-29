-- An answer's draft while it is written (docs/schema.md §2.8, Drafts): one
-- row per conversation in an UNLOGGED table, written by conversation.draft,
-- which records no action. A row written before fresh_after, or stale_before,
-- is no draft: reads leave it out, a write replaces it whatever it held, and
-- the sweep deletes it.

-- name: LockConversationForDraft :one
-- The conversation a draft is written in, held FOR SHARE until the write
-- ends: writing a message (TouchConversation), closing it, and proposing an
-- answer in it (LockConversationForAnswer) each take a lock this waits for,
-- or that waits for it. So a draft written after the answer is posted,
-- proposed or the conversation closed, finds it so (ConversationAwaitsAnswer,
-- asked after this), and one written before is deleted with them.
SELECT id, course_id, opener_member_id, respondent_member_id
FROM conversation
WHERE id = $1 AND course_id = $2
FOR SHARE;

-- name: ConversationAwaitsAnswer :one
-- Whether a conversation stands awaiting_answer, as the views say
-- (tools.conversationViews): open, the opener wrote last, and no answer of
-- the respondent's to the opener's newest message waits for a decision.
-- Asked after LockConversationForDraft, so that what that lock waited for
-- is seen.
SELECT (c.status = 'open' AND c.last_author_member_id = c.opener_member_id
        AND NOT EXISTS (
            SELECT 1 FROM action a
            WHERE a.target_type = 'conversation' AND a.target_id = c.id AND a.action_type = 'conversation.answer'
              AND a.status = 'proposed' AND a.member_id = c.respondent_member_id
              AND a.payload->>'in_reply_to_message_id' = (
                  SELECT m.id::text FROM conversation_message m
                  WHERE m.conversation_id = c.id AND m.author_member_id = c.opener_member_id
                  ORDER BY m.seq DESC LIMIT 1)))::bool AS awaiting
FROM conversation c
WHERE c.id = $1;

-- name: LockConversationForAnswer :exec
-- Proposing an answer takes the conversation as writing a message does,
-- FOR NO KEY UPDATE, so that a draft being written waits for the proposal,
-- and finds the answer waiting for approval, or the proposal waits for the
-- draft and deletes it (DeleteDraft).
SELECT 1 FROM conversation WHERE id = $1 FOR NO KEY UPDATE;

-- name: PutDraft :one
-- Writes a draft if it is newer than the one kept, and returns its version;
-- no row when it is passed over. Newer is: none kept, or one gone stale; one of another attempt,
-- which a new attempt replaces, unless this is the end (done) of an attempt
-- that is not the one kept; or, of the same attempt, not ended, a higher
-- version, or for its end, not a lower one. The end of an attempt keeps
-- the row, empty, so that a write of it that comes late is passed over.
-- body and steps given null keep what the same attempt had, and are none
-- for a new one. Under the row's lock, so that two writes at once are
-- ordered by it.
INSERT INTO conversation_draft AS d (conversation_id, course_id, attempt, version, body, steps, done, updated_at)
VALUES (sqlc.arg(conversation_id), sqlc.arg(course_id), sqlc.arg(attempt), sqlc.arg(version),
        CASE WHEN sqlc.arg(done)::bool THEN NULL ELSE sqlc.narg(body)::text END,
        CASE WHEN sqlc.arg(done)::bool THEN '[]'::jsonb ELSE coalesce(sqlc.narg(steps)::jsonb, '[]'::jsonb) END,
        sqlc.arg(done)::bool, sqlc.arg(at))
ON CONFLICT (conversation_id) DO UPDATE
   SET attempt    = EXCLUDED.attempt,
       version    = EXCLUDED.version,
       done       = EXCLUDED.done,
       updated_at = EXCLUDED.updated_at,
       body  = CASE WHEN EXCLUDED.done THEN NULL
                    WHEN sqlc.narg(body)::text IS NULL AND d.attempt = EXCLUDED.attempt AND d.updated_at >= sqlc.arg(stale_before)
                         THEN d.body
                    ELSE EXCLUDED.body END,
       steps = CASE WHEN EXCLUDED.done THEN '[]'::jsonb
                    WHEN sqlc.narg(steps)::jsonb IS NULL AND d.attempt = EXCLUDED.attempt AND d.updated_at >= sqlc.arg(stale_before)
                         THEN d.steps
                    ELSE EXCLUDED.steps END
 WHERE d.updated_at < sqlc.arg(stale_before)
    OR (d.attempt <> EXCLUDED.attempt AND NOT EXCLUDED.done)
    OR (d.attempt = EXCLUDED.attempt AND NOT d.done
        AND (d.version < EXCLUDED.version OR (EXCLUDED.done AND d.version <= EXCLUDED.version)))
RETURNING d.version;

-- name: DraftVersion :one
-- The version of the draft kept, as a reader finds it (GetDraft), for a
-- write passed over: 0 for none, one gone stale, or an attempt's end.
SELECT coalesce((SELECT d.version FROM conversation_draft d
                 WHERE d.conversation_id = sqlc.arg(conversation_id) AND NOT d.done
                   AND d.updated_at >= sqlc.arg(fresh_after)), 0)::bigint AS version;

-- name: GetDraft :one
-- The draft of a conversation, if there is one: written since fresh_after,
-- and not the end of its attempt.
SELECT attempt, version, body, steps, updated_at
FROM conversation_draft
WHERE conversation_id = $1 AND NOT done AND updated_at >= sqlc.arg(fresh_after);

-- name: DeleteDraft :exec
-- The respondent's answer is posted, or proposed, or the conversation
-- closed: in the same transaction, its draft is gone.
DELETE FROM conversation_draft WHERE conversation_id = $1;

-- name: DeleteDrafts :exec
-- DeleteDraft for the conversations a removal of seats closed.
DELETE FROM conversation_draft WHERE conversation_id = ANY(sqlc.arg(conversation_ids)::uuid[]);

-- name: DeleteStaleDrafts :execrows
-- The sweep: drafts nobody has written for a while, which reads leave out
-- already. The table is a row per conversation being answered, and scanned.
DELETE FROM conversation_draft WHERE updated_at < sqlc.arg(stale_before);

-- Attachments (docs/schema.md §2.8, Attachments): the files a message of a
-- conversation carries, written with it. Who may read them is who may read
-- the message, which is decided in Go (tools.addressing.mayRead); a
-- retracted message's files are withheld from its readers as its text is.

-- name: InsertConversationAttachment :exec
-- One file of a message, written in the message's transaction and dated as
-- the message is (conversation_attachment_with_its_message).
INSERT INTO conversation_attachment (id, message_id, conversation_id, course_id, position, filename, storage_key,
                                     content_type, byte_size, checksum, created_at)
VALUES (sqlc.arg(id), sqlc.arg(message_id), sqlc.arg(conversation_id), sqlc.arg(course_id), sqlc.arg(position),
        sqlc.arg(filename), sqlc.arg(storage_key), sqlc.arg(content_type), sqlc.arg(byte_size), sqlc.narg(checksum),
        sqlc.arg(created_at));

-- name: ConversationAttachmentBytes :one
-- What a conversation holds in files: every message's, a retracted one's
-- included, since its files are kept. Asked under the conversation's row
-- lock, which every message is written under, so that two messages at once
-- are held to the limit together.
SELECT coalesce(sum(byte_size), 0)::bigint AS bytes
FROM conversation_attachment
WHERE conversation_id = $1;

-- name: ListMessageAttachments :many
-- The files of the given messages, each message's in order. What a message
-- view shows of them: never where they are kept.
SELECT id, message_id, position, filename, content_type, byte_size, checksum, created_at
FROM conversation_attachment
WHERE message_id = ANY(sqlc.arg(message_ids)::uuid[])
ORDER BY message_id, position;

-- name: GetConversationAttachment :one
-- One file, with its message's author, whether the message is retracted,
-- and where the file is kept, for conversation.attachment to hand out a URL
-- for, once it has decided that the caller may read the message.
SELECT a.id, a.message_id, a.conversation_id, a.position, a.filename, a.storage_key, a.content_type, a.byte_size,
       a.checksum, a.created_at, m.author_member_id, m.seq AS message_seq,
       (r.message_id IS NOT NULL)::bool AS retracted
FROM conversation_attachment a
JOIN conversation_message m ON m.id = a.message_id
LEFT JOIN conversation_message_retraction r ON r.message_id = a.message_id
WHERE a.id = $1 AND a.course_id = $2;

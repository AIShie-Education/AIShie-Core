-- Agents' memory (docs/schema.md §2.9). What an agent may reach of it is
-- decided in Go (tools.memoryAccess), from its seats as authorization reads
-- them; these queries read and write one agent's entries, a bucket at a
-- time, and never give an agent a frozen entry (purge_after set). Entries
-- are deleted, not retired: what is forgotten is gone.
--
-- The reads name their columns rather than taking the row: the search
-- vector is the database's to use, never the application's to read.

-- name: LockMemoryBucket :exec
-- Writes to one bucket are counted one at a time: each takes this before it
-- counts, to the end of its transaction (memory.LockKey).
SELECT pg_advisory_xact_lock(sqlc.arg(namespace)::int4, sqlc.arg(key)::int4);

-- name: CountMemoryBucket :one
-- What one bucket holds that its agent can still read: in force, waiting
-- for review, turned down, and how many of the first two are pinned.
SELECT count(*) FILTER (WHERE status = 'active')                               AS active,
       count(*) FILTER (WHERE status = 'proposed')                             AS proposed,
       count(*) FILTER (WHERE status = 'rejected')                             AS rejected,
       count(*) FILTER (WHERE pinned AND status IN ('active', 'proposed'))     AS pinned
FROM memory_entry
WHERE holder_actor_id = $1 AND bucket = $2 AND purge_after IS NULL;

-- name: CountMemoryOfHolder :one
-- Everything one agent holds, frozen and turned down included.
SELECT count(*) FROM memory_entry WHERE holder_actor_id = $1;

-- name: InsertMemory :one
-- A new entry, unless the same text is already live in its bucket: then no
-- row comes back, and GetMemoryByHash finds the one that is there.
INSERT INTO memory_entry (id, holder_actor_id, scope, course_id, holder_member_id, subject_actor_id, subject_member_id,
                          status, body, search_text, text_hash, tags, pinned, source, replaces_id,
                          created_by_actor_id, created_by_action_id, updated_by_actor_id, updated_by_action_id,
                          created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(holder_actor_id), sqlc.arg(scope), sqlc.narg(course_id), sqlc.narg(holder_member_id),
        sqlc.narg(subject_actor_id), sqlc.narg(subject_member_id), sqlc.arg(status), sqlc.arg(body), sqlc.arg(search_text),
        sqlc.arg(text_hash), sqlc.arg(tags), sqlc.arg(pinned), sqlc.arg(source), sqlc.narg(replaces_id),
        sqlc.arg(actor_id), sqlc.arg(action_id), sqlc.arg(actor_id), sqlc.arg(action_id), sqlc.arg(at), sqlc.arg(at))
ON CONFLICT (holder_actor_id, bucket, text_hash) WHERE status IN ('active', 'proposed') DO NOTHING
RETURNING id;

-- name: GetMemoryByHash :one
-- The live entry of a bucket that holds this text.
SELECT id, holder_actor_id, scope, course_id, holder_member_id, subject_actor_id, subject_member_id, bucket, status,
       body, tags, pinned, source, version, replaces_id, created_at, updated_at, decided_at, decision_reason, purge_after
FROM memory_entry
WHERE holder_actor_id = $1 AND bucket = $2 AND text_hash = $3 AND status IN ('active', 'proposed');

-- name: GetMemory :one
SELECT id, holder_actor_id, scope, course_id, holder_member_id, subject_actor_id, subject_member_id, bucket, status,
       body, tags, pinned, source, version, replaces_id, created_at, updated_at, decided_at, decision_reason, purge_after
FROM memory_entry
WHERE id = $1;

-- name: GetMemoryForUpdate :one
-- The same, locked, by a change to it, which takes its bucket's lock first.
SELECT id, holder_actor_id, scope, course_id, holder_member_id, subject_actor_id, subject_member_id, bucket, status,
       body, tags, pinned, source, version, replaces_id, created_at, updated_at, decided_at, decision_reason, purge_after
FROM memory_entry
WHERE id = $1
FOR UPDATE;

-- name: UpdateMemoryBody :one
-- A change to an entry's text, tags or pin, which moves its version on.
-- Given a version, only that version is changed: no row comes back if it
-- has moved on since (0 changes whatever it is).
UPDATE memory_entry
SET body = sqlc.arg(body), search_text = sqlc.arg(search_text), text_hash = sqlc.arg(text_hash), tags = sqlc.arg(tags),
    pinned = sqlc.arg(pinned), source = sqlc.arg(source), version = version + 1,
    updated_by_actor_id = sqlc.arg(actor_id), updated_by_action_id = sqlc.arg(action_id), updated_at = sqlc.arg(at)
WHERE id = sqlc.arg(id) AND (sqlc.arg(version)::int = 0 OR version = sqlc.arg(version)::int)
RETURNING version;

-- name: DeleteMemory :execrows
DELETE FROM memory_entry WHERE id = $1 AND holder_actor_id = $2;

-- name: SearchMemory :many
-- An agent's entries in force in the given buckets, pinned first, then by
-- how well they match query (a to_tsquery argument, memory.QueryTerms) with
-- a term for recency that halves at 30 days, so that with no query, or no
-- match, the newest come first. only_matches keeps what matches, and what
-- is pinned.
SELECT m.id, m.holder_actor_id, m.scope, m.course_id, m.holder_member_id, m.subject_actor_id, m.subject_member_id, m.bucket,
       m.status, m.body, m.tags, m.pinned, m.source, m.version, m.replaces_id, m.created_at, m.updated_at, m.decided_at,
       m.decision_reason, m.purge_after
FROM memory_entry m
CROSS JOIN (SELECT CASE WHEN sqlc.arg(query)::text = '' THEN NULL
                        ELSE to_tsquery('simple', sqlc.arg(query)::text) END AS q) x
WHERE m.holder_actor_id = sqlc.arg(holder_actor_id) AND m.bucket = ANY(sqlc.arg(buckets)::text[])
  AND m.status = 'active' AND m.purge_after IS NULL
  AND (NOT sqlc.arg(only_matches)::bool OR m.pinned OR m.search @@ x.q)
ORDER BY m.pinned DESC,
         coalesce(ts_rank_cd(m.search, x.q), 0)
           + 0.3 / (1 + greatest(extract(epoch FROM sqlc.arg(now)::timestamptz - m.updated_at), 0) / 2592000.0) DESC,
         m.updated_at DESC, m.id DESC
LIMIT sqlc.arg(max_rows);

-- name: ListMemoryBucket :many
-- A page of one bucket in one status, by id: newest first, after the last
-- id seen, or oldest first.
SELECT id, holder_actor_id, scope, course_id, holder_member_id, subject_actor_id, subject_member_id, bucket, status,
       body, tags, pinned, source, version, replaces_id, created_at, updated_at, decided_at, decision_reason, purge_after
FROM memory_entry
WHERE holder_actor_id = sqlc.arg(holder_actor_id) AND bucket = sqlc.arg(bucket) AND status = sqlc.arg(status)
  AND purge_after IS NULL
  AND (sqlc.narg(after)::uuid IS NULL
       OR (sqlc.arg(newest)::bool AND id < sqlc.narg(after)::uuid)
       OR (NOT sqlc.arg(newest)::bool AND id > sqlc.narg(after)::uuid))
ORDER BY CASE WHEN sqlc.arg(newest)::bool THEN id END DESC, id
LIMIT sqlc.arg(max_rows);

-- name: MemoryEnabled :one
-- Whether an agent's owner lets it keep memory: no row is yes.
SELECT coalesce((SELECT enabled FROM memory_setting WHERE holder_actor_id = $1), true)::bool AS enabled;

-- name: CountMemoryWrites :one
-- An agent's writes in the hour that starts at hour, and in the day that
-- ends with it, with the oldest hour that day's count still holds.
SELECT coalesce(sum(n) FILTER (WHERE hour = sqlc.arg(hour)::timestamptz), 0)::int AS this_hour,
       coalesce(sum(n), 0)::int                                                 AS this_day,
       coalesce(min(hour), sqlc.arg(hour)::timestamptz)::timestamptz            AS oldest_hour
FROM memory_write_count
WHERE holder_actor_id = sqlc.arg(holder_actor_id) AND hour > sqlc.arg(hour)::timestamptz - interval '24 hours';

-- name: AddMemoryWrite :exec
INSERT INTO memory_write_count (holder_actor_id, hour, n) VALUES ($1, $2, 1)
ON CONFLICT (holder_actor_id, hour) DO UPDATE SET n = memory_write_count.n + 1;

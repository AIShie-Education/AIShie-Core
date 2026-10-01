-- AIshie Core — migration 0024 (up)
-- What exporting conversations for audit needs of the database. Design
-- reference: docs/schema.md §2.8 (Exporting conversations for audit).
-- PostgreSQL 13+.
--
-- An administrator exports the conversations of a course, of a department
-- and everything beneath it, or of the whole site (conversation.export),
-- narrowed, if they like, to what was written in a span of time. The export
-- is an action like any other: who made it, when, over what and how much it
-- held are its row in action, and its files are kept in the file store, not
-- here. What it asks of the database is to find the messages written in a
-- span of time without reading every message of the site, which the
-- indexes of conversation_message, all by conversation, cannot do; this
-- index does.
--
-- Nothing is written or changed. The previous release keeps working while
-- this goes in and after a rollback: it never asks for messages by when
-- they were written, and an index it does not use costs it a little on
-- each message it writes, nothing more.

BEGIN;

-- Waits for a busy table rather than queueing every call behind it; a
-- deploy that times out here is run again. Building the index holds off
-- messages being written while it is built.
SET LOCAL lock_timeout = '10s';

CREATE INDEX conversation_message_created_idx ON conversation_message (created_at);

COMMIT;

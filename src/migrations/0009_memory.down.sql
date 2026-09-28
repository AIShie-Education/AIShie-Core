-- AIshiteru Core — migration 0009 (down)
-- Reverts 0009_memory.up.sql. Every memory entry is lost. The actions that
-- wrote them stay, and never held their text.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TABLE IF EXISTS memory_write_count;
DROP TABLE IF EXISTS memory_setting;
DROP TABLE IF EXISTS memory_entry;

DROP FUNCTION IF EXISTS memory_entry_check();

COMMIT;

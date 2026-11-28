-- AIshie Core — migration 0033 (down)
-- Reverts 0033_document_search_passages.up.sql. The index is derived data:
-- the previous release never reads it, and a later up or background sweep
-- builds it again from the documents and text versions that remain.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TRIGGER IF EXISTS document_version_search_passages_purged ON document_version;
DROP FUNCTION IF EXISTS document_version_purge_search_passages();
DROP TABLE IF EXISTS document_search_passages;

COMMIT;

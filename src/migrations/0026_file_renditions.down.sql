-- AIshie Core — migration 0026 (down)
-- Reverts 0026_file_renditions.up.sql.
--
-- Lost: every rendition, and where each stood. The release before this one
-- neither keeps nor reads them. Their PDFs stay in the file store, under
-- renditions/, which the release before neither reads nor sweeps; migrated
-- up again, every file is queued and converted again, nothing points at
-- the PDFs made before, and the orphan sweep removes them once they are as
-- old as it waits for. Kept: every file, as it is.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TRIGGER IF EXISTS conversation_attachment_rendition_queued ON conversation_attachment;
DROP TRIGGER IF EXISTS document_version_file_rendition_queued ON document_version_file;
DROP TABLE IF EXISTS file_rendition;
DROP FUNCTION IF EXISTS conversation_attachment_queue_rendition();
DROP FUNCTION IF EXISTS document_version_file_queue_rendition();
DROP FUNCTION IF EXISTS file_rendition_guarded();
DROP FUNCTION IF EXISTS file_rendition_convertible(text, text);

COMMIT;

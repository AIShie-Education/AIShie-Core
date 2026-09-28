-- AIshiteru Core — migration 0015 (down)
-- Reverts 0015_flexible_records.up.sql.
--
-- Lost: every override of a computed total (the total as worked out stays),
-- and who purged a document or a version, when and why. A purged version
-- comes back as a version of empty text, since the release before this one
-- holds that every version has text or a file; what was purged stays gone.
-- Kept: every grade and every version, and a purged document, archived.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TRIGGER IF EXISTS document_version_append_only ON document_version;
DROP FUNCTION IF EXISTS document_version_guarded();

ALTER TABLE document_version
    DROP CONSTRAINT IF EXISTS document_version_purged_by_fk,
    DROP CONSTRAINT IF EXISTS document_version_purge_reason_length,
    DROP CONSTRAINT IF EXISTS document_version_purged_empty,
    DROP CONSTRAINT IF EXISTS document_version_purge_whole,
    DROP CONSTRAINT IF EXISTS document_version_has_content;

UPDATE document_version SET body_md = '' WHERE purged_at IS NOT NULL;

ALTER TABLE document_version
    DROP COLUMN IF EXISTS purge_reason,
    DROP COLUMN IF EXISTS purged_by_actor_id,
    DROP COLUMN IF EXISTS purged_at;
ALTER TABLE document_version
    ADD CONSTRAINT document_version_has_content CHECK (body_md IS NOT NULL OR storage_key IS NOT NULL);

CREATE TRIGGER document_version_append_only
    BEFORE UPDATE OR DELETE ON document_version
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

DROP TRIGGER IF EXISTS document_purge_kept ON document;
DROP FUNCTION IF EXISTS document_purge_kept();

ALTER TABLE document
    DROP CONSTRAINT IF EXISTS document_purged_by_fk,
    DROP CONSTRAINT IF EXISTS document_purge_reason_length,
    DROP CONSTRAINT IF EXISTS document_purged_course_level,
    DROP CONSTRAINT IF EXISTS document_purged_archived,
    DROP CONSTRAINT IF EXISTS document_purge_whole,
    DROP COLUMN IF EXISTS purge_reason,
    DROP COLUMN IF EXISTS purged_by_actor_id,
    DROP COLUMN IF EXISTS purged_at;

ALTER TABLE grade
    DROP CONSTRAINT IF EXISTS grade_override_by_fk,
    DROP CONSTRAINT IF EXISTS grade_override_reason_length,
    DROP CONSTRAINT IF EXISTS grade_override_nonneg,
    DROP CONSTRAINT IF EXISTS grade_override_of_computed,
    DROP CONSTRAINT IF EXISTS grade_override_whole,
    DROP COLUMN IF EXISTS overridden_at,
    DROP COLUMN IF EXISTS override_by_member_id,
    DROP COLUMN IF EXISTS override_reason,
    DROP COLUMN IF EXISTS override_score;

COMMIT;

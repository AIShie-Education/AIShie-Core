-- AIshie Core — migration 0033 (up)
-- Full-text search of the course's material, instructions and rubrics.
-- PostgreSQL 13+, with no extension: the application writes search_text using
-- internal/memory.SearchText (NFKC, lower case and CJK bigrams), and the
-- built-in simple text-search configuration indexes it.
--
-- One row is one passage of a version's own Markdown (file_id NULL), or of a
-- file's text version written by the transcription service or staff. Passages
-- are replaced by the application when their source changes, and by the
-- background sweep for versions written before this migration. No permission
-- is copied here: course_materials.search joins the live document, assignment
-- and member-scope rows on every read.

BEGIN;

SET LOCAL lock_timeout = '10s';

CREATE TABLE document_search_passages (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    course_id     uuid        NOT NULL REFERENCES course (id),
    document_id   uuid        NOT NULL,
    version_id    uuid        NOT NULL,
    file_id       uuid        REFERENCES document_version_file (id) ON DELETE CASCADE,
    text_source   text        NOT NULL,
    -- The text version's revision. A version's own body is immutable, at 1.
    text_revision integer     NOT NULL,
    section_kind  text        NOT NULL,
    section_no    integer     NOT NULL DEFAULT 0,
    -- The document.text part, from 1; passages never cross its boundaries.
    part_no       integer     NOT NULL,
    content       text        NOT NULL,
    search_text   text        NOT NULL,
    search        tsvector    GENERATED ALWAYS AS (to_tsvector('simple', search_text)) STORED,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (document_id, course_id) REFERENCES document (id, course_id),
    FOREIGN KEY (version_id, document_id) REFERENCES document_version (id, document_id),
    CONSTRAINT document_search_source_valid CHECK (text_source IN ('body_md', 'ai', 'staff')),
    CONSTRAINT document_search_file_matches_source CHECK ((text_source = 'body_md') = (file_id IS NULL)),
    CONSTRAINT document_search_revision_valid CHECK (text_revision >= 1),
    CONSTRAINT document_search_section_valid CHECK (section_kind IN ('body', 'page', 'slide')),
    CONSTRAINT document_search_section_no_valid CHECK (section_no >= 0),
    CONSTRAINT document_search_part_valid CHECK (part_no >= 1),
    CONSTRAINT document_search_content_present CHECK (content <> '')
);

-- The FTS lookup, course/document/version selection, and backfill's revision
-- comparison respectively. All are built-in PostgreSQL index types.
CREATE INDEX document_search_passages_search_idx ON document_search_passages USING gin (search);
CREATE INDEX document_search_passages_document_idx ON document_search_passages (course_id, document_id, version_id);
CREATE INDEX document_search_passages_file_idx ON document_search_passages (version_id, file_id);

-- A purged version's body has no file whose cascade could remove it, and its
-- text rows are part of what was purged too. Do this in the same transaction,
-- whichever release purges the version.
CREATE FUNCTION document_version_purge_search_passages() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM document_search_passages WHERE version_id = NEW.id;
    RETURN NULL;
END;
$$;

CREATE TRIGGER document_version_search_passages_purged
    AFTER UPDATE OF purged_at ON document_version
    FOR EACH ROW WHEN (OLD.purged_at IS NULL AND NEW.purged_at IS NOT NULL)
    EXECUTE FUNCTION document_version_purge_search_passages();

COMMIT;

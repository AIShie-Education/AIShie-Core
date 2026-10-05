-- AIshie Core — migration 0032 (up)
-- Peer evaluation within a group. Design reference: docs/schema.md §2.5b
-- (Peer evaluation), §2.7, §4. PostgreSQL 13+.
--
-- A group assignment may have a peer form: members of a group evaluate each
-- other's contribution, by rating each on criteria, or by splitting 100
-- points among them, within a window. Each rater keeps one current sheet
-- per assignment, replaced as a whole while the window is open, the earlier
-- kept as history. Those who grade read every sheet; no student reads
-- another's. Counted at the form's weight, what each member received moves
-- their grade from the group's, recorded on their grade as an adjustment of
-- its own kind, peer, with what it was worked out from (adjust_detail): a
-- teacher's own adjustment, replace or delta, wins over it.
--
-- The release before (0031's) keeps working while this goes in and after a
-- rollback. It knows no peer tables, and deletes an assignment with its
-- peer data by cascade, which the guards let through for an assignment being
-- deleted. A member's grade it writes again carries a peer adjustment on as
-- one of a kind it does not know, by its points, with an empty reason and
-- the nil member as who made it, which the database takes as none: a peer
-- adjustment is the form's, said by nobody (grade_peer_adjustment_unsaid).
-- This release reads such a one as a peer adjustment whose detail is lost,
-- and works it out again when it next writes the member's grade.

BEGIN;

SET LOCAL lock_timeout = '10s';

-- ===========================================================================
-- 1. The form
-- ===========================================================================

-- A rating form's criteria: 1 to 10 of them, each {key, label, description?,
-- weight}, the keys distinct.
CREATE FUNCTION peer_criteria_valid(c jsonb) RETURNS boolean
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE
    e jsonb;
    keys text[] := '{}';
BEGIN
    IF c IS NULL OR jsonb_typeof(c) <> 'array' THEN
        RETURN false;
    END IF;
    IF jsonb_array_length(c) NOT BETWEEN 1 AND 10 THEN
        RETURN false;
    END IF;
    FOR e IN SELECT x FROM jsonb_array_elements(c) AS x LOOP
        IF jsonb_typeof(e) <> 'object' THEN
            RETURN false;
        END IF;
        IF EXISTS (SELECT 1 FROM jsonb_object_keys(e) AS k WHERE k NOT IN ('key', 'label', 'description', 'weight')) THEN
            RETURN false;
        END IF;
        IF jsonb_typeof(e->'key') IS DISTINCT FROM 'string' THEN
            RETURN false;
        END IF;
        IF (e->>'key') !~ '^[a-z0-9_]{1,32}$' OR (e->>'key') = ANY (keys) THEN
            RETURN false;
        END IF;
        IF jsonb_typeof(e->'label') IS DISTINCT FROM 'string' THEN
            RETURN false;
        END IF;
        IF btrim(e->>'label') = '' OR char_length(e->>'label') > 200 THEN
            RETURN false;
        END IF;
        IF e ? 'description' THEN
            IF jsonb_typeof(e->'description') <> 'string' THEN
                RETURN false;
            END IF;
            IF char_length(e->>'description') > 1000 THEN
                RETURN false;
            END IF;
        END IF;
        IF jsonb_typeof(e->'weight') IS DISTINCT FROM 'number' THEN
            RETURN false;
        END IF;
        IF (e->>'weight')::numeric < 0.1 OR (e->>'weight')::numeric > 10 THEN
            RETURN false;
        END IF;
        keys := keys || (e->>'key');
    END LOOP;
    RETURN true;
END;
$$;

-- An assignment's peer form, one at most, only of a group assignment
-- (application rule: the assignment's set is the application's to read as
-- it changes). kind: rating (criteria on a scale) or share (100 points split
-- among those evaluated). opens: on_hand_in (for each group once it has
-- handed work in) or at opens_at; closes_at ends it for every group. weight:
-- the percentage of each member's grade that what they received moves, 0
-- for reference only. share_with_students: none, or own_average (a member's
-- own average from two peers or more, once it has closed). version: counted
-- by every change, for a change made over the version its caller read.
-- enabled false stops new sheets and stops it counting, and keeps what was
-- written.
CREATE TABLE peer_form (
    assignment_id        uuid        PRIMARY KEY,
    course_id            uuid        NOT NULL,
    enabled              boolean     NOT NULL DEFAULT true,
    kind                 text        NOT NULL,
    criteria             jsonb,
    scale_min            integer,
    scale_max            integer,
    self_evaluation      boolean     NOT NULL DEFAULT false,
    opens                text        NOT NULL,
    opens_at             timestamptz,
    closes_at            timestamptz NOT NULL,
    weight               integer     NOT NULL DEFAULT 0,
    share_with_students  text        NOT NULL DEFAULT 'none',
    version              integer     NOT NULL DEFAULT 1,
    created_by_member_id uuid        NOT NULL,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_by_member_id uuid        NOT NULL,
    updated_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (assignment_id, course_id),
    FOREIGN KEY (assignment_id, course_id) REFERENCES assignment (id, course_id) ON DELETE CASCADE,
    FOREIGN KEY (course_id, created_by_member_id) REFERENCES course_member (course_id, id),
    FOREIGN KEY (course_id, updated_by_member_id) REFERENCES course_member (course_id, id),
    CONSTRAINT peer_form_kind_valid CHECK (kind IN ('rating', 'share')),
    CONSTRAINT peer_form_shape CHECK (
        (kind = 'rating' AND scale_min IS NOT NULL AND scale_max IS NOT NULL
         AND scale_min IN (0, 1) AND scale_max > scale_min AND scale_max <= 10 AND peer_criteria_valid(criteria))
        OR (kind = 'share' AND criteria IS NULL AND scale_min IS NULL AND scale_max IS NULL)),
    CONSTRAINT peer_form_opens_valid CHECK (
        opens IN ('on_hand_in', 'at') AND (opens = 'at') = (opens_at IS NOT NULL) AND (opens_at IS NULL OR opens_at < closes_at)),
    CONSTRAINT peer_form_weight_valid CHECK (weight BETWEEN 0 AND 100),
    CONSTRAINT peer_form_share_valid CHECK (share_with_students IN ('none', 'own_average')),
    CONSTRAINT peer_form_version_positive CHECK (version >= 1)
);

-- ===========================================================================
-- 2. The sheets
-- ===========================================================================

-- A rater's sheet: their evaluation of the members of their group's circle
-- for the assignment, and a comment. One current sheet per rater per
-- assignment; a new one supersedes it (superseded_by), the earlier kept.
-- group_id: the group whose circle it was written in.
CREATE TABLE peer_review (
    id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    course_id            uuid        NOT NULL,
    assignment_id        uuid        NOT NULL,
    group_id             uuid        NOT NULL,
    rater_member_id      uuid        NOT NULL,
    comment              text,
    created_by_action_id uuid        NOT NULL REFERENCES action (id),
    created_at           timestamptz NOT NULL DEFAULT now(),
    superseded_by        uuid,
    UNIQUE (id, assignment_id, rater_member_id),
    FOREIGN KEY (assignment_id, course_id) REFERENCES peer_form (assignment_id, course_id) ON DELETE CASCADE,
    FOREIGN KEY (course_id, group_id) REFERENCES course_group (course_id, id),
    FOREIGN KEY (course_id, rater_member_id) REFERENCES course_member (course_id, id),
    CONSTRAINT peer_review_comment_valid CHECK (comment IS NULL OR char_length(comment) <= 2000),
    CONSTRAINT peer_review_not_superseded_by_self CHECK (superseded_by <> id)
);
-- Superseded only by the same rater's newer sheet for the same assignment.
-- Deferred: the old sheet names the new one before it is written, as a
-- regraded grade does, so that there are never two current sheets.
ALTER TABLE peer_review
    ADD CONSTRAINT peer_review_superseded_by_fk FOREIGN KEY (superseded_by, assignment_id, rater_member_id)
        REFERENCES peer_review (id, assignment_id, rater_member_id) DEFERRABLE INITIALLY DEFERRED;
CREATE UNIQUE INDEX peer_review_one_current ON peer_review (assignment_id, rater_member_id) WHERE superseded_by IS NULL;
CREATE INDEX peer_review_assignment_idx ON peer_review (assignment_id, rater_member_id);
CREATE INDEX peer_review_group_idx ON peer_review (group_id);

-- What a sheet gives one member: ratings by criterion key (a rating form) or
-- a share (a share form), and a comment. Written with its sheet, in the same
-- transaction, and never after; dated as it is.
CREATE TABLE peer_review_entry (
    review_id       uuid    NOT NULL REFERENCES peer_review (id) ON DELETE CASCADE,
    course_id       uuid    NOT NULL,
    ratee_member_id uuid    NOT NULL,
    ratings         jsonb,
    share           integer,
    comment         text,
    PRIMARY KEY (review_id, ratee_member_id),
    FOREIGN KEY (course_id, ratee_member_id) REFERENCES course_member (course_id, id),
    CONSTRAINT peer_review_entry_one_kind CHECK (num_nonnulls(ratings, share) = 1),
    CONSTRAINT peer_review_entry_ratings_object CHECK (ratings IS NULL OR jsonb_typeof(ratings) = 'object'),
    CONSTRAINT peer_review_entry_share_valid CHECK (share IS NULL OR share BETWEEN 0 AND 100),
    CONSTRAINT peer_review_entry_comment_valid CHECK (comment IS NULL OR char_length(comment) <= 1000)
);

-- The form's shape — its kind, criteria, scale and whether members evaluate
-- themselves — is fixed once a sheet names it: a sheet is read against the
-- shape it was written to. Its dates, weight, sharing and whether it is
-- enabled change. It stays its assignment's, in its course.
CREATE FUNCTION peer_form_check_shape_fixed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.assignment_id, NEW.course_id) IS DISTINCT FROM (OLD.assignment_id, OLD.course_id) THEN
        RAISE EXCEPTION 'peer form of assignment % stays its assignment''s', OLD.assignment_id
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF (NEW.kind, NEW.criteria, NEW.scale_min, NEW.scale_max, NEW.self_evaluation)
       IS DISTINCT FROM (OLD.kind, OLD.criteria, OLD.scale_min, OLD.scale_max, OLD.self_evaluation)
       AND EXISTS (SELECT 1 FROM peer_review r WHERE r.assignment_id = OLD.assignment_id) THEN
        RAISE EXCEPTION 'peer form of assignment % is in use: its kind, criteria, scale and self-evaluation no longer change',
            OLD.assignment_id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER peer_form_shape_fixed
    BEFORE UPDATE ON peer_form
    FOR EACH ROW EXECUTE FUNCTION peer_form_check_shape_fixed();

-- A sheet is written while its form is enabled, and is append-only but for
-- being superseded, once; it goes only with its assignment, deleted for
-- good. Each sheet written names itself, for this transaction, as one whose
-- entries may be written (aishie.peer_reviews_written): its entries are
-- written with it and never after.
CREATE FUNCTION peer_review_check_kept() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    expected peer_review;
BEGIN
    IF TG_OP = 'INSERT' THEN
        -- With no form at all, the foreign key refuses it.
        IF EXISTS (SELECT 1 FROM peer_form f WHERE f.assignment_id = NEW.assignment_id AND NOT f.enabled) THEN
            RAISE EXCEPTION 'peer review %: the assignment''s peer form is switched off', NEW.id
                USING ERRCODE = 'check_violation';
        END IF;
        PERFORM set_config('aishie.peer_reviews_written',
                           coalesce(current_setting('aishie.peer_reviews_written', true), '') || ' ' || NEW.id::text, true);
        RETURN NEW;
    END IF;
    IF TG_OP = 'DELETE' THEN
        IF assignment_being_deleted(OLD.assignment_id) THEN
            RETURN OLD;
        END IF;
        RAISE EXCEPTION 'peer review % is kept: DELETE is not allowed; it goes only with its assignment, deleted for good', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    expected := OLD;
    expected.superseded_by := NEW.superseded_by;
    IF OLD.superseded_by IS NOT NULL OR NEW.superseded_by IS NULL OR NEW IS DISTINCT FROM expected THEN
        RAISE EXCEPTION 'peer review % changes only by being superseded, once', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER peer_review_kept
    BEFORE INSERT OR UPDATE OR DELETE ON peer_review
    FOR EACH ROW EXECUTE FUNCTION peer_review_check_kept();

CREATE TRIGGER peer_review_no_truncate
    BEFORE TRUNCATE ON peer_review
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- An entry is written with its sheet, while it is current, in its course,
-- of the form's kind: a rating of every criterion, an integer on the scale,
-- and of nothing else, or a share. A rater's own entry only where members
-- evaluate themselves. Never changed; deleted only with its sheet.
CREATE FUNCTION peer_review_entry_check_kept() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    sheet peer_review;
    form  peer_form;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'peer review entry of % is kept as written', OLD.review_id
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF TG_OP = 'DELETE' THEN
        SELECT * INTO sheet FROM peer_review WHERE id = OLD.review_id;
        IF NOT FOUND OR assignment_being_deleted(sheet.assignment_id) THEN
            RETURN OLD;
        END IF;
        RAISE EXCEPTION 'peer review entry of % is kept: DELETE is not allowed; it goes only with its sheet', OLD.review_id
            USING ERRCODE = 'restrict_violation';
    END IF;
    SELECT * INTO sheet FROM peer_review WHERE id = NEW.review_id;
    IF NOT FOUND THEN
        RETURN NEW; -- the foreign key refuses it
    END IF;
    IF sheet.superseded_by IS NOT NULL
       OR NOT (sheet.id::text = ANY (string_to_array(btrim(coalesce(current_setting('aishie.peer_reviews_written', true), '')), ' '))) THEN
        RAISE EXCEPTION 'peer review %: its entries are written with it, and never after', sheet.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    NEW.course_id := sheet.course_id;
    SELECT * INTO form FROM peer_form WHERE assignment_id = sheet.assignment_id;
    IF NEW.ratee_member_id = sheet.rater_member_id AND NOT form.self_evaluation THEN
        RAISE EXCEPTION 'peer review %: members do not evaluate themselves on this form', sheet.id
            USING ERRCODE = 'check_violation';
    END IF;
    IF form.kind = 'share' THEN
        IF NEW.share IS NULL THEN
            RAISE EXCEPTION 'peer review %: a share form takes a share', sheet.id
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.ratings IS NULL
       OR (SELECT count(*) FROM jsonb_object_keys(NEW.ratings)) <> jsonb_array_length(form.criteria)
       OR EXISTS (
            SELECT 1
            FROM jsonb_array_elements(form.criteria) AS c,
                 LATERAL (SELECT CASE WHEN jsonb_typeof(NEW.ratings->(c->>'key')) = 'number'
                                      THEN (NEW.ratings->>(c->>'key'))::numeric END AS v) AS r
            WHERE r.v IS NULL OR r.v <> trunc(r.v) OR r.v < form.scale_min OR r.v > form.scale_max) THEN
        RAISE EXCEPTION 'peer review %: a rating form takes a rating of every criterion, a whole number from % to %, and of nothing else',
            sheet.id, form.scale_min, form.scale_max
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER peer_review_entry_kept
    BEFORE INSERT OR UPDATE OR DELETE ON peer_review_entry
    FOR EACH ROW EXECUTE FUNCTION peer_review_entry_check_kept();

CREATE TRIGGER peer_review_entry_no_truncate
    BEFORE TRUNCATE ON peer_review_entry
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- ===========================================================================
-- 3. A member's grade: a peer adjustment
-- ===========================================================================

-- A peer adjustment: what peer evaluation, counted at the form's weight,
-- moves the member's score from the group's (adjust_points, the score less
-- the group's), and what it was worked out from (adjust_detail: {factor,
-- weight, raters, form_version}). It is the form's: no reason, nobody who
-- made it. NOT VALID: every row there is holds an adjustment of 0031's
-- kinds, or none, and no detail.
ALTER TABLE grade ADD COLUMN adjust_detail jsonb;
ALTER TABLE grade
    DROP CONSTRAINT grade_adjust_kind_valid,
    ADD CONSTRAINT grade_adjust_kind_valid CHECK (adjust_kind IS NULL OR adjust_kind IN ('replace', 'delta', 'peer')) NOT VALID,
    ADD CONSTRAINT grade_adjust_peer_unsaid CHECK (
        adjust_kind IS DISTINCT FROM 'peer' OR (adjust_reason IS NULL AND adjust_by_member_id IS NULL)) NOT VALID,
    ADD CONSTRAINT grade_adjust_detail_of_peer CHECK (
        adjust_detail IS NULL OR (adjust_kind = 'peer' AND jsonb_typeof(adjust_detail) = 'object')) NOT VALID;

-- The release before carries a peer adjustment on, as a kind it does not
-- know, with an empty reason and the nil member as who made it: what it
-- holds for "none" of either. The database takes them as none.
CREATE FUNCTION grade_peer_adjustment_unsay() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.adjust_reason = '' THEN
        NEW.adjust_reason := NULL;
    END IF;
    IF NEW.adjust_by_member_id = '00000000-0000-0000-0000-000000000000'::uuid THEN
        NEW.adjust_by_member_id := NULL;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER grade_peer_adjustment_unsaid
    BEFORE INSERT ON grade
    FOR EACH ROW WHEN (NEW.adjust_kind = 'peer')
    EXECUTE FUNCTION grade_peer_adjustment_unsay();

COMMIT;

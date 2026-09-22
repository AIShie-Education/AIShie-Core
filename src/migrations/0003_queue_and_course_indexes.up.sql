-- AIshiteru Core — migration 0003 (up)
-- Two indexes the queries turned out to need. PostgreSQL 13+.

BEGIN;

-- The review queue is what is pending OR escalated (an escalated action still
-- waits for its second reviewer), and it is paginated by id. The index from
-- 0001 named only 'pending', which a query asking for both cannot use at all.
DROP INDEX IF EXISTS action_review_queue_idx;
CREATE INDEX action_review_queue_idx ON action (course_id, id)
    WHERE review_state IN ('pending', 'escalated');

-- submission.list without an assignment or a student named walks the course.
CREATE INDEX submission_course_idx ON submission (course_id, id);

COMMIT;

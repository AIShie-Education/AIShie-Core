-- AIshie Core — before 0030_assignment_deletion.up.sql, in `make db-test-sql`
--
-- What a large site does before the migration (docs/deploying.md, Migration
-- 0030): it builds two of the migration's indexes first, without holding
-- off writes, under the names the migration gives them, so that the
-- migration takes no lock building them. They stay, for the redo and the
-- down after it, which drops them.

\set ON_ERROR_STOP 1
\set QUIET 1
CREATE INDEX CONCURRENTLY IF NOT EXISTS event_assignment_idx ON event (assignment_id) WHERE assignment_id IS NOT NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS action_course_idx ON action (course_id) WHERE course_id IS NOT NULL;

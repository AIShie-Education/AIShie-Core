-- AIshideru Core — migration 0001 (down)
-- Drops everything 0001_init.up.sql creates. Destroys all data.
--
-- DROP TABLE does not fire row triggers, so the append-only guards on
-- document_version and event and the submission freeze do not block this.

BEGIN;

DROP TABLE IF EXISTS
    event,
    grade,
    action,
    submission,
    member_assignment_scope,
    assignment,
    document_version,
    document,
    grade_component,
    member_student_scope,
    course_member,
    permission_preset,
    course,
    credential,
    actor,
    department,
    term
CASCADE;

DROP FUNCTION IF EXISTS reject_mutation();
DROP FUNCTION IF EXISTS submission_reject_change_after_submit();

DROP TYPE IF EXISTS autonomy_level;

COMMIT;

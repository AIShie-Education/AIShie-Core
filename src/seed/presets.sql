-- AIshiteru Core — built-in permission presets
--
-- Policy, not schema: what a student, instructor, TA, observer, tutor agent
-- or grading agent may do by default. Apply after the migrations. Safe to
-- re-run: existing built-ins are left as they are, so local edits survive.
--
-- Columns, in order:
--   document_read, document_read_draft, document_write, rubric_read,
--   assignment_write, submission_read, submission_write, grade_read,
--   grade_submit, grade_post, member_read, member_manage, action_decide

BEGIN;

INSERT INTO permission_preset (
    name, description, role, student_scope, assignment_scope,
    perm_document_read, perm_document_read_draft, perm_document_write, perm_rubric_read,
    perm_assignment_write, perm_submission_read, perm_submission_write, perm_grade_read,
    perm_grade_submit, perm_grade_post, perm_member_read, perm_member_manage, perm_action_decide
) VALUES
    -- A student sees published material and their own work. The application
    -- adds the member_student_scope row pointing at the member itself.
    ('student', 'Reads published material, submits and sees own grades',
     'student', 'listed', 'all',
     'autonomous', 'denied',     'denied',     'denied',
     'denied',     'autonomous', 'autonomous', 'autonomous',
     'denied',     'denied',     'denied',     'denied',     'denied'),

    ('observer', 'Reads published material and the member list; changes nothing',
     'observer', 'all', 'all',
     'autonomous', 'denied',     'denied',     'denied',
     'denied',     'denied',     'denied',     'denied',
     'denied',     'denied',     'autonomous', 'denied',     'denied'),

    -- Grades but does not post: the instructor releases.
    ('ta', 'Reads everything, grades; instructor posts and approves',
     'ta', 'all', 'all',
     'autonomous', 'autonomous', 'denied',     'autonomous',
     'denied',     'autonomous', 'denied',     'autonomous',
     'autonomous', 'denied',     'autonomous', 'denied',     'denied'),

    ('instructor', 'Everything, unsupervised',
     'instructor', 'all', 'all',
     'autonomous', 'autonomous', 'autonomous', 'autonomous',
     'autonomous', 'autonomous', 'autonomous', 'autonomous',
     'autonomous', 'autonomous', 'autonomous', 'autonomous', 'autonomous'),

    -- Agent bound to listed students. Sees their work and grades, all
    -- material; writes nothing. Discussion permissions come with discussion.
    ('tutor', 'Agent: reads material and the listed students'' work and grades',
     'assistant', 'listed', 'all',
     'autonomous', 'denied',     'denied',     'denied',
     'denied',     'autonomous', 'denied',     'autonomous',
     'denied',     'denied',     'denied',     'denied',     'denied'),

    -- Agent bound to listed assignments. Proposes grades; a human approves.
    ('grader', 'Agent: reads material and rubric, proposes grades for the listed assignments',
     'assistant', 'all', 'listed',
     'autonomous', 'denied',     'denied',     'autonomous',
     'denied',     'autonomous', 'denied',     'denied',
     'confirm_required', 'denied', 'denied',   'denied',     'denied')
ON CONFLICT (name) WHERE dept_id IS NULL DO NOTHING;

COMMIT;

-- AIshiteru Core — built-in permission presets
--
-- Policy, not schema: what a student, instructor, TA, observer, tutor agent,
-- grading agent, a person's own agent or a course's question-answering agent
-- may do by default. Apply after the migrations. Safe to re-run: existing
-- built-ins are left as they are, so local edits survive. (Migration 0007
-- gave the built-ins already seeded their levels of the three permissions it
-- added; delegate and course_tutor are inserted here alone. Migration 0013
-- gave those seeded their level of member_invite, as it is here, and 0018
-- denied conversation_answer to those for people, as it is here: a person
-- answers no conversation.)
--
-- Columns, in order:
--   document_read, document_read_draft, document_write, rubric_read,
--   assignment_write, submission_read, submission_write, grade_read,
--   grade_submit, grade_post, member_read, member_manage, action_decide,
--   agent_delegate, conversation_ask, conversation_answer, member_invite

BEGIN;

INSERT INTO permission_preset (
    name, description, role, student_scope, assignment_scope,
    perm_document_read, perm_document_read_draft, perm_document_write, perm_rubric_read,
    perm_assignment_write, perm_submission_read, perm_submission_write, perm_grade_read,
    perm_grade_submit, perm_grade_post, perm_member_read, perm_member_manage, perm_action_decide,
    perm_agent_delegate, perm_conversation_ask, perm_conversation_answer, perm_member_invite
) VALUES
    -- A student sees published material and their own work. The application
    -- adds the member_student_scope row pointing at the member itself.
    ('student', 'Reads published material, submits and sees own grades',
     'student', 'listed', 'all',
     'autonomous', 'denied',     'denied',     'denied',
     'denied',     'autonomous', 'autonomous', 'autonomous',
     'denied',     'denied',     'denied',     'denied',     'denied',
     -- Brings an agent of their own with an instructor's approval; asks.
     'confirm_required', 'autonomous', 'denied',
     'denied'),

    ('observer', 'Reads published material and the member list; changes nothing',
     'observer', 'all', 'all',
     'autonomous', 'denied',     'denied',     'denied',
     'denied',     'denied',     'denied',     'denied',
     'denied',     'denied',     'autonomous', 'denied',     'denied',
     'denied',     'denied',     'denied',
     'denied'),

    -- Grades but does not post: the instructor releases.
    ('ta', 'Reads everything, grades; instructor posts and approves',
     'ta', 'all', 'all',
     'autonomous', 'autonomous', 'denied',     'autonomous',
     'denied',     'autonomous', 'denied',     'autonomous',
     'autonomous', 'denied',     'autonomous', 'denied',     'denied',
     'confirm_required', 'autonomous', 'denied',
     'denied'),

    -- Answers no conversation: conversations are between a person and an
    -- agent, and people talk to people elsewhere.
    ('instructor', 'Everything, unsupervised',
     'instructor', 'all', 'all',
     'autonomous', 'autonomous', 'autonomous', 'autonomous',
     'autonomous', 'autonomous', 'autonomous', 'autonomous',
     'autonomous', 'autonomous', 'autonomous', 'autonomous', 'autonomous',
     'autonomous', 'autonomous', 'denied',
     -- Hands out join links: a seat for whoever scans one.
     'autonomous'),

    -- Agent bound to listed students. Sees their work and grades, all
    -- material; writes nothing but its answers to whoever may address it.
    ('tutor', 'Agent: reads material and the listed students'' work and grades',
     'assistant', 'listed', 'all',
     'autonomous', 'denied',     'denied',     'denied',
     'denied',     'autonomous', 'denied',     'autonomous',
     'denied',     'denied',     'denied',     'denied',     'denied',
     'denied',     'denied',     'autonomous',
     'denied'),

    -- Agent bound to listed assignments. Proposes grades; a human approves.
    ('grader', 'Agent: reads material and rubric, proposes grades for the listed assignments',
     'assistant', 'all', 'listed',
     'autonomous', 'denied',     'denied',     'autonomous',
     'denied',     'autonomous', 'denied',     'denied',
     'confirm_required', 'denied', 'denied',   'denied',     'denied',
     'denied',     'denied',     'denied',
     'denied'),

    -- A person's own agent, seated as their delegate (member.add_delegate).
    -- Reads what its principal may read of the principal's own work: the
    -- application lists the principal's students on it, which for a student
    -- is the student. Answers its principal. Never more than its principal
    -- holds, whatever is set here. No agent preset seats members or hands
    -- out join links: someone who manages members gives an agent that.
    ('delegate', 'Agent: a person''s own assistant; reads material and its principal''s work and grades, answers its principal',
     'assistant', 'listed', 'all',
     'autonomous', 'denied',     'denied',     'denied',
     'denied',     'autonomous', 'denied',     'autonomous',
     'denied',     'denied',     'denied',     'denied',     'denied',
     'denied',     'denied',     'autonomous',
     'denied'),

    -- A course's question-answering agent. Listed for nobody, so it reads
    -- the material and nobody's work: that is what puts it within every
    -- student's seat, so that every student may ask it. Brought in with it
    -- by someone who manages the course's members, a delegate answers the
    -- course (course_member.answers_course), not its principal alone.
    ('course_tutor', 'Agent: answers questions about the course material; reads nobody''s work',
     'assistant', 'listed', 'all',
     'autonomous', 'denied',     'denied',     'denied',
     'denied',     'denied',     'denied',     'denied',
     'denied',     'denied',     'denied',     'denied',     'denied',
     'denied',     'denied',     'autonomous',
     'denied')
ON CONFLICT (name) WHERE dept_id IS NULL DO NOTHING;

COMMIT;

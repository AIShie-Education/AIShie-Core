# src

Database schema for AIshiteru Core.

- Design intent: [../docs/aishiteru-core-concepts.md](../docs/aishiteru-core-concepts.md)
- Data model: [../docs/schema.md](../docs/schema.md)

## Layout

```
src/
  migrations/
    0001_init.up.sql     creates everything (17 tables)
    0001_init.down.sql   drops everything (destroys all data)
    0002_action_replay_and_feed_scope.up.sql
                         what the tool layer needs: action.payload_hash and
                         result, event scope columns, session credentials,
                         the action status CHECKs, the member expiry index
    0002_action_replay_and_feed_scope.down.sql
    0003_queue_and_course_indexes.up.sql
                         the review-queue index as the queue is actually
                         queried (pending or escalated, by id), and a
                         course index on submission
    0003_queue_and_course_indexes.down.sql
    0004_no_credential_for_system_actor.up.sql
                         a trigger refusing to write a credential for the
                         system actor
    0004_no_credential_for_system_actor.down.sql
    0005_invitations.up.sql
                         the invite credential kind: a prefix and an expiry,
                         and one live invitation per actor
    0005_invitations.down.sql
    0006_credential_issuer.up.sql
                         who issued an API token
    0006_credential_issuer.down.sql
    0007_agent_ownership.up.sql
                         agents a person owns and the delegate seats they act
                         from; who suspended an actor; the permissions
                         agent_delegate, conversation_ask and
                         conversation_answer, backfilled by roster role
    0007_agent_ownership.down.sql
                         removes the delegate seats, cancelling their
                         proposals, before it drops what 0007 added
    0008_conversations.up.sql
                         conversations between two members, their messages
                         and retractions: append-only, only the participants
                         write, a closed conversation stays closed
    0008_conversations.down.sql
                         cancels answers waiting for approval, then drops the
                         three tables; the actions that wrote them stay
    0009_memory.up.sql   agents' memory: entries about an agent's owner,
                         about each person who asks it, and a course's
                         shared memory; the owner's switch; writes counted
                         by the hour. Deleted, not retired
    0009_memory.down.sql drops the three tables; every entry is lost, and the
                         actions that wrote them never held their text
    0010_department_admins.up.sql
                         departments as a tree, at most 8 levels deep, never
                         a cycle; their administrators' appointments, kept
                         once ended; in what capacity an action was allowed
    0010_department_admins.down.sql
                         drops the appointments and the capacity, and puts
                         every department back at the top
    0011_site_chat.up.sql
                         which credential of an agent's declared that it
                         takes conversations in the site
    0011_site_chat.down.sql
                         drops it; the agents and their credentials stay
    0012_join_links.up.sql
                         a course's join links, each living ten minutes, as
                         the hash of their tokens; the link a seat was taken
                         through; whose email nobody but its person vouches
                         for
    0012_join_links.down.sql
                         drops the links; the people who registered through
                         them and their seats stay, as ordinary ones
    0013_member_invite.up.sql
                         the permission member_invite, which makes join
                         links: a person's seat at its member_manage level,
                         an agent's denied; presets by role
    0013_member_invite.down.sql
                         drops it from both tables
    0014_agent_owner_and_decisions.up.sql
                         an agent's owner never changes: a trigger refuses
                         any change to it, taking it away or giving one; an
                         agent decides only by proposal: its seats and the
                         presets for agents lowered to confirm_required, and
                         an agent's seat written at no more
    0014_agent_owner_and_decisions.down.sql
                         drops both; every agent keeps its owner, and every
                         seat and preset its levels
    0016_login_ids.up.sql
                         a person's login ID, their student or staff number,
                         unique in any case, never an agent's, and whether
                         anyone vouches for it; a password someone else set,
                         which its person must change
    0016_login_ids.down.sql
                         drops them; a person with a login ID and no email
                         can no longer sign in by password, and a temporary
                         password works as any other
  seed/
    presets.sql          the eight built-in permission presets; safe to re-run
  tests/
    constraints_test.sql checks the database-enforced rules; rolls back
    down/NNNN.before.sql, NNNN.after.sql
                         data for a down migration to go over, and what must
                         hold once it has, run around NNNN's down
  embed.go               compiles migrations/ and seed/ into the server binary
```

The SQL files are the source of truth and stay runnable with plain `psql`, as
below. The server embeds the same files, so `aishiterud migrate up` and
`aishiterud seed` do the same thing without needing the repository.

## Requirements

PostgreSQL 13 or newer. No extensions, no elevated privileges.

## Apply and roll back

```
createdb aishiteru
for f in migrations/*.up.sql; do psql -v ON_ERROR_STOP=1 -d aishiteru -f "$f"; done
psql -v ON_ERROR_STOP=1 -d aishiteru -f seed/presets.sql
```

Roll back in reverse order, newest first:

```
for f in $(ls -r migrations/*.down.sql); do psql -v ON_ERROR_STOP=1 -d aishiteru -f "$f"; done
```

The seed is policy, not schema: it inserts the built-in presets and leaves
any that already exist alone, so local edits to them survive a re-run.

Each script is a single transaction, so a failure leaves the database
untouched. Keep `ON_ERROR_STOP=1`: without it psql carries on after the first
error and buries the real message under a screen of "current transaction is
aborted".

Or, with the server binary (`make build` at the repository root):

```
createdb aishiteru
DATABASE_URL=postgres:///aishiteru bin/aishiterud migrate up
DATABASE_URL=postgres:///aishiteru bin/aishiterud seed
```

`aishiterud` records the applied version in a `schema_migrations` table; psql
does not. A database first built with `psql -f` must be adopted once before
the binary will manage it: `aishiterud migrate force N`, N being the number of
the last migration applied by hand (after the loop above, the highest). Pick
one way per database and stay with it.

A migration that fails under `aishiterud` leaves its version recorded as
dirty. Each file applies whole or not at all, but the flag is also left when
a file committed and its version was never recorded, so look at the database
to see which. Fix the cause, then `aishiterud migrate force N`, N being the
last migration fully applied (0 if none), and `migrate up` again.

File names follow `NNNN_name.up.sql` / `NNNN_name.down.sql`. Every migration
needs both directions; a test enforces it.

## Test

Use a throwaway database: the test runs in one transaction and rolls back,
but its fixtures use fixed ids.

```
createdb aishiteru_test
for f in migrations/*.up.sql; do psql -v ON_ERROR_STOP=1 -d aishiteru_test -f "$f"; done
psql -X -d aishiteru_test -f tests/constraints_test.sql
dropdb aishiteru_test
```

Each check prints `PASS`. The first failure stops the run with `FAIL` and the
SQLSTATE it expected versus what it got.

`make db-test-sql` at the repository root does all of this against a scratch
database — every migration up, the seed, the checks, the newest two down and
up again over the seeded built-in presets (`tests/redo_builtins.sql` checks
they come through unchanged), every migration down, a check that nothing was
left behind, and up again — and is what CI runs on PostgreSQL 13 and 18. On
the way down, a migration with files in `tests/down/` goes down over the data
its `.before.sql` commits, and its `.after.sql` checks what became of it:
0007 over a delegate with a proposal waiting and a token, 0008 over a
conversation with an answer waiting, 0009 over a tutor's memory of a student
and a proposal to the course's shared memory, 0010 over a tree with an
appointment in force and one ended, 0011 over an agent's site chat declared,
0012 over join links, a person registered through one and seats taken
through it, 0013 over a seat and a preset that hand out links, 0014
over an owned agent and one nobody owns, seated deciding by proposal, whose
owners, and levels, may change again once it is down, and 0016 over login
IDs, one of them a person's own and unchecked, and a temporary password
signed in with.

## What the database enforces

These hold no matter what application code does. With agents writing through
MCP, these are the invariants that survive a bug in the tool layer.

| Rule | Mechanism |
|---|---|
| A submission's assignment and member are in the same course | composite FKs to `assignment(id, course_id)`, `course_member(course_id, id)` |
| A submission grade is for the student who submitted | composite FK to `submission(id, student_member_id)` |
| A grade is superseded only by a grade of the same student | deferred composite FK to `grade(id, student_member_id)` |
| At most one live grade per submission, per (component, student) | partial unique indexes `one_live_submission_grade`, `one_live_component_grade` |
| One live membership per actor per course | partial unique index `course_member_one_live` |
| One root grade component per course | partial unique index `grade_component_one_root` |
| Preset names are unique among built-ins and within a department | partial unique indexes `permission_preset_global_name_key`, `permission_preset_dept_name_key` |
| A published pointer names a version of its own document | composite FK to `document_version(id, document_id)` |
| Every grade names the action that created it | `grade.created_by_action_id NOT NULL` |
| A retried tool call cannot act twice | `unique(actor_id, idempotency_key)` |
| Every action carries the hash of what was asked, so a key reused for different content can be told from a retry | `action.payload_hash NOT NULL`, `action_payload_hash_valid` |
| An action's status agrees with its authorization: `denied` ⇔ `denied`; only `confirm_required` is proposed, rejected, cancelled or decided; only an executed `pending_review` is under review | `action_status_matches_authz` |
| `executed_at` is set exactly when status is `executed` | `action_executed_at_consistent` |
| Nobody approves or reviews their own action from the same seat | `action_not_self_decided`, `action_not_self_reviewed` |
| `document_version` and `event` are append-only | `reject_mutation()` triggers on UPDATE, DELETE, TRUNCATE |
| A submitted submission is never changed or deleted, except correcting `submitted` ⇄ `late` | `submission_frozen_after_submit` trigger |
| A grade has exactly one target | `grade_one_target` |
| Document owner columns match `kind` | `document_owner_matches_kind` |
| A version has text or a file; a file has a type and size | `document_version_has_content`, `document_version_file_described` |
| SSO credentials carry an identity; passwords, tokens and sessions carry a hash; tokens and sessions carry a lookup prefix; sessions expire | `credential_*` CHECKs |
| No credential is written for the system actor, nor moved to it | `credential_not_for_system_actor` trigger |
| Status, role, kind and scope columns hold only listed values | `*_valid` CHECKs |
| Emails are unique regardless of case | unique index on `lower(email)` |
| Login IDs are unique regardless of case, 1..64 of `[0-9A-Za-z._-]` (never an `@` or a space), and only a person's; one goes unverified only if it is a person's and there | unique index `actor_login_id_key`, `actor_login_id_valid`, `actor_login_id_is_a_persons`, `actor_unverified_login_id_is_a_persons` |
| Only a password is marked to be changed, and it says who set it | `credential_must_change_is_an_issued_password` |
| Only an agent has an owner; its owner is a person, not an agent, the system actor or itself; an agent someone owns holds no platform role | `actor_not_own_owner`, `actor_owned_is_agent`, `actor_owned_holds_no_platform_role`, trigger `actor_owner_valid` |
| An agent's owner is fixed when it is registered: never changed, taken away or given later | trigger `actor_owner_fixed` |
| An agent's seat that is not removed decides only by proposal: `action_decide` at `confirm_required` at most, a level above it written as that | trigger `course_member_agent_ceiling` |
| Making an actor active forgets who suspended it | trigger `actor_suspension_cleared` |
| A seat that is not removed has a principal exactly when its actor has an owner; the principal is the owner's seat, in the same course, and nobody's delegate | composite FK `course_member_principal_fk`, trigger `course_member_principal_valid` |
| A delegate's seat is removed with its principal's, whichever release removes it | trigger `course_member_delegates_follow` |
| Only a delegate's seat answers the course | `course_member_answers_course_is_delegate` |
| A conversation's two participants are seats of its course and never change; a closed conversation stays closed; none is deleted | composite FKs, `conversation_*` CHECKs, trigger `conversation_guarded` |
| Only a conversation's two participants write in it, only while it is open; a reply is the respondent's, to a message of the opener's in the same conversation | trigger `conversation_message_author_valid`, composite FKs on `conversation_message` |
| A message and its retraction are in their conversation's course; one message at each `seq`, from 1; a message is retracted once | composite FKs, `unique(conversation_id, seq)`, `conversation_message_seq_positive`, primary key of `conversation_message_retraction` |
| Every message and retraction names its action | `created_by_action_id NOT NULL` |
| `conversation_message` and `conversation_message_retraction` are append-only | `reject_mutation()` triggers on UPDATE, DELETE, TRUNCATE |
| Memory is held by agents; owner memory is about the agent's owner; each seat an entry names is its actor's, in the entry's course | trigger `memory_entry_guarded`, composite FKs on `memory_entry` |
| Each scope of memory has its shape; only shared memory is proposed or rejected; a live entry has 1..1000 characters and its hash, a rejected one neither | `memory_shape_valid`, `memory_body_valid` |
| One text per bucket among live entries | partial unique index `memory_text_key` |
| Whose an entry is, about whom, where and when it was made never change; a rejected entry never changes | trigger `memory_entry_guarded` |
| A department is never under itself or under a department beneath it, and the tree is at most 8 levels deep | `department_not_own_parent`, trigger `department_tree_valid` |
| A department's administrator is a person; nobody appoints themselves; one live appointment per person and department | trigger `department_admin_guarded`, `department_admin_not_self_appointed`, partial unique index `department_admin_one_live` |
| An appointment is kept as written, and ended once, saying by whom | trigger `department_admin_guarded`, `department_admin_removal_recorded` |
| An action's capacity is `platform`, `department` or none, and only `department` names a department | `action_authority_valid`, `action_authority_dept_fk` |
| Domain rows are never silently cascade-deleted | FKs default to NO ACTION |

## What the application must enforce

The database cannot express these. Each one is a place a bug can hide.

- **`authorize()` itself**: membership lookup, the permission column, and the
  student / assignment scope checks.
- **Action status flow**: the transitions. The database checks that a row at
  rest agrees with its `authz_result`; the order things happen in is code.
- **Idempotent replay**: same key and same `payload_hash` returns the stored
  `result`; same key and a different hash is refused.
- **Re-authorizing the proposer** when a proposal is approved, against the
  membership row it was made under, and cancelling proposals past their TTL.
- **Event scope**: filling `event.student_member_id` / `assignment_id`
  correctly, and filtering the feed by them.
- **Same-course consistency** where no composite key covers it: scope rows
  name members and assignments of the same course; `assignment` instructions
  and rubric are documents of the same course with the right `kind`;
  `document_version.author_member_id` is a member of the document's course.
- **The submitting member has `role = 'student'`.** The composite key only
  proves same course.
- **Grade computation**, and writing an `origin = 'computed'` snapshot only
  when a rolled-up component or the course total is posted.
- **Component tree acyclicity**: the CHECK blocks only a self-loop.
- **Cancelling pending proposals** when a member is removed or expires, and
  removing a member's delegates with them.
- **A delegate holds no more than its principal**: levels, reach and life,
  read with the principal on every call and in every list.
- **Who may address whom** in a conversation (`tools.addressing`): the
  respondent within the asker's seat, or the asker's own delegate; a delegate
  answers others only if its seat answers the course.
- **Closing a removed seat's conversations**, a delegate's with its
  principal's, and cancelling its proposals.
- **A token of the system actor's** written before migration 0004 refused
  them authenticates nobody.
- **A temporary password** (`credential.must_change`) refuses its person every
  call but setting their own; who may set one for whom is the tool's
  (`member.reset_password`).
- **`actor.kind` and `course_member.role` are never read by authorization.**

## Regrading

`grade.superseded_by` is a deferred foreign key, so a regrade must run in one
transaction, in this order:

1. `UPDATE` the old row: `superseded_by = <new id>`
2. `INSERT` the new row with `posted_at` and `posted_by_member_id` set

Reversing the order briefly produces two live grades and trips the partial
unique index.

## Verification status

Executed against PostgreSQL 18.6: up, down and up again apply cleanly, and
`tests/constraints_test.sql` passes (93 checks; 162 since migrations 0007
and 0008, which have been run on PostgreSQL 16; 220 since 0009, run on 16 and
18). **Not yet run on PostgreSQL
13**, the stated minimum: CI runs this suite and the Go tests on both 13 and
18, so the first pipeline run settles it — update this paragraph with the
result.

The tests cover every row of the table above, plus one structural guard: the
`perm_*` columns of `permission_preset` and `course_member` must be identical,
so adding an action type to one table and not the other fails the suite.
Application-level rules are not under test here; they belong with the
application code.

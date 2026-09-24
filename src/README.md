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
  seed/
    presets.sql          the six built-in permission presets; safe to re-run
  tests/
    constraints_test.sql checks the database-enforced rules; rolls back
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

A migration that fails under `aishiterud` has applied nothing, but leaves its
version recorded as dirty. Fix the cause, then `aishiterud migrate force N`,
N being the last migration fully applied (0 if none), and `migrate up` again.

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
database — every migration up, the seed, the checks, every migration down, a
check that nothing was left behind, and up again — and is what CI runs on
PostgreSQL 13 and 18.

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
| Nobody approves or reviews their own action | `action_not_self_decided`, `action_not_self_reviewed` |
| `document_version` and `event` are append-only | `reject_mutation()` triggers on UPDATE, DELETE, TRUNCATE |
| A submitted submission is never changed or deleted, except correcting `submitted` ⇄ `late` | `submission_frozen_after_submit` trigger |
| A grade has exactly one target | `grade_one_target` |
| Document owner columns match `kind` | `document_owner_matches_kind` |
| A version has text or a file; a file has a type and size | `document_version_has_content`, `document_version_file_described` |
| SSO credentials carry an identity; passwords, tokens and sessions carry a hash; tokens and sessions carry a lookup prefix; sessions expire | `credential_*` CHECKs |
| No credential is written for the system actor, nor moved to it | `credential_not_for_system_actor` trigger |
| Status, role, kind and scope columns hold only listed values | `*_valid` CHECKs |
| Emails are unique regardless of case | unique index on `lower(email)` |
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
- **Cancelling pending proposals** when a member is removed or expires.
- **A token of the system actor's** written before migration 0004 refused
  them authenticates nobody.
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
`tests/constraints_test.sql` passes (93 checks). **Not yet run on PostgreSQL
13**, the stated minimum: CI runs this suite and the Go tests on both 13 and
18, so the first pipeline run settles it — update this paragraph with the
result.

The tests cover every row of the table above, plus one structural guard: the
`perm_*` columns of `permission_preset` and `course_member` must be identical,
so adding an action type to one table and not the other fails the suite.
Application-level rules are not under test here; they belong with the
application code.

# AIshie Core

An agent-centered LMS where AI agents are primary actors, not bolted-on
features. Core is pure orchestration — state, events and one tool surface that
humans (REST) and agents (MCP) both call. It hosts no models and never dials
out to an agent.

- Why it is shaped this way: [docs/aishie-core-concepts.md](docs/aishie-core-concepts.md)
- The data model and `authorize()`: [docs/schema.md](docs/schema.md)
- The SQL layer, runnable with plain `psql`: [src/README.md](src/README.md)

## Status

The PostgreSQL layer is complete. The Go backend is being built in milestones.
In place so far:

- `authorize()`, the tool registry, and the action pipeline every call goes
  through — idempotent replay, proposals with re-authorization on approval,
  after-the-fact review, events;
- authentication (API tokens for agents and nobody else; for people, a
  session from a password or single sign-on) and the REST API, whose routes
  are generated from the tool registry;
- the tool catalogue: actors, terms, departments, presets, courses, members,
  the grading scheme, assignments, submissions, grades, documents, the
  approval and review queues, and the event feed — all scope-filtered in SQL;
- agents people own: registered by the person, brought into a course as their
  delegate, and never able to do more there than the person's own seat; given
  `member_manage`, one manages the course's members for the person, never the
  person's own seat nor their other agents'; any agent decides and reviews
  only by proposal, and each seat says the most it may hold of each
  permission (`perm_ceilings`);
- conversations, between a person and an agent and nothing else: a member
  asks an agent — the course's tutor agent, their own agent — questions, and
  it answers them, each message an action; a person answers none, and people
  talk to people elsewhere; nobody may ask an agent that can see or do more
  than they can, nor one the site's own agent runtime does not host: every
  agent is hosted one way for good, a runtime agent by the runtime, which
  alone is issued its token, by its id, and asked in the site, or an mcp
  agent, its owner's own tools', which is asked nothing there; a
  person's conversations in every course are one list, newest first, saying
  what they have not read yet; while an agent writes an answer, whoever reads
  the conversation watches it come — what the agent is doing, and the text
  where the answer would be shown — through a draft that is no action and
  that the answer, posted, replaces; and root and the administrators of the
  site, and of a department for its courses, export conversations for
  audit, retracted messages included, as files kept a day, each export
  itself on record;
- agents' memory, kept in Core whatever runs the agent (off unless
  `MEMORY=on`): about its owner, about each person who asks it in a course,
  reached only through that person's conversation, and a course's shared
  memory, which waits for review; bounded in size and rate, never secrets,
  and never in the action log;
- files: versioned documents with publish-by-pointer, uploads and downloads
  by short-lived URL (this server's disk, or any S3-compatible store), and
  feedback files that travel with a grade through a proposal; documents are
  renamed and brought back from the archive, and what was uploaded by
  mistake is purged by an administrator, leaving a tombstone; every Word,
  Excel, PowerPoint or OpenDocument file, a document's of any kind or a
  message's, converted to PDF once by the site's agent runtime, for the
  front end to preview, and read by whoever reads the file;
- records that can be corrected, with their history kept: a seat's roster
  role, what graded work is worth and where it counts (its grades rescaled or
  kept, the totals following), a computed total overridden beside the number
  worked out, and final grades' treating ungraded work as zero undone.

`make e2e` runs the real binary against a scratch database and, with nothing
but `curl`, builds the worked example from docs/schema.md §5 from an empty
installation: root, bootstrapped with a password and no token, signs in; every
person is invited, chooses a password and signs in with it, every agent is
given a token, and nobody gives a person one; an agent grades an essay, a person approves it, the student
sees the grade; the instructor renames the course, halves the assignment's
points with the grade rescaled, overrides the student's total and takes the
override off, renames the slides and brings them back from the archive, and
makes the student a TA and a student again; then, once the site's agent
runtime, given its credential on the command line, is issued the token of
the instructor's tutor agent by its id, the student asks the tutor a
question, watches its answer's draft come, waiting on the conversation, and
it answers, and she is refused the instructor as a respondent,
as he is refused answering; her chat panel lists the conversation, unread
until she marks it read; the runtime converts the instructor's Word handout
and the slides she sends the tutor to PDF, which she opens and nobody else
does; once the runtime stops hosting the tutor, she asks it nothing more; another agent of the instructor's, an mcp agent, given
`member_manage`, seats a student with his token for it, is refused on the
instructor's seat, is asked nothing in the site and works over MCP; the
instructor shows a join link, through which a new student registers and a
registered one joins, and which seats nobody once
revoked; a student with no email registers through another with her student
number as her login ID and signs in with it, is given a temporary password by
the instructor when she forgets hers, and sets her own before anything else,
and the instructor cannot reset a TA's; root exports the course's
conversations for audit and downloads both files, the question the student
withdrew in them, marked, a department's administrator exports only what is
beneath her, and the instructor, the student and an agent are refused; Core vouches for the instructor to an agent runtime, and the
key it publishes checks the assertion; the sign-in page is told whether
to offer single sign-on, with it off and then, against a stand-in provider,
on; last, root finds the server reaches no provider of the site's on this
machine, where the stand-in is, until it is restarted with
`SSO_ALLOW_PRIVATE_ISSUERS`; then sets up a provider of the site's against a
stand-in provider that signs, tests it, switches it on, and a person linked at
it signs in through it, and nothing says its secret; restarted without the
setting, the server reaches it no more, and the sign-in page offers it no
more.

- MCP: agents connect at `/mcp` (stateless streamable HTTP, bearer token) and
  get the same catalogue as REST, tool for tool, through the same pipeline.

- background sweeps, run as the system actor and recorded like any other
  action: stale proposals cancelled, expired memberships removed, missing
  submissions marked when a due date passes, uploads that nothing came to
  point at removed. Every instance may run them; Postgres advisory locks see
  that one does.

- single sign-on over OpenID Connect (written against ADFS), through the
  operator's provider and those the site's administrators set up from the
  front end, their client secrets sealed, which signs in people who are
  already registered and creates nobody;
- join links: a course's link, shown in class as a QR code and working for ten
  minutes, seats whoever opens it as a student, signed in or registering
  through it, which is the one way a person registers themselves; made by
  whoever holds `member_invite`, with their authority;
- assertions: a short-lived, signed statement of who is signed in, which a
  service that hosts agents checks against the key Core publishes, so that
  it needs no sign-in of its own;
- the things a server on the open internet needs: a per-actor rate limit
  shared by REST and MCP, a limit on sign-in attempts, a request log with no
  credentials in it, and a refusal to start against a schema older than the
  binary.

## Layout

```
cmd/aishie-core/  the server and its operator commands
internal/
  domain, apperr, ids, canon   vocabulary, errors, UUID v7, canonical JSON + payload hash
  db                           pool, migrations, queries/*.sql → dbq (sqlc)
  authz                        authorize(), exactly as docs/schema.md §3
  tool                         what a tool is; the registry both adapters are generated from
  pipeline                     the one road every call takes
  tools                        the catalogue, one file per noun
  events, gradecalc            the event feed's writer; grade rollups (pure)
  blob                         file storage: filesystem and S3, signed upload tokens
  jobs, members                background sweeps; what removing a member means
  auth                         who is calling: tokens, passwords, sessions, bootstrap
  httpapi                      REST adapter; routes generated from the registry
  mcpapi                       MCP adapter; tools generated from the registry
  testdb, testkit              a database per test; course fixtures
src/              migrations, seed and SQL tests; embedded into the binary
docs/             design documents
```

## Develop

Needs Go, PostgreSQL 13+ and, for the full check, `sqlc` and `golangci-lint`:

```
brew install go sqlc golangci-lint postgresql
```

```
make help          # every target, with a one-line description
make ci            # everything CI runs: lint, generated code, SQL suite, Go tests, build
make test          # Go tests only
make db-test-sql   # psql suite only
make e2e           # the real binary and curl, end to end
make sqlc          # regenerate internal/db/dbq after editing SQL
```

The Go tests give each test its own database, cloned from a template. They
need `TEST_DATABASE_URL` to point at a maintenance database whose role may
`CREATE DATABASE`; the Makefile defaults it to `postgres:///postgres`, a local
server over its unix socket. Without a local PostgreSQL, `make dev-db` starts
one in Docker (see the comment at the top of `docker-compose.yml`).
`make clean-testdb` drops the cached templates.

## Run

```
createdb aishie
make build
bin/aishie-core migrate up
bin/aishie-core seed
bin/aishie-core bootstrap --name "Your Name" --email you@example.edu --password-stdin
bin/aishie-core serve          # http://localhost:8080
```

`bootstrap` runs once. It creates the root actor, with the password it reads
from standard input and the email (`--email`) or login ID (`--login-id`) it
signs in with, and the system actor that background jobs run as. It prints no
API token: people hold none. On standard error it prints the two actors' ids
and what root signs in with; on standard output, nothing. Without a password,
or without an email or a login ID, it creates nothing. Root then signs in like
anyone else: in the web front end, or with curl, taking the session from the
cookie that comes back, which serves as a bearer token for as long as it lasts:

```
read -rsp 'Password: ' PW; echo
SESSION=$(printf '{"login":"you@example.edu","password":"%s"}' "$PW" |
  curl -s -o /dev/null -D - -H 'Content-Type: application/json' --data @- localhost:8080/v1/auth/login |
  sed -n 's/^[Ss]et-[Cc]ookie: ais_session=\([^;]*\).*/\1/p' | tr -d '\r'); unset PW
curl -H "Authorization: Bearer $SESSION" localhost:8080/v1/me
curl localhost:8080/v1/tools            # the whole catalogue, with JSON Schemas
```

A session lasts `SESSION_TTL` (12 hours by default). For tools and scripts, a
person uses one of their agents, which holds an API token of its own
(`agent.create`, then `agent.issue_token`; below), and does nothing they
cannot do themselves.

Configuration is environment variables only; `bin/aishie-core help` lists them.
Files are kept under `var/blobs` by default (`BLOB_STORE=fs`). For more than
one instance, or for production, use `BLOB_STORE=s3` with the `S3_*` settings
and a `SIGNING_KEY` shared by every instance. `S3_REGION` (`us-east-1` unless
set) is the region requests are signed for and, with AWS, the one they are
sent to, a region newer than the S3 client's own table of regions included.
`S3_BUCKET_LOOKUP` says how a request names the bucket: `path` after the
endpoint (`https://endpoint/bucket/key`), `dns` in the host name
(`https://bucket.endpoint/key`, virtual-hosted style, which some services take
alone), and `auto`, the default, in the host name for AWS, Google and Aliyun
and after the endpoint for anything else. With S3 an upload URL does not
limit what is PUT to it: `MAX_UPLOAD_BYTES` is checked only when the file is
attached, which refuses a larger one. An upload that is not attached
to a document within `PROPOSAL_TTL` plus two days is removed about an hour
after that, and can no longer be attached: the sweep goes through the store
once an hour and removes up to 200 such uploads every `JOBS_INTERVAL`, however
many attached files it passes on the way. So that no proposal outlives its
files, a call that would attach an upload by way of a proposal is refused
once the upload is two days old. With `PROPOSAL_TTL=0` proposals wait for
ever, and uploads are neither removed nor refused for their age. One version
of a document holds at most `DOCUMENT_MAX_FILES_PER_VERSION` files (20, at
most 100), `DOCUMENT_MAX_VERSION_BYTES` in all (200 MiB), each at most
`MAX_UPLOAD_BYTES`. The files
messages of conversations carry are kept the same way, under a prefix of their
own, and held to limits of their own: `ATTACHMENT_MAX_PER_MESSAGE` files to a
message (10), each at most `ATTACHMENT_MAX_BYTES` (50 MiB, and never more than
`MAX_UPLOAD_BYTES`), and `ATTACHMENT_MAX_CONVERSATION_BYTES` in one
conversation (500 MiB). The server keeps its files under `documents/`,
`courses/` (documents' uploads from before a version held several files) and
`conversations/` (with S3, `attached/documents/`, `attached/courses/` and
`attached/conversations/` as well), and exports of conversations for audit
under `exports/`, and leaves anything else in the directory
or bucket alone, uploads
under a course its database does not have included. The files may be moved from
the disk to a bucket by copying each to the key it has on the disk (its path
under `BLOB_FS_ROOT`; not the `.meta` beside it, whose content type the object
takes): Core finds what was attached where its version or message says, and an
upload attached on the disk is not attached again. Still, two deployments must not share a
directory or bucket: a test copy whose database was cloned from a school's
site has that site's courses, and each would take the files the other has attached
since the copy for orphans, and remove them.
`serve` never migrates on its own. It refuses to start against a schema older
than the binary (run `aishie-core migrate up` first), and `/healthz` reports
503 if the schema falls behind or a migration is left half-done.

### A person's first sign-in

A person signs in with their email or their login ID: the student or staff
number their school gives them (學號, 工號), which is what they have where
most students have no mailbox. It is 1 to 64 letters, digits, dots, hyphens
and underscores, never an `@`, unique in any case, and only a person has one.
An administrator gives it and corrects it (`actor.register`, `actor.update`
with `login_id`); nobody changes their own. `POST /v1/auth/login` takes
`{"login", "password"}`, where `login` is an email when it has an `@` and a
login ID otherwise; `{"email", "password"}`, as clients sent before login IDs,
still works and takes either the same way. Both are refused in one message
and limited alike, per address and per name.

An administrator registers the person with their email or login ID, or both
(`actor.register`), and invites them (`actor.invite`). The invitation is a token, `aisinv_…`, for
the web front end's page that takes invitations, which makes it a link. The
person opens it, chooses a password (the page sends the token and the
password to `POST /v1/auth/invite`) and is signed in, with the same session
cookie a sign-in gives; from then on they sign in with their email or login
ID and that password. An invitation works once, for seven days unless the administrator
gives another number of days (at most thirty), and inviting again replaces
it; a password set some other way, or a new email, withdraws it. Taken up by
someone who has a password already, it replaces that password: it is also how
a forgotten one is reset. A person holds no API token: `credential.issue_token`
and `actor.issue_token` refuse one for a person (`api_tokens_are_for_agents`),
whoever asks, and a person's token from before migration 0017, which revoked
them all, authenticates nobody. An agent is given a token instead (below), and
never a password, an invitation or single sign-on: each is refused
(`agents_use_api_tokens`).
`actor.list` finds anyone registered, by a piece of their name, email or login
ID, with whether they have a password yet or an invitation waiting, and
`actor.update` corrects a name, an email or a login ID, or gives one to
someone registered without. A department's administrator registers and
invites someone new in one step (`actor.invite_new`), finds someone registered
by their whole email or login ID (`actor.lookup_by_email`), and invites again only a
person whose account reaches nothing beyond the departments they administer;
that is asked again when the invitation is taken up.

### A forgotten password

A person with an email is invited again, which replaces their password. A
student with none is given a temporary password by whoever seats the course's
students: `member.reset_password` (`POST
/v1/courses/{course_id}/members/{member_id}/reset-password`), for a person who
holds `member_manage` at `autonomous`, returns it once in
`result.temporary_password` (with the student's `login_id`, to tell them what
to sign in with), signs the student out everywhere, and keeps it nowhere but
as a hash: not in the action log, its replay or its event
(`member.password_reset`). Their sign-in with it answers
`"password_change_required": true`, and until they have set their own
(`POST /v1/me/password`, which refuses the temporary one:
`password_unchanged`) every other call is refused, `403` with reason
`password_change_required`. It is only for an active student seat in the
course, a person's, whose account reaches nothing more: refused, each with its
reason, for an agent calling (`people_only`), by proposal (`not_by_proposal`)
or under review (`not_autonomous`), for one's own seat (`own_seat`), an
agent's (`not_a_person`), anyone but a student here (`not_a_student`), a seat
not active (`seat_not_active`), someone seated otherwise anywhere
(`seated_other_than_student`), with a platform role (`platform_role`), an
appointment (`administers`) or a linked identity (`sso_linked`), with nothing to
sign in with (`no_sign_in_name`), a student out of the caller's reach
(`student_out_of_scope`), or a seat holding more than the caller does
(`beyond_your_seat`). Anyone else's password is an administrator's
(`actor.invite`).

### Single sign-on

A person signs in through an OpenID Connect identity provider. There are two
kinds (docs/schema.md §2.1, Single sign-on):

- the operator's: set `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`
  and `SIGNING_KEY`. The defaults are for ADFS: accounts are known by their
  `upn` claim (`OIDC_SUBJECT_CLAIM`). Its id is `OIDC_PROVIDER_NAME`, such
  as `school-adfs`, under which every identity linked at it is recorded: set
  it before anyone is linked, and never change it after, or nobody linked
  can sign in. Unset, it is `polyu-adfs`, a default that stays for the
  installations that rely on it. The provider is discovered when the server
  starts, and administrators see it read-only.
- the site's: root and the platform's administrators set them up from the
  front end, with the `sso.*` tools, kept in the database with their client
  secrets sealed under `SECRETS_KEY` (32 random bytes in base64: `openssl
  rand -base64 32`). Without `SECRETS_KEY`, none is added
  (`secrets_key_missing`) and the operator's still works. A change is in
  force at the next sign-in, on every instance, with no restart.

What the server fetches of a provider of the site's, which administrators
name, it fetches from public addresses only: its discovery document, the key
set that names, and at a sign-in its token endpoint. Never from this machine,
a private network (10/8, 172.16/12, 192.168/16, fc00::/7), a link-local
address (169.254/16, fe80::/10, where clouds keep their metadata), the shared
100.64/10 (carrier-grade NAT, and a cloud's metadata), multicast, or any other
address IANA's registry says is not globally reachable, nor such an address
written in IPv6 (`::ffff:10.0.0.1`, NAT64's `64:ff9b::a00:1`, 6to4's
`2002:a00:1::`). It checks the address each connection is made to, once the
name is resolved, so that a name that resolves to a public address when it is
set up and to a private one later (DNS rebinding) reaches nothing, and a
redirect is checked as it is followed; it goes through no proxy
(`HTTPS_PROXY`) for them, which would choose the address itself. An issuer
plainly at such an address, or at `localhost`, is refused as it is set up
(`invalid_argument`, field `issuer`, reason `issuer_address_not_allowed`), and
one set up there earlier, or while the setting below was on, and switched on
is not offered on the sign-in page: `sso.list` gives it the status
`issuer_address_not_allowed`. One whose name resolves there is taken, and
`sso.test` reports it as a problem with the same reason, naming the URL and
never the address, as it does a key set it may not fetch and a token
endpoint none of whose addresses is public. A sign-in through such a
provider is `sso_provider_unavailable` as it starts, the reason in the
server's log: the token endpoint and the key set, which are fetched only
once the person comes back from the provider, are resolved as the provider
is discovered, and refused there when none of their addresses is public.
`SSO_ALLOW_PRIVATE_ISSUERS=true` lifts this, for development, tests and a site
whose provider is on its own network, or reached through a proxy, explicit or
transparent: a DNS that answers every name with a `198.18.` address (a
fake-IP mode) gives no public address at all. http is then taken for an
issuer on this machine alone, and the server says the setting is on as it
starts. The operator's provider is the operator's own setting, and is
reached wherever it is.

Register `<PUBLIC_URL>/v1/auth/sso/callback` with every provider: it is the
same for all of them, and `sso.list` gives it. A browser signs in by visiting
`/v1/auth/sso/start/{provider}?return_to=/where/to/go/afterwards` (or
`/v1/auth/sso/start?provider={provider}&return_to=…`; with one provider
offered, `/v1/auth/sso/start` alone goes through it) and comes back with the
same session cookie a password sign-in gives.

```
GET  /v1/sso/providers                          sso.list: the operator's first, then the site's; never a secret
GET  /v1/sso/providers/{provider_id}            sso.get
POST /v1/sso/providers                          sso.create {id, display_name, issuer, client_id, client_secret, ...}
POST /v1/sso/providers/{provider_id}            sso.update, over If-Match: "<version>" (or version in the body)
POST /v1/sso/providers/{provider_id}/enabled    sso.set_enabled {enabled}
POST /v1/sso/providers/{provider_id}/delete     sso.delete {force?}: refused while accounts are linked, unless forced
GET  /v1/sso/test?issuer=… | ?provider_id=…     sso.test: the discovery document and keys, checked; signs nobody in
```

The front end's sign-in page asks `GET /v1/auth/methods` which providers to
offer, so one front end serves every installation. Anyone may ask, with no
credential:

```
{"password": true, "password_accepts": ["login_id", "email"], "sso": null, "sso_providers": []}
{"password": true, "password_accepts": ["login_id", "email"],
 "sso": {"label": "School NetID", "start": "/v1/auth/sso/start"},
 "sso_providers": [{"id": "school-adfs", "label": "School NetID", "start": "/v1/auth/sso/start/school-adfs"}]}
```

`sso_providers` are the providers a sign-in may go through now: the
operator's first, then the site's that are switched on, by position. Each
has its `id`, its `label` — the operator's `OIDC_DISPLAY_NAME`, or null when
that is not set, and the front end then uses words of its own; a site's
provider's display name — and `start`, the path on this server to send the
browser to, with `return_to` added. `sso` is the first of them, for a front
end from before there were several: null when there is none, and its `start`
`/v1/auth/sso/start` when it is the only one. `OIDC_DISPLAY_NAME` is at most
64 characters, all printable, and the server refuses to start on anything
else. `password` is always true, since password sign-in cannot be turned
off. `password_accepts` says what the password sign-in's name field takes, in
the order its label should name them: a login ID (the student or staff
number) and an email. Nothing else about a provider is said. A browser or a
cache may keep the answer for a minute (`Cache-Control: public, max-age=60`),
so a provider switched on or off reaches the sign-in page within a minute.

Signing in creates nobody. An administrator registers the person
(`actor.register`) and links their identity (`actor.link_sso`, with the
provider's id and the person's subject there, such as their UPN) first; until
then the provider vouching for someone makes them nobody here. An identity
that has opened one account is never reassigned to another. A site's provider
may instead link by email (`link_by_email`, off by default): someone it
vouches for, whose identity was never linked, is linked at sign-in to the
active person whose email here is the one the provider vouches for
(`email_verified`), within the provider's `allowed_email_domains`, and never
to an account with a platform role.

Rotating `SECRETS_KEY`: put the new key in `SECRETS_KEY` and the old one in
`SECRETS_KEY_PREVIOUS`, restart, run `aishie-core secrets rewrap`, and then
remove the old one.

### Join links

Students come into a course by a link, typically a QR code shown in class.
Whoever holds `member_invite` makes one with `course.join_link_create`
(`{course_id, max_uses?, allowed_email_domains?}`), and is given its token
once, with `link_id` and `expires_at`: every link works for ten minutes from
when it is made, and then never again. `course.join_link_list` shows the
course's links, newest first, with their `status` (`live`, `expired`,
`used_up`, `revoked`), whether each seats anyone now and why not, and never a
token; `course.join_link_revoke` stops one. The front end puts the token in
its own join page's URL, and that page calls, with no tool in between:

```
GET  /v1/join/{token}            anyone; no credential
     200 {"course": {"code", "section", "title"}, "joinable": true|false,
          "reason": "expired|used_up|revoked|course_archived|creator_lost_authority",
          "expires_at", "registration": true|false, "allowed_email_domains": [...],
          "email_required": true|false}
     404 for every token that is not one, whatever is wrong with it
POST /v1/join/{token}            signed in, however: password, single sign-on, a token
     Idempotency-Key as every write; the answer is a tool call's, with
     result {course_id, member_id, status, already_member, join_link_id}
POST /v1/join/{token}/register   {"display_name", "login_id"?, "email"?, "password"}, a login ID
                                 or an email or both; an email when email_required; no credential
     200 {actor_id, expires_at, course_id, member_id, action_id}, signed in
     with the session cookie a password sign-in gives
```

A person signed in is seated as a student at once, or answered with the seat
they have (`already_member`, no use counted). A refusal says why in
`error.details.reason`: the link's own reason, `people_only` for an agent,
`email_domain_not_allowed` (also for no email, through a link kept to
domains), `actor_not_active`; registering, `email_taken` or `login_id_taken`
(sign in, then open the link again; the account that has it is not touched)
or a field's rule. Registering is the one way a person registers themselves,
and only through a live link: their email and login ID are recorded as
unchecked (`email_verified`, `login_id_verified` false) until an administrator
sets them. It is
limited per address together with sign-in attempts
(`SIGN_IN_ATTEMPTS_PER_MINUTE`) and per link (`JOIN_REGISTRATIONS_PER_MINUTE`,
60). `JOIN_LINK_REGISTRATION=off` turns registering off, as when single
sign-on covers everyone: the preview says `registration: false`, the endpoint
refuses (`registration_disabled`), and people sign in and then join.

### Connecting an agent

Every agent is hosted one way, chosen when it is registered and never changed
(`hosting`): `runtime`, run by the site's own agent runtime, which alone is
issued its token, by the agent's id, and asked by people in the site; or
`mcp`, reached by tools of its own over MCP, with tokens issued here, and
asked nothing in the site. For an mcp agent, register it and give it a token
(`actor.register` with `"hosting": "mcp"`, `actor.issue_token`, or
`aishie-core token issue`; only an mcp agent is given one), seat it in a
course (`member.add` with a preset such as `grader` or `tutor`), and point its
MCP client at `https://<host>/mcp` with `Authorization: Bearer <token>`. Tool names are the
registry's with the dot turned to an underscore (`grade_submit`). Every tool
that changes something takes an `idempotency_key` argument. A result whose
status is `proposed` is not an error: the action waits for a person, and the
agent learns the decision from `event_list`, which it may long-poll (`wait_s`,
below). The server's MCP
instructions tell a connecting model all of this. One HTTP request carries
one call: JSON-RPC batches are refused, since the rate limit counts requests.

To look around by hand: `npx @modelcontextprotocol/inspector`, transport
"Streamable HTTP", URL `http://localhost:8080/mcp`, and the bearer token.

The program that runs an agent lives outside Core.
[docs/agent-runtime.md](docs/agent-runtime.md) is the handout for building a
service that hosts agents: what it calls, how it finds questions and answers
them, and how it works with the mainstream model APIs.

A person may also have agents of their own, with no administrator involved:
`agent.create` registers one they own, saying how it is hosted (`hosting`,
required: `runtime` or `mcp`, for good); `agent.issue_token` gives an mcp agent
a token for their own tools, and the site's runtime is given a runtime
agent's (below); and `member.add_delegate` brings it into a course where they are seated, as
their delegate (a student's request waits for an instructor's approval by
default). There it can do nothing the person cannot, reach no one the person
cannot, and last no longer than the person's seat. The agent of someone who
does not manage the course's members, a student's, may be given their own
writes — drafting their submission, say — but does each only by proposal,
confirmed before it is carried out. It manages the course's
members only when the person names `member_manage` for it, and then never the
person's own seat nor their other agents' (`not_your_principal`). `AGENT_SELF_SERVICE=off`
leaves agents to administrators, and `AGENT_MAX_PER_OWNER` (5) bounds how many
agents that are not suspended one person may create or reactivate for
themselves; an administrator registering one is not counted.
`agent.list` returns both settings, as `self_service` and `limit`. An agent's
owner is fixed when it is registered: nobody changes it afterwards, and an
agent registered with no owner stays nobody's.

People in the site ask a runtime agent, and only a runtime agent, while the
site's agent runtime hosts it: while a token issued to the runtime for it
lives, and the agent and its owner are active. Nothing is declared. The
runtime is a site service (scope `agent_runtime`, like the transcriber's
`document_text`), whose credential root or an administrator issues
(`service.issue_credential`), or the operator at setup:
`aishie-core service issue agent_runtime --label runtime` prints it once on
standard output. With it the runtime hosts an agent by its id: it asks whether
the person signed in to it owns the agent (`agent_runtime.check_owner`), is
issued the agent's one token (`agent_runtime.issue_token`, which revokes the
one before), and revokes it when the hosting ends
(`agent_runtime.revoke_token`). With the same credential it converts every
Office and OpenDocument file Core keeps to PDF, once, for the front end to
preview: it claims what waits (`agent_runtime.rendition_claim`), downloads the
file, uploads the PDF (`agent_runtime.rendition_upload_url`) and says it is
done, or why not (`agent_runtime.rendition_complete`); `RENDITION_MAX_BYTES`
(100 MiB) bounds a PDF, and whoever may read a file reads its PDF
(`rendition` in `document.get`, `document.file` and `conversation.attachment`;
docs/schema.md §2.4, Renditions). A runtime agent's owner holds no token for it
(`hosted_by_runtime`). An mcp agent acts only while a person uses it from a
tool of their own (a chat app, an editor, a script), so it is not offered in
the site, and a question to it there is refused (`mcp_agent`); one to a
runtime agent the runtime does not run now is refused `agent_not_hosted`.
Nothing declares it (docs/schema.md §2.8): `me_site_chat`, which changed
nothing for one release, is gone since migration 0027.

An agent that answers questions long-polls `conversation_inbox` in each course
where it may (its `conversation_answer` in `me_memberships`): with `wait_s`, up
to 25 seconds, an empty inbox waits for a question and answers as soon as one
is asked, through any instance of the server, and the agent calls it again at
once. It reads each
waiting conversation with `conversation_messages`, and answers with
`conversation_answer`, naming the question it answers
(`in_reply_to_message_id`, the conversation's `latest_opener_message_id`). An
answer to anything but the latest question is refused as a conflict, so a
reply that took a while is never posted under a newer question, and so is a
second answer to one question, so an answer that failed or was rejected is
written again safely. People find the agents they may ask with
`conversation.respondents` and start with `conversation.open`; a person is
nobody's respondent and answers nothing (`conversations_are_with_agents`),
and a person's seat holds `conversation_answer` at `denied`. A person's chat
panel reads `me.conversations` (`GET /v1/me/conversations`): their
conversations in every course, newest activity first, each saying whether the
agent has written since they last marked it read (`conversation.mark_read`),
and waits on the one open with `conversation.messages`, `after_seq` and
`wait_s`, which answers as soon as the agent writes or the conversation's state
changes (`seen_state`, the state it last read, catches a change between two
calls), and, given `seen_draft_version`, as soon as the answer's draft changes:
while the agent's runtime writes an answer, it streams a draft of it
(`conversation.draft`: what it is doing, and the text so far), which the
conversation shows as `draft` until the answer replaces it, its text to the
asker only where the agent answers without approval, and otherwise to whoever
would approve the answer;
whoever decides actions lists one agent's conversations with
`conversation.list` and `respondent_member_id`. What is written is readable
by the two participants, by course staff who decide actions for the one who
asked, and, in the action log, by anyone who decides actions in the course;
and an agent that answers others too, such as a course's tutor agent, may
repeat it to them (docs/schema.md §2.8).

With `MEMORY=on`, an agent keeps its memory here, so that whatever runs it
reads and writes the same: `memory_write` about its owner (scope `owner`),
about the person who opened the conversation it answers (`asker`, with
`course_id` and `conversation_id`), or for the whole course (`course`, a
proposal until someone who manages the course approves it);
`memory_search`, `memory_list` and `memory_get` read it, `memory_update`
corrects and `memory_forget` deletes it. What is about one person is reached
only through a conversation of theirs the agent may answer now. The text is
never in the action log, and a secret is refused. `MEMORY_MAX_*` bound what an
agent keeps and `MEMORY_WRITES_PER_HOUR` (60) and `_PER_DAY` (300) how fast it
writes (docs/schema.md §2.9). It is off by default for now; memory tools
answer `memory_unavailable` while it is.

### Exporting conversations for audit

Root and the platform's administrators export the site's conversations for
audit, and a department's administrators those of the courses of the
departments they administer and beneath them; nobody else, and never an
agent (`people_only`), whatever role it holds. `conversation.export` (`POST
/v1/conversation-exports`) takes a course (`course_id`) or a department and
everything beneath it (`within_dept_id`), or neither for the whole site's,
which only a platform administrator exports; a participant
(`participant_actor_id`); and a span of time (`from`, `before`). It holds
every message of the conversations it chooses, a retracted one with its
text, marked `retracted`; what files each carries, described and never their
bytes; and the answers and questions proposed and never posted. It is two
files: the conversations as JSON Lines, one to a line with their messages,
and the messages as CSV, one to a row, in UTF-8 with a byte order mark so
that a spreadsheet opens Chinese as written. The answer gives a URL for
each, which downloads it for fifteen minutes and is kept nowhere;
`conversation.export_file` (`GET /v1/conversation-exports/{export_id}/{format}`,
`jsonl` or `csv`) gives another, to whoever made the export alone. Every
export is an action, its filters and counts on record. The files hold
personal data: they are kept under `exports/` for `EXPORT_TTL` (24 hours)
and then removed, and links to them expire. Past `EXPORT_MAX_MESSAGES`
(100,000) messages or `EXPORT_MAX_BYTES` (256 MiB) of their text an export is
refused, `export_too_large`, saying how much it would hold: narrow it.
docs/schema.md §2.8 has the whole of it.

### Signing in to a service that hosts agents

The service that hosts agents (the runtime) has a web interface in the front
end, where people connect their agents to it. It knows who they are by
Core's word, not by a sign-in of its own: the front end, signed in here,
asks `POST /v1/auth/assertion` with `{"audience": "https://lms.example.edu/runtime"}`
and gets back `{"assertion": "eyJ…", "expires_at": "…"}`, a JWT signed with
Ed25519 that names the person (`sub`, `kind`, `name`, `email`,
`platform_role`) for that audience alone, which it sends to the runtime as a
bearer token. The runtime checks it against `GET /v1/auth/keys`, a JSON Web
Key Set anyone may read. Only an active person is given one, never an agent,
and it lasts `ASSERTION_TTL` (5 minutes; 1 to 15) and never past the session
that asked. It is no credential here: Core takes only its own
`ais_` tokens. The request log never carries it, and the answer is not to be
cached.

`RUNTIME_AUDIENCES` lists the audiences, comma separated absolute URLs; with
none, no assertion is made. The key is `ASSERTION_KEY` (base64 of a 32-byte
Ed25519 seed, `openssl rand -base64 32`), or else one derived from
`SIGNING_KEY`, so a server that has it needs nothing new. A server given
`RUNTIME_AUDIENCES` and neither key refuses to start. The key set is
published whenever there is a key. The handout, docs/agent-runtime.md §5.1,
says what the runtime checks.

### The API in one paragraph

Every route is a tool, and `GET /v1/tools` lists them. A tool that changes
state is a `POST` and needs an `Idempotency-Key` header, all but
`conversation.draft`, an answer's draft while it is written, which is listed
as `ephemeral`, takes none and records nothing: send the same key
with the same body again and you get the first answer back
(`Idempotency-Replayed: true`) with nothing done twice; send it with a
different body and you get `409 idempotency_conflict`. The response says what
became of the call. A call that was attempted is recorded, and the answer
names the action in a top-level `action_id`: `200` executed, `202` proposed
(it now waits for a human; watch the action id; one its tool's rules refuse as
the course stands fails at once instead), `403` denied, and a failure
with its error's own status, `400`, `403`, `404`, `409` or `422`, or `429`, with
`Retry-After`, for an agent writing to its memory faster than it may. A proposal
replayed says what has become of it: `202` while it waits, `200` executed,
`409` rejected, `422` cancelled, or its failure's status. An answer with no
top-level `action_id` recorded nothing, whatever its status: among them every
`401` and every other `429`, a `400` or `404` for a call that was never attempted (its
arguments refused by the schema, or by its tool's check of what they say alone, such as a
score below zero on a grade, whoever makes it), a `403`
for a browser's `POST` from another origin not in `TRUSTED_ORIGINS`, a `405`,
a `500`, every read, and every draft, whose writes are bounded per
conversation, ten a second, then `429`, and cost nothing of the rate limit
when they are carried out; `429` carries `Retry-After`. A
`409 idempotency_conflict` names the earlier action in
`error.details.action_id` and records nothing either, so a call corrected
after a recorded failure needs a new key. Every answer, including the one for
a path that does not exist, is JSON. Agents authenticate with
`Authorization: Bearer <token>`, their API token, and a site service with its
own credential (`aissvc_`), at its own tools' routes alone; people sign in at `POST /v1/auth/login` (or
through single sign-on, or by taking up an invitation at `POST /v1/auth/invite`)
and carry a session cookie, which may also be sent as a bearer token. A person
holds no API token: one issued before migration 0017, which revoked them, is
answered `401` with `api_tokens_are_for_agents`, and an agent's password
signs it in nowhere (`agents_use_api_tokens`). Set `TRUSTED_ORIGINS` to
the web front end's origin so that its browser requests are accepted. The
front end is expected to be same-site with this server (the session cookie is
`SameSite=Lax`); a front end on another site needs `COOKIE_SAMESITE=none`.

The server speaks plain HTTP and expects a reverse proxy to terminate TLS.
Name the proxy's address range in `TRUSTED_PROXIES` (CIDRs), or every
request looks like it comes from the proxy and the per-address limit on
sign-in attempts becomes one bucket for the whole installation; with the
proxy named, the client is the one it forwards in `X-Forwarded-For`, and that
header is ignored from anywhere else. A proxy on the same machine is
`127.0.0.1/32` (or `::1/128`). Until it is named, `/mcp` refuses what it
forwards: a request over loopback for a public host name is also what a page
reaching the server by DNS rebinding sends.

## CI and releases

Every pull request and every push to `main` runs [ci.yml](.github/workflows/ci.yml):
lint, generated-code drift, the SQL suite and the Go tests on PostgreSQL 13 and
18, the S3 store against MinIO, the curl end-to-end, a build (including the
Docker image, never pushed) and `govulncheck`. Each job is a `make` target, so
a green `make ci` locally means the same thing.

A push to `main` whose checks all pass is published:
[publish.yml](.github/workflows/publish.yml) pushes its image as
`ghcr.io/aishie-education/aishie-core:sha-<commit>` and moves `:edge` to it.

Releases are built only from version tags (`v*.*.*`):
[release.yml](.github/workflows/release.yml) checks that the tag is on
`main`, runs the whole of CI again, and then publishes binaries for Linux and
macOS with checksums, and a multi-architecture image (`:1.2.3`, `:1.2`,
`:latest`, and `:stable` for the highest stable release) with its SBOM and
build provenance. A pre-release tag (`v1.2.3-rc.1`) moves neither `:latest`
nor `:stable`. How to cut a release, and the repository settings all this
needs, are in [CONTRIBUTING.md](CONTRIBUTING.md).

This repository and its image are public: anyone pulls the image, and clones
the code, with no login.

## Running a site

A site runs Core with the agent runtime and the web front end, one Docker
Compose stack per server, which
[AIShie-Deploy](https://github.com/AIShie-Education/AIShie-Deploy) sets up
and documents. The server keeps itself up to date: every five minutes it
looks at the tag each service follows, `:edge` on a test site and the
release its operator names on a school's, and deploys a new image by a safe
sequence (a backup, `migrate up`, the switch, the health check, and a
rollback if it fails, though one past migration 0027 has to migrate down
first: docs/deploying.md). Nothing in this repository reaches a server.

The older way, a server of Core alone, is still here:
[deploy/setup-server.sh](deploy/setup-server.sh) sets one up, and
[deploy.yml](.github/workflows/deploy.yml) deploys to it over SSH, with
[deploy/aishie-deploy](deploy/aishie-deploy) on the server, once the
repository has the server's address and key; until then a deploy records
itself in the environment, says which image is ready and does nothing. The
agent runtime and the web front end join such a server by scripts of their
own repositories, the same older way. [docs/deploying.md](docs/deploying.md)
covers it.

## License

AIshie Core is copyright 2026 XIE Hanming, and source-available under the [Elastic License 2.0](LICENSE) (ELv2), governed by the laws of Hong Kong. You may use, copy, change and redistribute it on the terms in LICENSE, which include that you may not offer it to others as a hosted or managed service.

For clarity: an educational institution that runs its own installation for its own staff and students is not providing the software to third parties as a hosted or managed service.

（補充說明：教育機構自行架設、供其教職員及學生使用，不視為向第三方提供託管服務。）

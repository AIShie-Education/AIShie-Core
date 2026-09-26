# AIShiteru-Core

An agent-centered LMS where AI agents are primary actors, not bolted-on
features. Core is pure orchestration — state, events and one tool surface that
humans (REST) and agents (MCP) both call. It hosts no models and never dials
out to an agent.

- Why it is shaped this way: [docs/aishiteru-core-concepts.md](docs/aishiteru-core-concepts.md)
- The data model and `authorize()`: [docs/schema.md](docs/schema.md)
- The SQL layer, runnable with plain `psql`: [src/README.md](src/README.md)

## Status

The PostgreSQL layer is complete. The Go backend is being built in milestones.
In place so far:

- `authorize()`, the tool registry, and the action pipeline every call goes
  through — idempotent replay, proposals with re-authorization on approval,
  after-the-fact review, events;
- authentication (API tokens for agents, password sessions for people) and
  the REST API, whose routes are generated from the tool registry;
- the tool catalogue: actors, terms, departments, presets, courses, members,
  the grading scheme, assignments, submissions, grades, documents, the
  approval and review queues, and the event feed — all scope-filtered in SQL;
- agents people own: registered by the person, brought into a course as their
  delegate, and never able to do more there than the person's own seat;
- conversations: a member asks one other member — the course's tutor agent,
  their own agent — questions, and it answers them, each message an action;
  nobody may ask anyone who can see or do more than they can;
- files: versioned documents with publish-by-pointer, uploads and downloads
  by short-lived URL (this server's disk, or any S3-compatible store), and
  feedback files that travel with a grade through a proposal.

`make e2e` runs the real binary against a scratch database and, with nothing
but `curl`, builds the worked example from docs/schema.md §5 from an empty
installation: an agent grades an essay, a person approves it, the student
sees the grade; then the student asks the instructor's tutor agent a question,
and it answers.

- MCP: agents connect at `/mcp` (stateless streamable HTTP, bearer token) and
  get the same catalogue as REST, tool for tool, through the same pipeline.

- background sweeps, run as the system actor and recorded like any other
  action: stale proposals cancelled, expired memberships removed, missing
  submissions marked when a due date passes, uploads that nothing came to
  point at removed. Every instance may run them; Postgres advisory locks see
  that one does.

- single sign-on over OpenID Connect (written against ADFS), which signs in
  people who are already registered and creates nobody;
- the things a server on the open internet needs: a per-actor rate limit
  shared by REST and MCP, a limit on sign-in attempts, a request log with no
  credentials in it, and a refusal to start against a schema older than the
  binary.

## Layout

```
cmd/aishiterud/   the server and its operator commands
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
createdb aishiteru
make build
bin/aishiterud migrate up
bin/aishiterud seed
bin/aishiterud bootstrap --name "Your Name" --email you@example.edu --password-stdin
bin/aishiterud serve          # http://localhost:8080
```

`bootstrap` runs once. It creates the root actor (and the system actor that
background jobs run as) and prints root's API token, once:

```
export TOKEN=ais_...
curl -H "Authorization: Bearer $TOKEN" localhost:8080/v1/me
curl localhost:8080/v1/tools            # the whole catalogue, with JSON Schemas
```

Configuration is environment variables only; `bin/aishiterud help` lists them.
Files are kept under `var/blobs` by default (`BLOB_STORE=fs`). For more than
one instance, or for production, use `BLOB_STORE=s3` with the `S3_*` settings
and a `SIGNING_KEY` shared by every instance. With S3 an upload URL does not
limit what is PUT to it: `MAX_UPLOAD_BYTES` is checked only when the file is
attached, which refuses a larger one. An upload that is not attached
to a document within `PROPOSAL_TTL` plus two days is removed about an hour
after that, and can no longer be attached: the sweep goes through the store
once an hour and removes up to 200 such uploads every `JOBS_INTERVAL`, however
many attached files it passes on the way. So that no proposal outlives its
files, a call that would attach an upload by way of a proposal is refused
once the upload is two days old. With `PROPOSAL_TTL=0` proposals wait for
ever, and uploads are neither removed nor refused for their age. The server
keeps its files under `courses/` (with S3, `attached/courses/` as well) and
leaves anything else in the directory or bucket alone, uploads under a course
its database does not have included. Still, two deployments must not share a
directory or bucket: a staging copy whose database was cloned from production
has production's courses, and each would take the files the other has attached
since the copy for orphans, and remove them.
`serve` never migrates on its own. It refuses to start against a schema older
than the binary (run `aishiterud migrate up` first), and `/healthz` reports
503 if the schema falls behind or a migration is left half-done.

### A person's first sign-in

An administrator registers the person with their email (`actor.register`)
and invites them (`actor.invite`). The invitation is a token, `aisinv_…`, for
the web front end's page that takes invitations, which makes it a link. The
person opens it, chooses a password (the page sends the token and the
password to `POST /v1/auth/invite`) and is signed in, with the same session
cookie a sign-in gives; from then on they sign in with their email and that
password. An invitation works once, for seven days unless the administrator
gives another number of days (at most thirty), and inviting again replaces
it; a password set some other way, or a new email, withdraws it. Taken up by
someone who has a password already, it replaces that password: it is also how
a forgotten one is reset. An agent is given a token instead (below). `actor.list` finds anyone
registered, with whether they have a password yet or an invitation waiting,
and `actor.update` corrects a name or gives an email to someone registered
without one.

### Single sign-on

Set `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET` and `SIGNING_KEY`,
and register `<PUBLIC_URL>/v1/auth/sso/callback` with the provider. The
defaults are for ADFS: accounts are known by their `upn` claim
(`OIDC_SUBJECT_CLAIM`) and the provider is recorded as `polyu-adfs`
(`OIDC_PROVIDER_NAME`). A browser signs in by visiting
`/v1/auth/sso/start?return_to=/where/to/go/afterwards` and comes back with the
same session cookie a password sign-in gives.

Signing in creates nobody. An administrator registers the person
(`actor.register`) and links their identity (`actor.link_sso`, with the
provider's name and the person's UPN) first; until then the provider vouching
for someone makes them nobody here. An identity that has opened one account is
never reassigned to another.

### Connecting an agent

Register the agent and give it a token (`actor.register`, `actor.issue_token`,
or `aishiterud token issue`), seat it in a course (`member.add` with a preset
such as `grader` or `tutor`), and point its MCP client at
`https://<host>/mcp` with `Authorization: Bearer <token>`. Tool names are the
registry's with the dot turned to an underscore (`grade_submit`). Every tool
that changes something takes an `idempotency_key` argument. A result whose
status is `proposed` is not an error: the action waits for a person, and the
agent learns the decision by polling `event_list`. The server's MCP
instructions tell a connecting model all of this. One HTTP request carries
one call: JSON-RPC batches are refused, since the rate limit counts requests.

To look around by hand: `npx @modelcontextprotocol/inspector`, transport
"Streamable HTTP", URL `http://localhost:8080/mcp`, and the bearer token.

A person may also have agents of their own, with no administrator involved:
`agent.create` registers one they own, `agent.issue_token` gives it a token,
and `member.add_delegate` brings it into a course where they are seated, as
their delegate (a student's request waits for an instructor's approval by
default). There it can do nothing the person cannot, reach no one the person
cannot, and last no longer than the person's seat. `AGENT_SELF_SERVICE=off`
leaves agents to administrators, and `AGENT_MAX_PER_OWNER` (5) bounds how many
agents that are not suspended one person may have.

An agent that answers questions polls `conversation_inbox` in each course
where it may (its `conversation_answer` in `me_memberships`), reads each
waiting conversation with `conversation_messages`, and answers with
`conversation_answer`, naming the question it answers
(`in_reply_to_message_id`, the conversation's `latest_opener_message_id`). An
answer to anything but the latest question is refused as a conflict, so a
reply that took a while is never posted under a newer question. People find
whom they may ask with `conversation.respondents` and start with
`conversation.open`. What is written is readable by the two participants,
by course staff who decide actions for the one who asked, and, in the action
log, by anyone who decides actions in the course (docs/schema.md §2.8).

### The API in one paragraph

Every route is a tool, and `GET /v1/tools` lists them. A tool that changes
state is a `POST` and needs an `Idempotency-Key` header: send the same key
with the same body again and you get the first answer back
(`Idempotency-Replayed: true`) with nothing done twice; send it with a
different body and you get `409 idempotency_conflict`. The response says what
became of the call. A call that was attempted is recorded, and the answer
names the action in a top-level `action_id`: `200` executed, `202` proposed
(it now waits for a human; watch the action id), `403` denied, and a failure
with its error's own status, `400`, `403`, `404`, `409` or `422`. A proposal
replayed says what has become of it: `202` while it waits, `200` executed,
`409` rejected, `422` cancelled, or its failure's status. An answer with no
top-level `action_id` recorded nothing, whatever its status: among them every
`401` and `429`, a `400` or `404` for a call that was never attempted, a `403`
for a browser's `POST` from another origin not in `TRUSTED_ORIGINS`, a `405`,
a `500`, and every read; `429` carries `Retry-After`. A
`409 idempotency_conflict` names the earlier action in
`error.details.action_id` and records nothing either, so a call corrected
after a recorded failure needs a new key. Every answer, including the one for
a path that does not exist, is JSON. Agents authenticate with
`Authorization: Bearer <token>`; browsers sign in at `POST /v1/auth/login` (or
through single sign-on, or by taking up an invitation at `POST /v1/auth/invite`)
and carry a session cookie. Set `TRUSTED_ORIGINS` to
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
`ghcr.io/aishiteru-lms/aishiteru-core:sha-<commit>`, moves `:edge` to it, and
hands it to [deploy.yml](.github/workflows/deploy.yml) for the `staging`
environment.

Releases are built only from version tags (`v*.*.*`):
[release.yml](.github/workflows/release.yml) checks that the tag is on
`main`, runs the whole of CI again, and then publishes binaries for Linux and
macOS with checksums, and a multi-architecture image (`:1.2.3`, `:1.2`,
`:latest`) with its SBOM and build provenance. A pre-release tag
(`v1.2.3-rc.1`) does not move `:latest`, and goes to staging. A stable release
goes to production when somebody runs Deploy for it, from its tag.

deploy.yml deploys over SSH to a server set up with
[deploy/setup-server.sh](deploy/setup-server.sh), once the repository has its
address and key; until then a deploy records itself in the environment and
says which image is ready. On the server,
[deploy/aishiteru-deploy](deploy/aishiteru-deploy) backs up, runs `migrate up`
and `seed` with the new image, replaces the container, and waits for
`/healthz` to report the new version. Setting a server up, connecting it, and
running it day to day are in [docs/deploying.md](docs/deploying.md). How to cut
a release, and the repository settings this needs, are in
[CONTRIBUTING.md](CONTRIBUTING.md).

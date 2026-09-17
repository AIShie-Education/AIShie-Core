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
- files: versioned documents with publish-by-pointer, uploads and downloads
  by short-lived URL (this server's disk, or any S3-compatible store), and
  feedback files that travel with a grade through a proposal.

`make e2e` runs the real binary against a scratch database and, with nothing
but `curl`, builds the worked example from docs/schema.md §5 from an empty
installation: an agent grades an essay, a person approves it, the student
sees the grade.

- MCP: agents connect at `/mcp` (stateless streamable HTTP, bearer token) and
  get the same catalogue as REST, tool for tool, through the same pipeline.

- background sweeps, run as the system actor and recorded like any other
  action: stale proposals cancelled, expired memberships removed, missing
  submissions marked when a due date passes. Every instance may run them;
  Postgres advisory locks see that one does.

Still to come: SSO, rate limiting, the release workflow.

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
and a `BLOB_SIGNING_KEY` shared by every instance.
`serve` never migrates on its own: `/healthz` reports 503 until the schema
matches the version the binary was built for.

### Connecting an agent

Register the agent and give it a token (`actor.register`, `actor.issue_token`,
or `aishiterud token issue`), seat it in a course (`member.add` with a preset
such as `grader` or `tutor`), and point its MCP client at
`https://<host>/mcp` with `Authorization: Bearer <token>`. Tool names are the
registry's with the dot turned to an underscore (`grade_submit`). Every tool
that changes something takes an `idempotency_key` argument. A result whose
status is `proposed` is not an error: the action waits for a person, and the
agent learns the decision by polling `event_list`. The server's MCP
instructions tell a connecting model all of this.

To look around by hand: `npx @modelcontextprotocol/inspector`, transport
"Streamable HTTP", URL `http://localhost:8080/mcp`, and the bearer token.

### The API in one paragraph

Every route is a tool, and `GET /v1/tools` lists them. A tool that changes
state is a `POST` and needs an `Idempotency-Key` header: send the same key
with the same body again and you get the first answer back
(`Idempotency-Replayed: true`) with nothing done twice; send it with a
different body and you get `409 idempotency_conflict`. The response says what
became of the call: `200` executed, `202` proposed (it now waits for a human;
watch the action id), `403` denied, `409`/`422` failed. All four are recorded.
`400`, `401` and `404` mean the call was never attempted, and nothing was
recorded. Agents authenticate with `Authorization: Bearer <token>`; browsers
sign in at `POST /v1/auth/login` and carry a session cookie. Set
`TRUSTED_ORIGINS` to the web front end's origin so that its browser requests
are accepted.

## CI and releases

Every pull request and every push to `main` runs [ci.yml](.github/workflows/ci.yml):
lint, generated-code drift, the SQL suite and the Go tests on PostgreSQL 13 and
18, a build (including the Docker image, never pushed) and `govulncheck`.
Each job is a `make` target, so a green `make ci` locally means the same thing.

Nothing is deployed automatically. Release artifacts are built only from
version tags (`v*.*.*`).

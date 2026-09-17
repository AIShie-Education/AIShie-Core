# AIShiteru-Core

An agent-centered LMS where AI agents are primary actors, not bolted-on
features. Core is pure orchestration — state, events and one tool surface that
humans (REST) and agents (MCP) both call. It hosts no models and never dials
out to an agent.

- Why it is shaped this way: [docs/aishiteru-core-concepts.md](docs/aishiteru-core-concepts.md)
- The data model and `authorize()`: [docs/schema.md](docs/schema.md)
- The SQL layer, runnable with plain `psql`: [src/README.md](src/README.md)

## Status

The PostgreSQL layer is complete. The Go backend is being built in milestones;
so far the binary can migrate, seed and serve a health endpoint.

## Layout

```
cmd/aishiterud/   the server and its operator commands
internal/         Go packages (config, db, httpapi, testdb, ...)
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
bin/aishiterud serve          # http://localhost:8080/healthz
```

Configuration is environment variables only; `bin/aishiterud help` lists them.
`serve` never migrates on its own: `/healthz` reports 503 until the schema
matches the version the binary was built for.

## CI and releases

Every pull request and every push to `main` runs [ci.yml](.github/workflows/ci.yml):
lint, generated-code drift, the SQL suite and the Go tests on PostgreSQL 13 and
18, a build (including the Docker image, never pushed) and `govulncheck`.
Each job is a `make` target, so a green `make ci` locally means the same thing.

Nothing is deployed automatically. Release artifacts are built only from
version tags (`v*.*.*`).

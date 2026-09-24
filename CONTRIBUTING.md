# Contributing

## The loop

```
make ci     # what CI runs: lint, generated-code drift, the psql suite, Go tests, the curl end-to-end
make minio  # builds MinIO from source into bin/minio, for the S3 tests
make test-s3
```

Work on a branch and open a pull request against `main`. `main` is protected:
it takes changes by pull request only, with `lint`, `generated code is
current`, `sql` and `test` (each on PostgreSQL 13 and 18), `end to end (curl)`
and `build` green. CI also runs `test (s3 against minio)` and `vuln`.

## Where things go

- A rule the **database** can hold goes in a migration, with a check in
  `src/tests/constraints_test.sql`. A rule it cannot goes in the tool, with a
  Go test, and a line under "Enforced by the application" in `docs/schema.md`.
- A new **tool** is one `tool.Define` in `internal/tools`. It gets its REST
  route and its MCP tool from that declaration; there is nothing to add in
  `httpapi` or `mcpapi`. If it emits a new event type, give the type a row in
  the visibility table in `internal/tools/event.go` — a type with no row is
  visible to nobody but the member who caused it.
- A new **permission** is a constant in `internal/domain/perm.go` plus a
  migration adding the `perm_*` column to both `course_member` and
  `permission_preset`. A test fails until all three agree.
- **Authorization never reads `actor.kind` or `course_member.role`.**
  `domain.Actor` and `domain.Member` do not carry them, on purpose.
- SQL lives in `internal/db/queries/*.sql`; run `make sqlc` after editing it.
  List queries filter by scope **in SQL**, never afterwards.

## Migrations

`NNNN_name.up.sql` and `NNNN_name.down.sql`, both required, each one
transaction. `serve` never migrates on its own, and an old binary keeps
running while the new schema goes in, so a migration must leave the previous
release working: add a column in one release, stop using the old one in the
next, drop it in the one after.

## Releasing

A push to `main` goes out by itself once CI passes: its image is pushed to
GHCR as `:sha-<commit>` and `:edge`, and deployed to `staging`. A release is
made by a tag, from `main`:

```
git switch main && git pull
git tag -s v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

`release.yml` re-runs the whole of CI on the tagged commit, then publishes
binaries (Linux and macOS, amd64 and arm64) with checksums to the release
page, and a multi-architecture image to `ghcr.io/aishiteru-lms/aishiteru-core`.
The release notes list the migrations new in the release; for a stable
release, new since the last stable one, pre-releases included. A tag with a
hyphen (`v0.1.0-rc.1`) is a pre-release: it leaves `:latest` alone and is
deployed to staging.

A stable release goes to production when somebody runs **Deploy** for it:
Actions → Deploy → Run workflow, use the workflow from the release's tag, and
give the environment `production` and the image the release run's summary
names (`ghcr.io/aishiteru-lms/aishiteru-core:1.2.3`). That run is the decision
to deploy and to migrate. For production, Deploy takes nothing else: run from
a branch or a pre-release's tag, or given an image that is not a stable
release's, it stops before it deploys. To roll back, run Deploy from the
newest release's tag, which carries the current deploy procedure, with the
older release's image: `migrate up` leaves a schema a newer release migrated
as it is.

To try the build without publishing anything:

```
goreleaser release --snapshot --clean
```

### One-time settings

In the repository's settings on GitHub:

- **Environments**: `staging` and `production`. Let `staging` take `main` and
  tags `v*`, and `production` tags `v*` only. On a private repository,
  environments need a paid plan (Pro, Team or Enterprise), and required
  reviewers need GitHub Enterprise; where they are available, add them to
  `production` for a second look before a deploy runs. Deploy keeps
  production to stable releases by itself, on any plan.
- **Packages**: the image's package must let this repository's Actions write
  to it (package settings, Manage Actions access). A package the workflow
  creates starts that way.
- **Variables**, when they apply: `ATTESTATIONS` = `true` where artifact
  attestations are available (a public repository, or GitHub Enterprise
  Cloud); `DEPLOY_TARGET`, once deploy.yml has a deploy step for it.
- **Storage**: every green push to `main` leaves a `:sha-*` image, with its
  SBOM and provenance, which counts against a private repository's package
  storage. Nothing deletes them automatically, since deleting untagged
  versions can break a multi-architecture image; prune old ones from the
  package page when needed.

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
- A rule about what a call's **arguments** say alone — a blank name, a number
  below zero, two fields that exclude each other — goes in the tool's `Check`,
  so that the call is refused before anything is recorded or proposed,
  whoever makes it, with the error `Execute` would have given. `Check` is
  asked of a stored proposal too, so it takes whatever `Pin` stores; what a
  call may not say and a stored proposal may goes in `CheckCall`. A rule that
  needs the course, the caller's seat or the moment to tell — an expiry
  already past among them — goes in `Validate`, which is given the moment and
  runs before a proposal is queued as well as before `Execute`, again, as the
  proposer's, when it is approved, and for an agent's owner deciding whether
  it is theirs to decide. `Pin` refuses only what is true of a proposal and of
  no call, such as an upload too old to outlast it. A rule left to `Execute`
  alone is found only when someone approves the proposal.
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
- A member's level is `domain.Member.Perm`, never the `Perms` map: a
  delegate's is capped by its principal's there, and a list query takes the
  principal's scope as well as the member's (`authz.ScopeFilter`). A call
  that takes a delegate's seat and its principal's KEY SHARE takes the seat
  first; one that locks a delegate's seat FOR UPDATE takes the principal's
  KEY SHARE before it (`holdPrincipalOf`); removing a principal locks it and
  then its delegates (docs/schema.md §3).
- SQL lives in `internal/db/queries/*.sql`; run `make sqlc` after editing it.
  List queries filter by scope **in SQL**, never afterwards.

## Migrations

`NNNN_name.up.sql` and `NNNN_name.down.sql`, both required, each one
transaction. `serve` never migrates on its own, and an old binary keeps
running while the new schema goes in, so a migration must leave the previous
release working: add a column in one release, stop using the old one in the
next, drop it in the one after.

A migration that has reached `main` has run on edge: never delete,
renumber or rewrite it; undo it with a new one. `migrate up` leaves a schema
that is ahead of the binary as it is, since that is what a rollback looks
like, so a revert that takes a migration out leaves edge at a version
`main` no longer has, and the next migration to take its number is never
applied there. If one must go, run `migrate down` with an image that still
has it before the revert is deployed.

## Releasing

A push to `main` goes out by itself once CI passes: its image is pushed to
GHCR as `:sha-<commit>` and `:edge`, which edge servers pull within five
minutes ([AIShie-Deploy](https://github.com/AIShie-Education/AIShie-Deploy)).
When pushes come faster than they are published, one that a newer push
overtakes while it waits is not published. A release is made by a tag, from
`main`:

```
git switch main && git pull
git tag -s v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

`release.yml` re-runs the whole of CI on the tagged commit, then publishes
binaries (Linux and macOS, amd64 and arm64) with checksums to the release
page, and a multi-architecture image to `ghcr.io/aishie-education/aishie-core`.
The release notes list the migrations new in the release; for a stable
release, new since the last stable one, pre-releases included. A tag with a
hyphen (`v0.1.0-rc.1`) is a pre-release: it leaves `:latest` and `:stable`
alone. A stable release moves `:stable` to itself when it is the highest
stable release.

A stable release goes to a school's site when its operator names it in the
server's `/etc/aishie/aishie.env` (AIShie-Deploy's README, Upgrading
stable): that is the decision to deploy and to migrate. Going back is
pinning the release before (its README, Rolling back): `migrate up` leaves
a schema a newer release migrated as it is, and a migration keeps the
release before it working. A release further back may need what a later
migration has dropped.

To a server of Core's own (the older way,
[docs/deploying.md](docs/deploying.md)), somebody runs **Deploy** for it
instead: Actions → Deploy → Run workflow, use the workflow from the
release's tag, and give the environment `stable` and the image the release
run's summary names (`ghcr.io/aishie-education/aishie-core:1.2.3`). For
stable, Deploy takes nothing else: run from a branch or a pre-release's tag,
or given an image that is not a stable release's, it stops before it
deploys. To roll back there, run Deploy from the newest release's tag, whose
checks are the current ones, with the image of the release before.

To try the build without publishing anything:

```
goreleaser release --snapshot --clean
```

### One-time settings

Before the first push to `main` after the CD workflows land, in GitHub:

- **Environments** (repository Settings → Environments): `edge` and
  `stable`. Let `edge` take branch `main` and tags `v*`, and `stable` tags
  `v*` only (Deployment branches and tags → Selected branches and tags).
  Create them first: a run that names an environment that does not exist
  creates it, with no rules. The repository is public, so its environments
  take protection rules on GitHub Free: add required reviewers to `stable`.
  A repository set up when they were called `staging` and `production` needs
  `edge` and `stable` made as well, with the same rules
  ([docs/deploying.md](docs/deploying.md#settings-from-before-the-rename)).
- **Packages** (organization Settings → Packages): Default Package Settings
  should keep "Inherit access from source repository". The first publish
  then creates the package linked to this repository, which its workflows
  can write to. The package must be public (its settings → Danger Zone →
  Change visibility → Public): servers, and the other repositories' end to
  end tests, pull it with no login. Do not push the image by hand before the
  first publish: a package pushed from outside a workflow is not linked, and
  the workflow cannot push to it until it is given access (package settings,
  Manage Actions access).
- **Allowed actions** (organization Settings → Actions → General →
  Policies): if the organization allows only selected actions, allow
  `docker/*` and `goreleaser/*` with the rest. Pull requests' CI uses only
  `actions/*` and `sqlc-dev/*`, so a policy that leaves the others out
  first shows at the first publish or release.
- **Variables and secrets**, when they apply. Releases are attested with no
  setting, the repository being public (a private one would need GitHub
  Enterprise Cloud and `ATTESTATIONS` = `true`). For each environment with a
  server of Core's own (the older way: [docs/deploying.md](docs/deploying.md)),
  the repository variables `DEPLOY_TARGET_EDGE` and `DEPLOY_KNOWN_HOSTS_EDGE`
  and the repository secret `DEPLOY_SSH_KEY_EDGE` (`_STABLE` for stable),
  which `deploy/setup-server.sh` prints. They are the repository's, not the
  environment's, as the workflow was written when the repository was private
  and GitHub Free gave it no environment variables or secrets.
- **Minutes and storage**: the repository is public, so its Actions minutes
  on GitHub's standard runners cost nothing, and neither does a public
  package's storage. Every push to `main` runs the whole of CI and a
  two-architecture image build. Every green push also leaves a `:sha-*`
  image, with its SBOM and provenance. Nothing deletes old images
  automatically, since deleting untagged versions can break a
  multi-architecture image; prune them from the package page when needed.

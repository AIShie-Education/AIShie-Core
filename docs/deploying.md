# Deploying AIShiteru Core

One server per environment: staging first, production when staging has
earned it. On each server, [Caddy](https://caddyserver.com) serves HTTPS and
hands requests to the `aishiterud` container on `127.0.0.1:8080`, and
PostgreSQL runs on the same machine. The files people upload are kept under
`/srv/aishiteru/data`, and the configuration is in
`/etc/aishiteru/aishiteru.env`.

The scripts in [`deploy/`](../deploy) do the work:

- `setup-server.sh` sets a fresh server up, once.
- `aishiteru-deploy` puts an image on the server. It is used for the first
  start, for every upgrade and for a rollback, by hand or by the Deploy
  workflow.
- `aishiterud` runs a one-off command with the image that is running.

## The server

- Ubuntu 24.04 or later, 2 CPUs, 4 GB of memory and 40 GB of disk, to start
  with. It keeps grades and students' work, so pick a provider and a region
  your institution allows for that.
- A DNS name for it, such as `lms-staging.example.edu`, pointing at its
  address.
- Ports 22 (SSH), 80 and 443 open. The Deploy workflow connects on 22 from
  GitHub's runners, so 22 is open to the internet. The only key it accepts
  there can run `aishiteru-deploy` and nothing else.

## Setting a server up

1. Copy `deploy/` to the server, and run the set-up as root, with the
   server's name and its environment:

   ```
   scp -r deploy you@lms-staging.example.edu:
   ssh you@lms-staging.example.edu
   sudo -i
   sh ~you/deploy/setup-server.sh lms-staging.example.edu staging
   ```

   It installs Docker, PostgreSQL and Caddy. It creates the database, and the
   env file with a generated database password and `SIGNING_KEY`. It creates
   the data and backup directories and a nightly backup, and installs the two
   scripts. It points Caddy at the name, which gets a certificate as soon as
   the name resolves to the server. Last, it creates the SSH user `deploy`
   for the Deploy workflow. Running it again leaves what is already there as
   it is. Keep a copy of the env file somewhere safe: `SIGNING_KEY` must not
   change, or every upload and download link already given out stops
   working.

2. Let the server pull the image. The package is private. On GitHub, go to
   Settings → Developer settings → Personal access tokens → Tokens (classic)
   and make a token with `read:packages` only. GitHub's registry does not take
   fine-grained tokens. Give it a long expiry and put the date in a calendar:
   once it expires, deploys fail at the pull, and the running version is not
   touched. Then, as root:

   ```
   docker login ghcr.io -u <your GitHub user name>
   ```

3. Start it. Every green push to `main` publishes
   `ghcr.io/aishiteru-lms/aishiteru-core:sha-<commit>`: the Publish job's
   summary in the Actions tab names it, and so does the package's page. A
   release publishes `:X.Y.Z`. Production takes only releases.

   ```
   aishiteru-deploy ghcr.io/aishiteru-lms/aishiteru-core:sha-de4f548
   ```

4. Create the first administrator. `bootstrap` prints the administrator's API
   token once, so keep it in a password manager. Then restart, so that the
   background jobs start: they run as the system actor, which `bootstrap`
   creates.

   ```
   read -rsp 'Password (10 characters or more): ' PW; echo
   printf '%s\n' "$PW" | aishiterud bootstrap --name "Your Name" --email you@example.edu --password-stdin; unset PW
   docker restart aishiteru
   curl https://lms-staging.example.edu/healthz
   ```

## Connecting the Deploy workflow

`setup-server.sh` ends by printing three settings. Add them in the
repository's Settings → Secrets and variables → Actions:

| Kind | Name | Value |
| --- | --- | --- |
| Variable | `DEPLOY_TARGET_STAGING` | `deploy@lms-staging.example.edu` |
| Variable | `DEPLOY_KNOWN_HOSTS_STAGING` | the server's host key line, as printed |
| Secret | `DEPLOY_SSH_KEY_STAGING` | the whole of `/root/aishiteru-deploy-key` |

Then delete `/root/aishiteru-deploy-key` from the server. The server keeps
only the public half, in `~deploy/.ssh/authorized_keys`.

For production, the names end in `_PRODUCTION`. SSH on a port other than 22
is `ssh://deploy@host:2222` in the target and `[host]:2222 ssh-ed25519 …` in
the host key line.

From then on, every green push to `main` deploys to staging, and a
pre-release tag (`v1.2.3-rc.1`) does too. To try the connection without a
push, go to Actions → Deploy → Run workflow, from `main`, with environment
`staging` and image `ghcr.io/aishiteru-lms/aishiteru-core:edge`. Production is
deployed only by running Deploy by hand, from a release's tag
([CONTRIBUTING.md](../CONTRIBUTING.md#releasing)).

Anyone with write access to the repository can run a workflow that uses these
secrets. On GitHub Free, nothing narrows that down to a branch or to people.
The key still does only one thing, deploying an image of this repository, but
it can deploy any such image. Treat write access as deploy access.

## Day to day

Run all of these as root on the server.

- **Logs:** `docker logs -f aishiteru`. Each request is one JSON line, and
  Docker keeps the last 100 MB.
- **What is deployed:** `curl -s 127.0.0.1:8080/healthz` and
  `aishiterud migrate version`. `/var/log/aishiteru-deploy.log` lists every
  deploy, from what to what.
- **Changing the configuration:** edit `/etc/aishiteru/aishiteru.env`, then
  deploy the image that is running again. A restart does not re-read the
  file.

  ```
  aishiteru-deploy "$(docker inspect -f '{{.Config.Image}}' aishiteru)"
  ```

  A web front end on another origin needs `TRUSTED_ORIGINS=https://app.example.edu`.
  Give the origin only: no path, and no `/` at the end, or the server will
  not start. If the front end is on another site altogether, such as
  `*.vercel.app`, it also needs `COOKIE_SAMESITE=none`. Single sign-on is
  `OIDC_*` (README, Single sign-on).
- **An agent's token:** register the agent with the administrator's token,
  then issue its token by the id that comes back. `--actor` with your own
  email issues a token for you.

  ```
  curl -X POST https://lms-staging.example.edu/v1/actors \
    -H "Authorization: Bearer $ADMIN_TOKEN" -H "Idempotency-Key: register-grader-bot" \
    -H 'Content-Type: application/json' -d '{"kind":"agent","display_name":"grader-bot"}'
  aishiterud token issue --actor <result.actor_id> --label grader-bot --days 90
  ```

  Then seat it in a course (`member.add`, preset `grader` or `tutor`), and
  point its MCP client at `https://lms-staging.example.edu/mcp`.
- **Disk:** `docker image prune -a` removes the images no container uses. A
  rollback pulls its image again.

## When something goes wrong

- **A deploy failed before the new version started.** For example, the pull
  was refused or a migration failed. The old version is still running.
  `aishiteru-deploy` said which step failed, and the workflow's log has the
  same.
- **A migration failed.** `aishiteru-deploy` stops with
  `Dirty database version N`. The old version keeps serving, but `/healthz`
  answers 503 until this is put right. Each migration runs in one
  transaction, so a failed one has usually left nothing behind. Fix the
  cause the error names, record the migration before it as the last one
  applied, and deploy again:

  ```
  aishiterud migrate force <N - 1>
  aishiteru-deploy <the same image>
  ```

  If you are not sure what the failed migration left, restore the backup
  instead.
- **Rolling back** to the release before is a deploy of its image. The new
  schema is left as it is, and the release before works with it. Never run
  `migrate down`: it deletes data. Going back further than one release means
  restoring a backup.

  ```
  aishiteru-deploy ghcr.io/aishiteru-lms/aishiteru-core:1.2.2
  ```

- **Restoring a backup.** `aishiteru-deploy` takes one before every deploy
  (`/var/backups/aishiteru/deploy-*.dump`, the last ten), and cron takes one
  every night (`daily-1.dump` to `daily-7.dump`). Everything written after
  the backup is lost. Deploy the image that was running when the backup was
  taken, which `/var/log/aishiteru-deploy.log` tells you:

  ```
  docker stop aishiteru
  runuser -u postgres -- dropdb aishiteru
  runuser -u postgres -- createdb -O aishiteru aishiteru
  runuser -u postgres -- pg_restore --exit-on-error -d aishiteru /var/backups/aishiteru/<file>.dump
  aishiteru-deploy <the image from then>
  ```

## Backups off the server

The backups above sit on the same disk as the database. Copy these somewhere
else regularly:

- `/var/backups/aishiteru/`, the database;
- `/srv/aishiteru/data/`, the uploaded files;
- `/etc/aishiteru/aishiteru.env`, which holds `SIGNING_KEY` and the database
  password.

## More than one server per environment

Two servers behind a load balancer need more than this set-up gives:

- files in S3 (`BLOB_STORE=s3` and the `S3_*` settings);
- the same `SIGNING_KEY` on every server;
- `HTTP_ADDR` that the load balancer can reach;
- `TRUSTED_PROXIES` set to the load balancer's addresses;
- the database on a server of its own.

`aishiteru-deploy` and the Deploy workflow handle one server per
environment.

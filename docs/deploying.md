# Deploying AIShiteru Core

One server per environment: staging first, production when staging has
earned it. On each server, [Caddy](https://caddyserver.com) serves HTTPS and
hands requests to the `aishiterud` container on `127.0.0.1:8080`, and
PostgreSQL runs on the same machine. The files people upload are kept under
`/srv/aishiteru/data`, and the configuration is in
`/etc/aishiteru/aishiteru.env`.

The scripts in [`deploy/`](../deploy) do the work:

- `setup-server.sh` sets a server up. Run again, it installs newer copies of
  the other two scripts and leaves everything else as it is.
- `aishiteru-deploy` puts an image on the server. It is used for the first
  start, for every upgrade and for a rollback, by hand or by the Deploy
  workflow.
- `aishiterud` runs a one-off command with the image that is running.

## The server

- Ubuntu 24.04 or later, 2 CPUs, 4 GB of memory and 40 GB of disk, to start
  with. It keeps grades and students' work, so pick a provider and a region
  your institution allows for that.
- A DNS name for it, such as `lms-staging.example.edu`, pointing at its
  address. If the name's DNS is on Cloudflare, make the record "DNS only":
  SSH does not go through Cloudflare's proxy.
- Ports 22 (SSH), 80 and 443 open. The Deploy workflow connects on 22 from
  GitHub's runners, so 22 is open to the internet. The only key it accepts
  there can run `aishiteru-deploy` and nothing else. Some providers turn ufw
  on in their images (Vultr does); `setup-server.sh` opens the three ports in
  it. A firewall in the provider's console must allow them too.
- Log in to it with a key, not a password: port 22 is open to everyone.
  `setup-server.sh` warns when SSH still takes passwords. Once your key works,
  put `PasswordAuthentication no` in a file in `/etc/ssh/sshd_config.d/` and
  run `systemctl restart ssh`.

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
   for the Deploy workflow.

   If it stops, fix what it names and run it again. A server that has just
   booted may still be updating itself, and the script waits for that.

   Keep a copy of the env file somewhere safe. `SIGNING_KEY` must not change,
   or every upload and download link already given out stops working.

2. Let the server pull the image. The package is private, and GitHub's
   registry takes only a personal access token (classic), not a fine-grained
   one. Make the token with `read:packages` only. A classic token reads every
   package its owner can read, and Docker keeps it unencrypted in
   `/root/.docker/config.json`. So make it on an account of its own, one that
   can read this repository and nothing else. Give it a long expiry and put
   the date in a calendar. Once it expires, deploys fail at the pull, and the
   running version is not touched. Then, as root:

   ```
   docker login ghcr.io -u <that account's user name>
   ```

3. Start it. Every green push to `main` publishes
   `ghcr.io/aishiteru-lms/aishiteru-core:sha-<commit>`: the CI run's
   `publish / image` job names it, and so does the package's page. A release
   publishes `:X.Y.Z`. Production takes only releases.

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

5. The day after, check that the nightly backup ran:
   `ls -l /var/backups/aishiteru/daily-*`.

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
`staging` and image `ghcr.io/aishiteru-lms/aishiteru-core:edge`. That is also
the way to deploy staging again: re-running an older run's deploy does
nothing once `main` has moved on. Production is deployed only by running
Deploy by hand, from a release's tag
([CONTRIBUTING.md](../CONTRIBUTING.md#releasing)).

The key only runs `aishiteru-deploy`, but that script deploys any image of
this repository. Anyone with write access to the repository can run a
workflow that reads the secret, or copy the key out. They can also push an
image of their own under this repository's name and deploy it. On GitHub
Free, nothing narrows that down to a branch or to people: write access is
access to everything on the servers. When someone loses write access,
replace the key and delete any package versions they pushed.

To replace the key: on the server, delete `~deploy/.ssh/authorized_keys` and
any `/root/aishiteru-deploy-key*` left, run `setup-server.sh` again, and put
the new key it prints into the secret.

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

  The file is one `NAME=value` per line: no quotes, no `export`, and no
  comment after a value. Docker takes quotes and comments as part of the
  value.

  A web front end works best on the same site as the server, such as
  `app.example.edu` next to `lms.example.edu`. It then needs only
  `TRUSTED_ORIGINS=https://app.example.edu`. Give the origin only: no path,
  and no `/` at the end, or the server will not start. A front end on
  another site altogether, such as `*.vercel.app`, also needs
  `COOKIE_SAMESITE=none`. Safari, and every browser on iOS, still refuses
  that sign-in cookie. Single sign-on is `OIDC_*` (README, Single sign-on).
- **An agent's token:** register the agent with the administrator's token,
  then issue its token by the id that comes back. `--actor` with your own
  email issues a token for you.

  ```
  curl -X POST https://lms-staging.example.edu/v1/actors \
    -H "Authorization: Bearer $ADMIN_TOKEN" -H "Idempotency-Key: register-grader-bot" \
    -H 'Content-Type: application/json' -d '{"kind":"agent","display_name":"grader-bot"}'
  aishiterud token issue --actor <result.actor_id> --label grader-bot --days 90
  ```

  Then an instructor of the course seats it: `member.add`, that is
  `POST /v1/courses/{course_id}/members`, with preset `grader` or `tutor`.
  The administrator is no member of the course and cannot. The administrator
  seats the instructor first, with `course.seat_instructor`. Point the agent's
  MCP client at `https://lms-staging.example.edu/mcp`.
- **Updating the scripts:** when `deploy/` changes, copy it to the server
  again and run `setup-server.sh` as in step 1. It installs the new scripts
  and leaves the rest.
- **Disk:** after each deploy, `aishiteru-deploy` removes this project's
  images that no container uses. A rollback pulls its image again.

## When something goes wrong

- **A deploy failed before the new version started.** For example, the pull
  was refused or a migration failed. The old version is still running. The
  last lines of `aishiteru-deploy`'s output, and of the workflow's log, name
  the step and its error.
- **A migration failed.** `aishiteru-deploy` stops at `migrate up` with the
  migration's error. The old version keeps serving, but `/healthz` answers
  503 until this is put right. `aishiterud migrate version` shows
  `schema version N … DIRTY`, N being the migration that failed. Each
  migration runs in one transaction, so a failed one has usually left
  nothing behind. Fix the cause the error names, record the migration before
  it as the last one applied, and deploy again:

  ```
  aishiterud migrate force <N - 1>
  aishiteru-deploy <the same image>
  ```

  If you are not sure what the failed migration left, restore the backup
  instead.
- **The new version did not report healthy.** `aishiteru-deploy` printed the
  new container's last log lines. If another version was running before, it
  is running again. If the same image was deployed again, most likely after
  a change to the env file, nothing is running: fix the file and deploy that
  image again.
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

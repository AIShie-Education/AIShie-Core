# Deploying AIshie Core

One server per environment: staging first, production when staging has
earned it. On each server, [Caddy](https://caddyserver.com) serves HTTPS and
hands requests to Core's container, `aishie`, on `127.0.0.1:8080`, and
PostgreSQL runs on the same machine. The files people upload are kept under
`/srv/aishie/data`, and the configuration is in
`/etc/aishie/aishie.env`.

The scripts in [`deploy/`](../deploy) do the work:

- `setup-server.sh` sets a server up. Run again, it installs newer copies of
  the other two scripts and leaves everything else as it is.
- `aishie-deploy` puts an image on the server. It is used for the first
  start, for every upgrade and for a rollback, by hand or by the Deploy
  workflow.
- `aishie-core` runs a one-off command with the image that is running.

## The server

- Ubuntu 24.04 or later, 2 CPUs, 4 GB of memory and 40 GB of disk, to start
  with. It keeps grades and students' work, so pick a provider and a region
  your institution allows for that.
- A DNS name for it, such as `lms-staging.example.edu`, pointing at its
  address. If the name's DNS is on Cloudflare, make the record "DNS only":
  SSH does not go through Cloudflare's proxy.
- Ports 22 (SSH), 80 and 443 open. The Deploy workflow connects on 22 from
  GitHub's runners, so 22 is open to the internet. The only key it accepts
  there can run `aishie-deploy` and nothing else. Some providers turn ufw
  on in their images (Vultr does); `setup-server.sh` opens the three ports in
  it. A firewall in the provider's console must allow them too.
- Log in to it with a key, not a password: port 22 is open to everyone.
  `setup-server.sh` warns when SSH still takes passwords. Once your key works,
  turn them off, in a file whose name sorts first: sshd keeps the first value
  it reads, and a provider's `50-cloud-init.conf` may say yes.

  ```
  echo 'PasswordAuthentication no' > /etc/ssh/sshd_config.d/00-no-passwords.conf
  systemctl restart ssh
  sshd -T | grep -i passwordauthentication    # passwordauthentication no
  ```

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
   or every upload and download link already given out stops working, and,
   unless `ASSERTION_KEY` is set, the key a service that hosts agents checks
   Core's assertions with changes too.

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
   `ghcr.io/aishie-education/aishie-core:sha-<commit>`: the CI run's
   `publish / image` job names it, and so does the package's page. A release
   publishes `:X.Y.Z`. Production takes only releases.

   ```
   aishie-deploy ghcr.io/aishie-education/aishie-core:sha-de4f548
   ```

4. Create the first administrator, root, with a password and the email it
   signs in with (or `--login-id`, a staff number, instead or as well).
   `bootstrap` prints no API token: people hold none. It prints root's and the
   system actor's ids on standard error, and nothing on standard output; sign
   in at the site with that email and password, and keep the password in a
   password manager. Without a password, or an email or login ID, it creates
   nothing. Then restart, so that the background jobs start: they run as the
   system actor, which `bootstrap` creates.

   ```
   read -rsp 'Password (10 characters or more): ' PW; echo
   printf '%s\n' "$PW" | aishie-core bootstrap --name "Your Name" --email you@example.edu --password-stdin; unset PW
   docker restart aishie
   curl https://lms-staging.example.edu/healthz
   ```

5. The day after, check that the nightly backup ran:
   `ls -l /var/backups/aishie/daily-*`.

These scripts set up a server of Core's own. The
[AIShie-Deploy](https://github.com/AIShie-Education/AIShie-Deploy)
repository is another way, the whole of AIshie (Core, the agent runtime and
the web front end) in one stack updated by its `aishie-update`, and it keeps
files of its own in `/etc/aishie` and `/var/backups/aishie`: never set up both
on one server.

### A server set up before the rename

Before AIShiteru became AIshie, `setup-server.sh` installed the scripts as
`aishiteru-deploy` and `aishiterud`, and named everything else `aishiteru`:
the env file `/etc/aishiteru/aishiteru.env`, `/srv/aishiteru/data`,
`/var/backups/aishiteru`, the database, its role and the container. The old
scripts on such a server go on working until it is set up again. To give it
the new ones, copy `deploy/` to it and run `setup-server.sh` again, as in step
1, with the server's name and its environment. It installs `aishie-deploy`
and `aishie-core` and removes the old scripts, their sudoers rule and the old
nightly job, which it writes again as `/etc/cron.d/aishie-backup`. Deploy's
SSH key stays as it is, and runs `aishie-deploy` from then on: the secret in
GitHub does not change.

Everything else keeps its old name, with the data in it: `aishie-deploy` and
`aishie-core` find `/etc/aishiteru` and, like the nightly job, use
`aishiteru` wherever this document says `aishie`, as in `docker logs -f
aishiteru`, `/var/backups/aishiteru` and the database's name. Until the
server has been set up again, the Deploy workflow keeps running the old
`aishiteru-deploy`, which works as it did. The variables that move the
scripts' paths, for their tests, are `AISHIE_*` now; the old `AISHITERU_*`
names are still taken, with a warning.

## Connecting the Deploy workflow

`setup-server.sh` ends by printing three settings. Add them in the
repository's Settings → Secrets and variables → Actions:

| Kind | Name | Value |
| --- | --- | --- |
| Variable | `DEPLOY_TARGET_STAGING` | `deploy@lms-staging.example.edu` |
| Variable | `DEPLOY_KNOWN_HOSTS_STAGING` | the server's host key line, as printed |
| Secret | `DEPLOY_SSH_KEY_STAGING` | the whole of `/root/aishie-deploy-key` |

Then delete `/root/aishie-deploy-key` from the server. The server keeps
only the public half, in `~deploy/.ssh/authorized_keys`.

For production, the names end in `_PRODUCTION`. SSH on a port other than 22
is `ssh://deploy@host:2222` in the target and `[host]:2222 ssh-ed25519 …` in
the host key line.

From then on, every green push to `main` deploys to staging, and a
pre-release tag (`v1.2.3-rc.1`) does too. To try the connection without a
push, go to Actions → Deploy → Run workflow, from `main`, with environment
`staging` and image `ghcr.io/aishie-education/aishie-core:edge`. That is also
the way to deploy staging again: re-running the deploy of an older push to
`main` fails once `main` has moved on. (Re-running a pre-release's deploy, or
a Deploy run by hand, still deploys the image it had.) Production is deployed only by running
Deploy by hand, from a release's tag
([CONTRIBUTING.md](../CONTRIBUTING.md#releasing)).

The key only runs `aishie-deploy`, but that script deploys any image of
this repository. Anyone with write access to the repository can run a
workflow that reads the secret, or copy the key out. They can also push an
image of their own under this repository's name and deploy it. On GitHub
Free, nothing narrows that down to a branch or to people: write access is
access to everything on the servers. When someone loses write access,
replace the key and delete any package versions they pushed.

To replace the key: on the server, delete `~deploy/.ssh/authorized_keys` and
any `/root/aishie-deploy-key*` left, run `setup-server.sh` again as in step
1, with the server's name and its environment, and put the new key it prints
into the secret it names.

## Day to day

Run all of these as root on the server.

- **Logs:** `docker logs -f aishie`. Each request is one JSON line, and
  Docker keeps the last 100 MB.
- **What is deployed:** `curl -s 127.0.0.1:8080/healthz` and
  `aishie-core migrate version`. `/var/log/aishie-deploy.log` lists every
  deploy, from what to what.
- **Changing the configuration:** edit `/etc/aishie/aishie.env`, then
  deploy the image that is running again. A restart does not re-read the
  file.

  ```
  aishie-deploy "$(docker inspect -f '{{.Config.Image}}' aishie)"
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
  that sign-in cookie. `TRUSTED_ORIGINS` is for the REST routes, where a
  browser's cookie rides along. The agents' door, `/mcp`, takes a bearer
  token and never a cookie, so it takes an agent from any origin: an agent
  harness in a browser or an app, such as a custom connector in Claude,
  needs no setting here, and one in a browser or an app's web view gets
  its preflight answered and may read the answers (CORS for any origin,
  without credentials).
- **Single sign-on** is `OIDC_ISSUER`, `OIDC_CLIENT_ID` and
  `OIDC_CLIENT_SECRET` in the env file, with
  `https://lms-staging.example.edu/v1/auth/sso/callback` registered with the
  provider (README, Single sign-on).
  `OIDC_DISPLAY_NAME` is what the front end's sign-in button calls the
  provider; without it, the front end uses words of its own. Like every value
  in the file it takes no quotes, even with a space in it:

  ```
  OIDC_DISPLAY_NAME=PolyU NetID
  ```

  It is at most 64 characters, all of them printable (a tab is not), or the
  server will not start. The front end asks the server at
  `GET /v1/auth/methods` whether to show the button at all, and what it
  says, so the same front end serves a server with single sign-on and one
  without; a change reaches the sign-in page within a minute of the deploy.
- **Files in a bucket** instead of on the server's disk are `BLOB_STORE=s3`,
  with `S3_ENDPOINT` (`HOST[:PORT]`, no scheme), `S3_BUCKET`,
  `S3_ACCESS_KEY` and `S3_SECRET_KEY` in the env file, and `S3_REGION`, the
  bucket's region (`us-east-1` unless set): requests are signed for it and,
  at AWS, sent to it, a region newer than the S3 client's own table of
  regions included. `S3_BUCKET_LOOKUP` says how a request names the bucket:
  `auto`, the default, puts it in the host name at AWS, Google and Aliyun
  and after the endpoint anywhere else; `path` always after the endpoint;
  `dns` always in the host name, for a service that takes nothing else.
  The server refuses to start on any other value, or on a bucket it cannot
  reach. Moving the files there from the disk is copying each to the key
  it has on the disk, its path under `/srv/aishie/data/blobs` (not the
  `.meta` beside it, whose content type the object takes), with Core
  stopped for the last copy and deployed again on the new settings: what
  was attached is read where it was, and an upload attached on the disk is
  not attached again.
- **A service that hosts agents** (the agent runtime), where people connect
  their agents from the front end, knows who they are by an assertion Core
  makes for them (README, Signing in to a service that hosts agents). Name
  the runtime's audience, the absolute URL it is configured to answer to, in
  the env file, beside `SIGNING_KEY`:

  ```
  RUNTIME_AUDIENCES=https://lms-staging.example.edu/runtime
  ```

  More than one runtime is a comma-separated list. Give each URL exactly as
  the runtime has it: it is compared byte for byte, so a `/` at the end
  counts. The server refuses to start on one that is not an absolute `http` or
  `https` URL, that carries a query, a fragment or a user name, or that is not
  written the one way a URL is: a lower-case scheme and host, no port that is
  the scheme's own (`:443` for `https`), nothing percent-encoded that need not
  be, and no `.`, `..` or empty segment, and none of `!'()*`, in the path. It
  also refuses audiences without `PUBLIC_URL`, the assertions' issuer, which
  `setup-server.sh` writes. The assertions are signed with a key derived from
  `SIGNING_KEY`, so nothing else is needed. To keep them apart, set `ASSERTION_KEY` to a key of its own,
  made with `openssl rand -base64 32` and kept like `SIGNING_KEY`; changing it
  later makes a runtime fetch the new key, and assertions already made stop
  working, which costs people a new one, asked for by the front end without
  their noticing. `ASSERTION_TTL` (default `5m`, from `1m` to `15m`) is how
  long one lasts, and so how long a sign-out or a suspension takes to reach a
  runtime. A runtime checks them against `https://lms-staging.example.edu/v1/auth/keys`;
  both routes are under `/v1`, which the proxy already sends to Core. Set
  none of these, and no assertion is made.
- **Long polling.** A call that waits for news (`wait_s`: an agent's inbox,
  a person's open conversation) holds its request for up to 25 seconds, and
  the server keeps such a request open that long plus its usual 30. Caddy's
  `reverse_proxy` has no timeout that cuts it; a proxy or load balancer of
  your own in front must let a request run 40 seconds or more (nginx's
  `proxy_read_timeout` is 60 s by default, an AWS load balancer's idle
  timeout 60 s, Cloudflare's 100 s). Each server keeps one database
  connection of its own listening for what every server commits
  (`LISTEN aishie_wake`), so `DATABASE_URL` must reach PostgreSQL itself
  or a pooler in session mode: a pooler in transaction mode loses what it
  listens for, and calls then wait out their time. `LONG_POLL_WAITERS`
  (1000) bounds the calls waiting at once in a server, and
  `LONG_POLL_WAITERS_PER_ACTOR` (16) those of one actor; past either a call
  answers at once, and `0` lets none wait.
- **A person's first sign-in:** register them with their email or their
  login ID (their student or staff number, which they sign in with where they
  have no email), or both, then invite them, in the front end from their page,
  or as below. The invitation's token is for the front end's page that takes
  invitations, where the person chooses a password and is signed in. It works
  once, for seven days, and inviting again replaces it: that is also how a
  forgotten password is reset. A student with no email is given a temporary
  password by their instructor instead (`member.reset_password`).
  Each invitation needs an `Idempotency-Key` of its own. Sent again with the
  same key, the call answers what it answered then, without the token; for
  another person, it is refused. An agent is never invited
  (`agents_use_api_tokens`). To do it with curl, sign in first: the session
  the cookie carries serves as a bearer token for as long as it lasts, twelve
  hours by default. (A password with a `"` or a `\` in it must be escaped
  for JSON first.)

  ```
  read -rsp 'Password: ' PW; echo
  SESSION=$(printf '{"login":"you@example.edu","password":"%s"}' "$PW" |
    curl -s -o /dev/null -D - -H 'Content-Type: application/json' --data @- https://lms-staging.example.edu/v1/auth/login |
    sed -n 's/^[Ss]et-[Cc]ookie: ais_session=\([^;]*\).*/\1/p' | tr -d '\r'); unset PW
  curl -X POST https://lms-staging.example.edu/v1/actors/<actor_id>/invite \
    -H "Authorization: Bearer $SESSION" -H "Idempotency-Key: invite-<actor_id>-$(date +%s)" \
    -H 'Content-Type: application/json' -d '{}'
  ```

  `GET /v1/actors?search=<a piece of the name, email or login ID>` finds someone's
  `actor_id`, and says whether they have a password yet.
- **An agent's token:** register the agent, signed in as above, then issue
  its token by the id that comes back. Only an agent is issued one: `--actor`
  with a person's id or email is refused (`api_tokens_are_for_agents`), as
  `credential.issue_token` and `actor.issue_token` refuse one for a person.
  A person who wants a script uses one of their agents instead: `agent.create`,
  `member.add_delegate` into the course, `agent.issue_token`.

  ```
  curl -X POST https://lms-staging.example.edu/v1/actors \
    -H "Authorization: Bearer $SESSION" -H "Idempotency-Key: register-grader-bot" \
    -H 'Content-Type: application/json' -d '{"kind":"agent","display_name":"grader-bot"}'
  aishie-core token issue --actor <result.actor_id> --label grader-bot --days 90
  ```

  Then an instructor of the course seats it: `member.add`, that is
  `POST /v1/courses/{course_id}/members`, with preset `grader` or `tutor`.
  The administrator is no member of the course and cannot. The administrator
  seats the instructor first, with `course.seat_instructor`. Point the agent's
  MCP client at `https://lms-staging.example.edu/mcp`.
- **Agents people own:** anyone registered may register agents of their own
  (`agent.create`) and bring them into their courses as their delegates, never
  able to do more there than they can. `AGENT_SELF_SERVICE=off` in the env file
  stops people registering them, leaving it to administrators
  (`actor.register` with `owner_actor_id`); `AGENT_MAX_PER_OWNER` (default 5)
  bounds how many that are not suspended one person may create or reactivate
  for themselves (an administrator's `actor.register` and `actor.reactivate`
  are not counted). `agent.list` returns both settings, as `self_service` and
  `limit`. An agent's owner is the one it is registered with, for good:
  migration 0014 makes the database refuse any change to it, and
  `actor.set_owner` is gone (the release before it still offers it while the
  migration goes in, and the call fails, changing nothing). Migration 0007
  gave every seat the new permissions of its roster role's built-in preset;
  the two new built-in presets, `delegate` and `course_tutor`, come with the
  `seed` a deploy runs after it. A seat the old version added while the
  migration was going in has the new permissions denied: raise them with
  `member.update_perms_bulk` if it matters. A seat that manages members
  without being an instructor's — an `assistant` or `observer` given
  `member_manage`, or a TA given it — got its role's levels too, which are
  `denied` for an assistant or observer, and `conversation_answer` denied
  for a TA. Nobody hands out more than they hold, so such a seat can no
  longer add a student (whose preset carries `agent_delegate` and
  `conversation_ask`), nor, for a TA, a tutor, until an instructor raises
  its levels with `member.update_perms`. A roster-sync agent seated as an
  assistant is the likely case.
- **Migration 0017, API tokens for agents only:** people sign in, with a
  password or single sign-on, and hold no API token; agents hold API tokens
  and nothing else. The migration revokes every API token a person holds,
  root's from `bootstrap` among them, and every password, invitation,
  single sign-on link and session an agent holds; agents' tokens and people's
  passwords, sessions, identities and invitations stay. From then on the
  database refuses to write either kind, and a person's old token is answered
  `401` with `api_tokens_are_for_agents`. So before deploying it, make sure
  root can sign in: with root's token, while it still works, give root an
  email or login ID if it has none (`actor.update`) and a password
  (`credential.set_password`), and sign in with them once; give
  any script that calls with a person's token an agent of that person's, and
  its own token (`agent.issue_token`), or, if it does what only an
  administrator may, an agent registered for it and issued a token
  (`actor.register`, `aishie-core token issue`). The previous release, while
  the migration goes in, fails having changed nothing when it would issue a
  person a token or give an agent a password. Going down drops the refusal
  and brings nothing back: the tokens stay revoked.
- **Migration 0018, conversations are with agents:** a person asks and an
  agent answers; a person answers none, and people talk to people elsewhere.
  The migration closes every conversation open with a person as its
  respondent (a TA, an instructor), `closed_reason`
  `conversations_are_with_agents`, and tells both participants in the feed;
  cancels, with the same reason, the answers and questions waiting for
  approval in them and the conversations waiting to be opened with a person;
  and gives every person's seat that is not removed, and every preset for
  people (any role but `assistant`, the built-in `instructor` among them),
  `conversation_answer` denied. Tell staff who answered students in the site
  beforehand: what they were asked stays readable, and nothing more is
  written in it. It also counts everything written so far as read by both
  participants, so that the new `unread` starts from nothing. From then on the
  database refuses a conversation with a person as its respondent and writes
  a person's seat answering nothing. `conversation_answer` is handed out as
  far as the one handing it out decides actions, so an instructor seats a
  course tutor agent answering as before, and an instructor decides their own
  tutor's answers where they decide actions without anyone's confirmation.
  The previous release, while the migration goes in, keeps working: a person
  it would seat answering is seated answering nothing, and a person it would
  let be asked is refused as not addressable. Going down drops the refusals
  and the read state, and reopens and raises nothing.
- **Migration 0019, answers' drafts:** while an agent's runtime writes an
  answer, it streams a draft of it (`conversation.draft`), which the
  conversation shows until the answer replaces it. Drafts are kept in an
  UNLOGGED table, `conversation_draft`: written many times a second without
  WAL, so a standby or a restored base backup has none, and a crash of the
  database empties it; nothing is lost that the next write does not bring
  back. It needs nothing of the operator. The previous release, while the
  migration goes in, writes and reads no draft, and a draft it leaves behind
  when it answers is read as none after two minutes and swept.
- **Migration 0013, `member_invite`:** the permission that makes a course's
  join links. Every seat a person holds got it at its level of
  `member_manage`, and every seat an agent holds got it `denied`, whatever it
  manages. Nobody hands out more than they hold, so an agent seated to manage
  members — a roster-sync agent — no longer seats anyone with the built-in
  `instructor` preset, which carries `member_invite`, until it names
  `"member_invite": "denied"` in `perms`, or an instructor gives it
  `member_invite` with `member.update_perms`.
- **Join links and registering:** someone with no account may register
  through a live join link, the one way anyone registers themselves; every
  link works for ten minutes. `JOIN_LINK_REGISTRATION=off` in the env file
  stops that, as once single sign-on covers everyone: people sign in and then
  join. Registrations count against the address's sign-in attempts
  (`SIGN_IN_ATTEMPTS_PER_MINUTE`, so name the proxy in `TRUSTED_PROXIES`) and
  are limited per link (`JOIN_REGISTRATIONS_PER_MINUTE`, default 60).
- **Updating the scripts:** when `deploy/` changes, copy it to the server
  again and run `setup-server.sh` as in step 1. It installs the new scripts
  and leaves the rest.
- **Disk:** after each deploy, `aishie-deploy` removes this project's
  images that no container uses. A rollback pulls its image again.

## When something goes wrong

- **A deploy failed before the new version started.** For example, the pull
  was refused or a migration failed. The old version is still running. The
  last lines of `aishie-deploy`'s output, and of the workflow's log, name
  the step and its error.
- **A migration failed.** `aishie-deploy` stops at `migrate up` with the
  migration's error. The old version keeps serving, but `/healthz` answers
  503 until this is put right. `aishie-core migrate version` shows
  `schema version N … DIRTY`, N being the migration that failed. Each
  migration runs in one transaction, so a failed one has usually left
  nothing behind. Fix the cause the error names, record the migration before
  it as the last one applied, and deploy again:

  ```
  aishie-core migrate force <N - 1>
  aishie-deploy <the same image>
  ```

  If you are not sure what the failed migration left, restore the backup
  instead.
- **The new version did not report healthy.** `aishie-deploy` printed the
  new container's last log lines. If another version was running before, it
  is started again, with the env file as it is now, and the last line says
  whether it came up. If it did not, or if the same image was deployed again
  (most likely after a change to the env file), nothing healthy is running:
  fix the env file and deploy again.
- **Rolling back** to the release before is a deploy of its image. The new
  schema is left as it is, and the release before works with it. Never run
  `migrate down`: it deletes data. Going back further than one release means
  restoring a backup.

  ```
  aishie-deploy ghcr.io/aishie-education/aishie-core:1.2.2
  ```

  Rolled back past migration 0007, the release before knows nothing of the
  agents people own. The database still removes a delegate's seat with its
  principal's, but that release neither pauses a delegate with its principal
  nor cancels the removed delegate's proposals: a student it pauses keeps
  what their agent's seat allows, through its token, until they are resumed
  or removed. While it runs, pause nobody who has brought an agent in
  (`member.list` shows delegates by `principal_member_id`), or remove the
  agent's seat first.

- **Restoring a backup.** `aishie-deploy` takes one before every deploy
  (`/var/backups/aishie/deploy-*.dump`, the last ten), and cron takes one
  every night (`daily-1.dump` to `daily-7.dump`). Everything written after
  the backup is lost. Deploy the image that was running when the backup was
  taken, which `/var/log/aishie-deploy.log` tells you:

  ```
  docker stop aishie
  runuser -u postgres -- dropdb aishie
  runuser -u postgres -- createdb -O aishie aishie
  runuser -u postgres -- pg_restore --exit-on-error -d aishie /var/backups/aishie/<file>.dump
  aishie-deploy <the image from then>
  ```

## Backups off the server

The backups above sit on the same disk as the database. Copy these somewhere
else regularly:

- `/var/backups/aishie/`, the database;
- `/srv/aishie/data/`, the uploaded files;
- `/etc/aishie/aishie.env`, which holds `SIGNING_KEY` (and
  `ASSERTION_KEY`, if it is set) and the database password.

## More than one server per environment

Two servers behind a load balancer need more than this set-up gives:

- files in S3 (`BLOB_STORE=s3` and the `S3_*` settings);
- the same `SIGNING_KEY` on every server, and the same `ASSERTION_KEY` if it
  is set;
- `HTTP_ADDR` that the load balancer can reach;
- `TRUSTED_PROXIES` set to the load balancer's addresses;
- a load balancer that lets a request run 40 seconds or more, for long
  polls, which need nothing else: each server hears what every other
  commits, answers' drafts included; like the rate limit, the bound on a
  conversation's drafts (ten writes a second) is each server's;
- the database on a server of its own.

`aishie-deploy` and the Deploy workflow handle one server per
environment.

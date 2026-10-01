# Deploying AIshie Core

A site runs Core with the agent runtime and the web front end, as one stack
per server, from
[AIShie-Deploy](https://github.com/AIShie-Education/AIShie-Deploy), which
keeps itself up to date: that is the way to run one. This document is the
older way, a server of Core's own, deployed over SSH by this repository's
Deploy workflow, which the agent runtime and the web front end join by
scripts of their own repositories, the same way.

One server per environment: edge first, the test site every green push to
`main` reaches, and stable, the site a school runs on releases, when edge
has earned it. On each server, [Caddy](https://caddyserver.com) serves HTTPS
and hands requests to Core's container, `aishie`, on `127.0.0.1:8080`, and
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
- A DNS name for it, such as `lms-test.example.edu`, pointing at its
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
   scp -r deploy you@lms-test.example.edu:
   ssh you@lms-test.example.edu
   sudo -i
   sh ~you/deploy/setup-server.sh lms-test.example.edu edge
   ```

   It installs Docker, PostgreSQL and Caddy. It creates the database, and the
   env file with a generated database password, `SIGNING_KEY` and
   `SECRETS_KEY` (an env file from before `SECRETS_KEY` is given one, and
   nothing else in it changes). It creates
   the data and backup directories and a nightly backup, and installs the two
   scripts. It points Caddy at the name, which gets a certificate as soon as
   the name resolves to the server. Last, it creates the SSH user `deploy`
   for the Deploy workflow.

   If it stops, fix what it names and run it again. A server that has just
   booted may still be updating itself, and the script waits for that.

   Keep a copy of the env file somewhere safe. `SIGNING_KEY` must not change,
   or every upload and download link already given out stops working, and,
   unless `ASSERTION_KEY` is set, the key a service that hosts agents checks
   Core's assertions with changes too. `SECRETS_KEY` must not be lost: the
   client secrets of the identity providers administrators set up are sealed
   with it and open with nothing else (rotating it: below, Single sign-on).

2. Start it. The image is public: the server pulls it with no login. Every
   green push to `main` publishes
   `ghcr.io/aishie-education/aishie-core:sha-<commit>`: the CI run's
   `publish / image` job names it, and so does the package's page. A release
   publishes `:X.Y.Z`. Stable takes only releases.

   ```
   aishie-deploy ghcr.io/aishie-education/aishie-core:sha-de4f548
   ```

3. Create the first administrator, root, with a password and the email it
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
   curl https://lms-test.example.edu/healthz
   ```

4. The day after, check that the nightly backup ran:
   `ls -l /var/backups/aishie/daily-*`.

These scripts set up a server of Core's own. AIShie-Deploy's stack keeps
files of its own in `/etc/aishie` and `/var/backups/aishie` too: never set up
both on one server.

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
| Variable | `DEPLOY_TARGET_EDGE` | `deploy@lms-test.example.edu` |
| Variable | `DEPLOY_KNOWN_HOSTS_EDGE` | the server's host key line, as printed |
| Secret | `DEPLOY_SSH_KEY_EDGE` | the whole of `/root/aishie-deploy-key` |

Then delete `/root/aishie-deploy-key` from the server. The server keeps
only the public half, in `~deploy/.ssh/authorized_keys`.

For stable, the names end in `_STABLE`. SSH on a port other than 22 is
`ssh://deploy@host:2222` in the target and `[host]:2222 ssh-ed25519 …` in
the host key line. Settings added before edge and stable had those names
end in `_STAGING` and `_PRODUCTION`: they are read, with a warning, until a
later release ([below](#settings-from-before-the-rename)).

From then on, every green push to `main` deploys to edge, and a
pre-release tag (`v1.2.3-rc.1`) does too. To try the connection without a
push, go to Actions → Deploy → Run workflow, from `main`, with environment
`edge` and image `ghcr.io/aishie-education/aishie-core:edge`. That is also
the way to deploy edge again: re-running the deploy of an older push to
`main` fails once `main` has moved on. (Re-running a pre-release's deploy, or
a Deploy run by hand, still deploys the image it had.) Stable is deployed only by running
Deploy by hand, from a release's tag
([CONTRIBUTING.md](../CONTRIBUTING.md#releasing)).

The key only runs `aishie-deploy`, but that script deploys any image of
this repository. Anyone with write access to the repository can run a
workflow that reads the secret, or copy the key out. They can also push an
image of their own under this repository's name and deploy it. The secret
is the repository's, not an environment's, so no environment's rule narrows
that down to a branch or to people: write access is access to everything on
the servers. When someone loses write access,
replace the key and delete any package versions they pushed.

To replace the key: on the server, delete `~deploy/.ssh/authorized_keys` and
any `/root/aishie-deploy-key*` left, run `setup-server.sh` again as in step
1, with the server's name and its environment, and put the new key it prints
into the secret it names.

### Settings from before the rename

The environments were called `staging` and `production`, and are `edge` and
`stable` now. Deploy reads each of its settings by the new name first and,
until a later release that removes this, by the old one, with a warning in
the run that names the setting to add; so deploys go on while the settings
are renamed. In the repository's settings, before merging the rename if you
can:

1. **Environments** (Settings → Environments → New environment): make `edge`
   with the rules `staging` has, and `stable` with the rules `production`
   has: its required reviewers, and Deployment branches and tags (`edge`:
   branch `main` and tags `v*`; `stable`: tags `v*` only). **Give `stable`
   production's protection before its first deploy.** GitHub neither renames
   environments nor carries their rules over: the first run that names
   `stable` creates it with no protection at all, and then nothing but
   Deploy's own check that it runs from a stable release's tag stands
   between write access to this repository and the schools' sites.
2. **Variables and secrets** (Settings → Secrets and variables → Actions):
   add each one that is set under its new name, with the same value, then
   delete the old one.

   | Kind | Old name | New name |
   | --- | --- | --- |
   | Variable | `DEPLOY_TARGET_STAGING` | `DEPLOY_TARGET_EDGE` |
   | Variable | `DEPLOY_KNOWN_HOSTS_STAGING` | `DEPLOY_KNOWN_HOSTS_EDGE` |
   | Secret | `DEPLOY_SSH_KEY_STAGING` | `DEPLOY_SSH_KEY_EDGE` |
   | Variable | `DEPLOY_TARGET_PRODUCTION` | `DEPLOY_TARGET_STABLE` |
   | Variable | `DEPLOY_KNOWN_HOSTS_PRODUCTION` | `DEPLOY_KNOWN_HOSTS_STABLE` |
   | Secret | `DEPLOY_SSH_KEY_PRODUCTION` | `DEPLOY_SSH_KEY_STABLE` |

   A variable's value can be copied from its page. A secret's cannot be read
   back: paste the key from wherever a copy is kept or, with none, give the
   server a new key ([above](#connecting-the-deploy-workflow),
   to replace the key), which `setup-server.sh` prints under the new name.
3. Once a deploy to each environment runs without a warning, the
   environments `staging` and `production` can be deleted, with the
   deployments they recorded.

The Deploy form offers `edge` and `stable` alone, as GitHub takes nothing
but a choice's options there; a workflow that calls Deploy with `staging`
or `production` has them taken as `edge` and `stable`, with a warning.
Servers need nothing: `deploy/setup-server.sh` takes `edge` or `stable`, or
their old names until the same later release, only to name the settings it
prints.

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
- **Single sign-on** has two sources (README, Single sign-on). The
  operator's provider is `OIDC_ISSUER`, `OIDC_CLIENT_ID` and
  `OIDC_CLIENT_SECRET` in the env file; administrators see it read-only.
  Root and the platform's administrators add others from the front end,
  which needs `SECRETS_KEY` in the env file: 32 random bytes in base64,
  which seal each provider's client secret. `setup-server.sh` writes one; by
  hand:

  ```
  printf 'SECRETS_KEY=%s\n' "$(openssl rand -base64 32)" >> /etc/aishie/aishie.env
  ```

  It needs `SIGNING_KEY`, is the same on every server, and is never lost:
  what it sealed opens with nothing else. Without it, the front end is told
  no provider can be added (`secrets_key_missing`), and the operator's
  provider works as before. To rotate it, put a new key in `SECRETS_KEY` and
  the old one in `SECRETS_KEY_PREVIOUS` (comma separated, for more than one),
  deploy, run `aishie-core secrets rewrap`, which seals every client secret
  again under the new key and says how many, and then remove the old key.
  Register `https://lms-test.example.edu/v1/auth/sso/callback` with every
  provider: it is the same for all of them.
  `OIDC_DISPLAY_NAME` is what the front end's sign-in button calls the
  provider; without it, the front end uses words of its own. Like every value
  in the file it takes no quotes, even with a space in it:

  ```
  OIDC_DISPLAY_NAME=PolyU NetID
  ```

  It is at most 64 characters, all of them printable (a tab is not), or the
  server will not start. The front end asks the server at
  `GET /v1/auth/methods` which buttons to show, and what each says, so the
  same front end serves a server with single sign-on and one without; a
  change reaches the sign-in page within a minute of the deploy, or of an
  administrator's change to a provider.
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
  RUNTIME_AUDIENCES=https://lms-test.example.edu/runtime
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
  runtime. A runtime checks them against `https://lms-test.example.edu/v1/auth/keys`;
  both routes are under `/v1`, which the proxy already sends to Core. Set
  none of these, and no assertion is made.

  The runtime also hosts the runtime agents, by their ids, as the site
  service `agent_runtime` (README, Connecting an agent): give it its
  credential once, on the server, and keep it where the runtime reads its
  secrets. It is printed once, on standard output, and nowhere else; the
  service is made the first time. Run it again with `--replace`, which
  revokes the others, when the copy kept is lost:

  ```
  aishie-core service issue agent_runtime --label runtime > /root/agent-runtime.credential
  ```

  With the same credential the runtime converts every Office and
  OpenDocument file to PDF, for the front end to preview it (migration
  0026, below); nothing more is issued for that.

  `aishie-core service issue document_text --label transcriber` does the
  same for its transcriber. Root and administrators list and revoke them from
  the front end (`service.list_credentials`, `service.revoke_credential`).
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
- **Exporting conversations for audit:** root and the administrators of
  the site, and of a department for its courses, export conversations
  (`conversation.export`, README, Exporting conversations for audit). The
  files are written to the file store under `exports/` (on the server's
  disk, `/srv/aishie/data/blobs/exports/`), kept `EXPORT_TTL` (default `24h`, from
  `15m` to `168h`) and then removed by the sweep; they hold personal data,
  so keep the time short, and do not copy `exports/` into backups that
  outlive it. `EXPORT_MAX_MESSAGES` (100000) and `EXPORT_MAX_BYTES`
  (268435456, 256 MiB) bound one export; past either it is refused, saying
  how much it would hold, and the administrator narrows it. A large export
  takes as long as writing it takes, and the server keeps its request open up
  to five minutes for it; a proxy that cuts a request sooner loses the answer
  and not the export: the same call made again with the same idempotency key
  answers with it, and `conversation.export_file` gives its files.
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
    curl -s -o /dev/null -D - -H 'Content-Type: application/json' --data @- https://lms-test.example.edu/v1/auth/login |
    sed -n 's/^[Ss]et-[Cc]ookie: ais_session=\([^;]*\).*/\1/p' | tr -d '\r'); unset PW
  curl -X POST https://lms-test.example.edu/v1/actors/<actor_id>/invite \
    -H "Authorization: Bearer $SESSION" -H "Idempotency-Key: invite-<actor_id>-$(date +%s)" \
    -H 'Content-Type: application/json' -d '{}'
  ```

  `GET /v1/actors?search=<a piece of the name, email or login ID>` finds someone's
  `actor_id`, and says whether they have a password yet.
- **An agent's token:** register the agent, signed in as above, then issue
  its token by the id that comes back. Only an mcp agent is issued one: one
  its own tools reach over MCP (`"hosting": "mcp"`, which never changes).
  `--actor` with a person's id or email is refused (`api_tokens_are_for_agents`),
  as `credential.issue_token` and `actor.issue_token` refuse one for a person,
  and with a runtime agent's (`hosted_by_runtime`), whose one token the
  site's agent runtime is issued. A person who wants a script uses one of
  their agents instead: `agent.create` with `"hosting": "mcp"`,
  `member.add_delegate` into the course, `agent.issue_token`.

  ```
  curl -X POST https://lms-test.example.edu/v1/actors \
    -H "Authorization: Bearer $SESSION" -H "Idempotency-Key: register-grader-bot" \
    -H 'Content-Type: application/json' -d '{"kind":"agent","display_name":"grader-bot","hosting":"mcp"}'
  aishie-core token issue --actor <result.actor_id> --label grader-bot --days 90
  ```

  Then an instructor of the course seats it: `member.add`, that is
  `POST /v1/courses/{course_id}/members`, with preset `grader` or `tutor`.
  The administrator is no member of the course and cannot. The administrator
  seats the instructor first, with `course.seat_instructor`. Point the agent's
  MCP client at `https://lms-test.example.edu/mcp`.
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
- **Migration 0023, several files to a version:** a version of a document
  holds files in order, each with a name (`document_version_file`), and each
  file of material, instructions or a rubric has a text version of its own.
  The migration records every version's file as its one file, named after its
  document's title with its type's extension ("Week 1.pdf"), and each text
  version as that file's; it needs nothing of the operator. A version holds
  at most `DOCUMENT_MAX_FILES_PER_VERSION` files (20, at most 100) and
  `DOCUMENT_MAX_VERSION_BYTES` in all (200 MiB), each file at most
  `MAX_UPLOAD_BYTES` as before. New uploads for documents are kept under
  `documents/` beside `courses/`, and the sweep looks under both; a move of
  the files to another store copies both. The
  previous release, while the migration goes in and after a rollback, works
  for a version of one file as before: it reads a version's first file, whose
  columns this release still fills, writes one file, which the database
  records as the version's one file, and never sweeps `documents/`, so a
  version's other files are kept. Of a version of several files it shows the
  first alone, and a write of its text that would write every file's (a
  staff edit, a transcription of it) is refused; its transcriber may claim
  such a file and be refused when it completes it, until the file is failed
  after five claims. Deploy this release again and send those back with
  `document.text_retranscribe`.
- **Migration 0024, exporting conversations for audit:** an index of the
  messages of conversations by when they were written, so that an export of
  a span of time does not read every message; it needs nothing of the
  operator, and holds off messages being written while it is built, a
  moment on a school's site. The previous release, while the migration goes
  in and after a rollback, neither exports nor removes exports: files under
  `exports/` that this release wrote stay until it is deployed again and its
  sweep removes them.
- **Migration 0025, one hosting for each agent:** every agent is hosted one
  way, for good: `runtime`, run by the site's agent runtime, which alone is
  issued its token, by the agent's id, through the site service
  `agent_runtime`, and asked by people in the site while that token lives;
  or `mcp`, reached by its owner's own tools with tokens they issue, and
  asked nothing in the site. Nothing else makes an agent answer in the site,
  and nothing is declared any more (`me.site_chat` changes nothing, for one
  release, and migration 0027 removes it). The migration makes a runtime agent of
  every agent whose runtime had declared site chat with a token still live,
  takes that token as the runtime's, so that it is asked as before, and
  revokes the agent's other tokens: an owner's own tool connected to such an
  agent stops working, and needs an mcp agent of its own. Every other agent
  is an mcp agent, its tokens as they were. Before deploying it, give the
  runtime its credential (`aishie-core service issue agent_runtime`, above),
  and deploy a runtime that hosts by agent id: it issues each agent it hosts
  a token of its own on its first start, which revokes the one it was handed.
  `actor.site_chat_credential_id` is kept, pointing at each runtime agent's
  token, for the release before, which reads it, and dropped by migration
  0027. The previous release, while the migration goes in and after a
  rollback, registers mcp agents, and fails having changed nothing when it
  would issue a runtime agent a token of its owner's. Going down drops hosting
  and keeps every token as it is, the runtime's included, and the agent
  runtime service becomes a suspended agent nobody owns, as 0020's down
  leaves the transcriber.
- **Migration 0026, PDF renditions of Office files:** every Word, Excel,
  PowerPoint or OpenDocument file Core keeps, of a document of any kind or
  carried by a message, is converted to PDF once, by the site's agent
  runtime, with the credential it already holds (`agent_runtime`, above),
  for the front end to show in its PDF viewer; whoever may read the file
  reads its PDF (docs/schema.md §2.4, Renditions). The migration queues
  every such file there is, behind every file uploaded after it, the newest
  first: on a site with years of files, the runtime works through them for a
  while, one at a time by default, and nothing waits on it meanwhile. It
  needs nothing of the operator. `RENDITION_MAX_BYTES` (default
  `104857600`, 100 MiB) is the largest PDF taken; a larger one is skipped,
  `too_large`, and the file stays a download. Nothing else is configured,
  and there is no switch: no runtime claiming leaves every rendition
  `queued`, and the files are downloads, as before. The PDFs are kept in the
  file store under `renditions/` (on the server's disk,
  `/srv/aishie/data/blobs/renditions/`; with S3, `attached/renditions/`),
  beside the uploads, so a move of the files to another store copies it too,
  and the orphan sweep removes a PDF nothing names. The previous release,
  while the migration goes in and after a rollback, records files, which are
  queued as any are, and purges versions, whose renditions go with their
  files, their PDFs left to this release's sweep; it reads no rendition and
  never sweeps `renditions/`. Going down drops the renditions and leaves
  their PDFs in the store: migrated up again, every file is queued and
  converted again, and the sweep removes the PDFs made before.
- **Migration 0027, what 0023 and 0025 kept for one release goes:** a
  version's own file columns (`document_version.storage_key`,
  `content_type`, `byte_size`, `checksum`) and
  `actor.site_chat_credential_id` are dropped, with what kept them in step
  for the release before 0023 and 0025; `me.site_chat` is gone;
  `document.create` and `document.add_version` take a version's files in
  `files` alone, never `upload_token`; `document.get` and
  `document.versions` say nothing of a version's first file but in `files`;
  every call about a text names its file (`file_id`); `agent.update` takes
  no `site_chat`; and an agent registered naming no hosting is refused. It
  changes no row but by dropping those columns: a purged version no longer
  says what type and size its file was. **Deploy it only once every server
  runs a release with migrations 0023 and 0025, and an agent runtime that
  names a file (`file_id`) when it tries a transcription credential**
  (AIShie-Agent-Runtime's change for AIShie-Core #54). The runtime tries
  one, when an administrator sets or replaces it, by a renewal that this
  release refuses if it names no file, so an older runtime refuses every
  good credential (`credential_rejected`) until it is updated. The front end
  sends none of the above since AIShie-Core #49 and #52. A proposal waiting
  at the upgrade that gives a version its file by `upload_token` alone no
  longer reads as a call of this release: when someone approves it, it is
  cancelled (`tool_removed`), and its agent proposes it again with `files`;
  rejected, it is rejected as before.
  Unlike every migration before it, it does not leave the release before
  working: that release writes and reads the columns it drops. While it goes
  in, until this release has started, the release before fails what reads an
  actor or a version, a moment on a school's site; and if this release does
  not come up, starting the release before again does not help, whether
  `aishie-deploy` does it here or AIShie-Deploy's `aishie-update` does it on
  its stack, after a health check that failed: that release reports itself
  healthy, and fails every call that reads an actor or a version. To roll
  back, migrate down once with this release's image before running the
  release before (Rolling back, below). The down puts back each version's
  first file in its own columns and each runtime agent's runtime token as its
  site chat credential; what it cannot put back is the type and size of a
  purged version's file, which the release before shows and nothing keeps
  once 0027 has dropped them.
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
  fix the env file and deploy again. If the new version brought migration
  0027, though, the version before, started again, does not work on its
  schema: stop it, migrate down with the new image, and deploy the version
  before, as under Rolling back, below.
- **Rolling back** to the release before is a deploy of its image. The new
  schema is left as it is, and the release before works with it. Never run
  `migrate down`: it deletes data. Going back further than one release means
  restoring a backup.

  ```
  aishie-deploy ghcr.io/aishie-education/aishie-core:1.2.2
  ```

  Rolling back from the release with migration 0027 is the exception: the
  release before does not work on its schema, and this release does not
  work on the schema the down puts back (it fails to record a version with
  files, or to purge one). Stop this release, take the schema down one
  migration with its image, then deploy the release before. The site is
  down from the stop until the release before has started, which includes
  the backup `aishie-deploy` takes first:

  ```
  docker stop aishie
  docker run --rm --network host --env-file /etc/aishie/aishie.env \
    ghcr.io/aishie-education/aishie-core:<this release> migrate down --yes
  aishie-deploy ghcr.io/aishie-education/aishie-core:<the release before>
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
- `/srv/aishie/data/`, the uploaded files, but for `blobs/exports/`, the exports
  of conversations for audit, which are removed a day after they are made
  and are not to be kept longer elsewhere;
- `/etc/aishie/aishie.env`, which holds `SIGNING_KEY`, `SECRETS_KEY` (and
  `ASSERTION_KEY`, if it is set) and the database password. The database's
  backups hold identity providers' client secrets sealed with `SECRETS_KEY`:
  keep the two apart, or both in one place as well kept as the key.

## More than one server per environment

Two servers behind a load balancer need more than this set-up gives:

- files in S3 (`BLOB_STORE=s3` and the `S3_*` settings);
- the same `SIGNING_KEY` and `SECRETS_KEY` on every server, and the same
  `ASSERTION_KEY` and `SECRETS_KEY_PREVIOUS` if they are set;
- `HTTP_ADDR` that the load balancer can reach;
- `TRUSTED_PROXIES` set to the load balancer's addresses;
- a load balancer that lets a request run 40 seconds or more, for long
  polls, which need nothing else: each server hears what every other
  commits, answers' drafts included; like the rate limit, the bound on a
  conversation's drafts (ten writes a second) is each server's;
- the database on a server of its own.

`aishie-deploy` and the Deploy workflow handle one server per
environment.

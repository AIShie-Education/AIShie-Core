#!/bin/sh
# Sets up a fresh Ubuntu server (24.04 or later) to run AIShiteru Core, once.
# Run as root with this directory copied to the server (docs/deploying.md):
#
#   sh deploy/setup-server.sh lms-staging.example.edu staging
#
# It installs Docker, PostgreSQL and Caddy; creates the database, the env file
# with a generated database password and SIGNING_KEY, the data and backup
# directories and a daily backup; installs aishiteru-deploy and aishiterud;
# points Caddy at the server for HTTPS; and makes an SSH user, deploy, that
# can do one thing: run aishiteru-deploy, for the Deploy workflow. Run again,
# it leaves what is already there alone.
set -eu

HOST=${1:-}
ENVIRONMENT=${2:-staging}
case $HOST in '' | *[!A-Za-z0-9.-]* | .* | -*)
  echo "usage: setup-server.sh HOSTNAME [staging|production], e.g. lms-staging.example.edu" >&2; exit 2 ;;
esac
case $ENVIRONMENT in staging | production) ;; *) echo "the environment is staging or production" >&2; exit 2 ;; esac
[ "$(id -u)" = 0 ] || { echo "run this as root (sudo -i)" >&2; exit 1; }
here=$(cd "$(dirname "$0")" && pwd)
ENV_FILE=/etc/aishiteru/aishiteru.env
KEY=/root/aishiteru-deploy-key
say() { printf '\n== %s\n' "$*"; }

say "Packages"
pkgs="postgresql caddy openssh-server cron"
command -v docker >/dev/null 2>&1 || pkgs="docker.io $pkgs"
apt-get update -q
# shellcheck disable=SC2086 # a list of package names
DEBIAN_FRONTEND=noninteractive apt-get install -y -q $pkgs
if [ -d /run/systemd/system ]; then systemctl enable --now docker postgresql caddy ssh cron; fi

say "Database and $ENV_FILE"
role=$(runuser -u postgres -- psql -tAc "SELECT 1 FROM pg_roles WHERE rolname = 'aishiteru'")
if [ -e "$ENV_FILE" ]; then
  echo "$ENV_FILE is there already: left as it is"
  [ "$role" = 1 ] || echo "warning: there is no database role aishiteru; $ENV_FILE may be for another database" >&2
elif [ "$role" = 1 ]; then
  echo "the database role aishiteru exists but $ENV_FILE does not; write it by hand (docs/deploying.md)" >&2; exit 1
else
  # Hex only, so that neither the SQL nor the URL needs anything escaped.
  pw=$(openssl rand -hex 24)
  runuser -u postgres -- psql -q -c "CREATE ROLE aishiteru LOGIN PASSWORD '$pw'"
  runuser -u postgres -- createdb -O aishiteru aishiteru
  install -d -m 700 /etc/aishiteru
  (umask 077 && cat > "$ENV_FILE" <<ENVEOF)
DATABASE_URL=postgres://aishiteru:$pw@127.0.0.1:5432/aishiteru
HTTP_ADDR=127.0.0.1:8080
PUBLIC_URL=https://$HOST
TRUSTED_PROXIES=127.0.0.1/32
SIGNING_KEY=$(openssl rand -hex 32)
BLOB_STORE=fs
BLOB_FS_ROOT=/data/blobs
ENVEOF
  echo "wrote $ENV_FILE (keep a copy somewhere safe: SIGNING_KEY must not change)"
fi

say "Directories, scripts and the daily backup"
# The image runs as user 65532 (distroless nonroot).
install -d -o 65532 -g 65532 -m 750 /srv/aishiteru/data
install -d -o postgres -g postgres -m 700 /var/backups/aishiteru
install -m 755 "$here/aishiteru-deploy" "$here/aishiterud" /usr/local/bin/
cat > /etc/cron.d/aishiteru-backup <<'CRONEOF'
# AIShiteru Core: a database backup every night, one per weekday (seven kept).
0 3 * * * root runuser -u postgres -- pg_dump -Fc -f /var/backups/aishiteru/daily-$(date +\%u).dump aishiteru
CRONEOF

say "Caddy (HTTPS for $HOST)"
caddyfile=/etc/caddy/Caddyfile
if grep -q "^$HOST {" "$caddyfile" 2>/dev/null; then
  echo "$caddyfile already serves $HOST"
elif [ ! -e "$caddyfile" ] || grep -q 'root \* /usr/share/caddy' "$caddyfile"; then
  # The file the package ships is a demo page on :80.
  [ ! -e "$caddyfile" ] || cp "$caddyfile" "$caddyfile.orig"
  printf '%s {\n\treverse_proxy 127.0.0.1:8080\n}\n' "$HOST" > "$caddyfile"
  if [ -d /run/systemd/system ]; then systemctl reload caddy; fi
  echo "wrote $caddyfile; Caddy gets the certificate once $HOST points at this server"
else
  echo "$caddyfile has been changed by hand: add this yourself, then systemctl reload caddy" >&2
  printf '%s {\n\treverse_proxy 127.0.0.1:8080\n}\n' "$HOST" >&2
fi

say "SSH user deploy, for the Deploy workflow"
id deploy >/dev/null 2>&1 || useradd --create-home --shell /bin/sh deploy
# No password to log in with, and not locked either: a locked account is
# refused even with a key.
usermod -p '*' deploy
sudoers=/etc/sudoers.d/aishiteru-deploy
printf 'deploy ALL=(root) NOPASSWD: /usr/local/bin/aishiteru-deploy\n' > "$sudoers.new"
chmod 440 "$sudoers.new"
visudo -cqf "$sudoers.new" && mv "$sudoers.new" "$sudoers"
[ -e "$KEY" ] || ssh-keygen -q -t ed25519 -N '' -C "aishiteru-deploy@$HOST" -f "$KEY"
home=$(getent passwd deploy | cut -d: -f6)
install -d -o deploy -g deploy -m 700 "$home/.ssh"
# The key can do nothing but this: no shell, no forwarding, and the command
# it asks for is only ever the image aishiteru-deploy is given.
# shellcheck disable=SC2016 # $SSH_ORIGINAL_COMMAND is for sshd to expand
printf 'restrict,command="sudo -n /usr/local/bin/aishiteru-deploy \\"$SSH_ORIGINAL_COMMAND\\"" %s\n' "$(cat "$KEY.pub")" > "$home/.ssh/authorized_keys"
chown deploy:deploy "$home/.ssh/authorized_keys"
chmod 600 "$home/.ssh/authorized_keys"

upper=$(echo "$ENVIRONMENT" | tr '[:lower:]' '[:upper:]')
say "Done. What is left"
cat <<DONE
1. Let this server pull the image (a GitHub token, classic, with read:packages):
     docker login ghcr.io -u <your GitHub user name>
2. Start it (the image from the latest Publish run, or a release):
     aishiteru-deploy ghcr.io/aishiteru-lms/aishiteru-core:<tag>
3. Create the first administrator, then restart so the background jobs start:
     read -rsp 'Password (10 characters or more): ' PW; echo
     printf '%s\n' "\$PW" | aishiterud bootstrap --name "Your Name" --email you@example.edu --password-stdin; unset PW
     docker restart aishiteru
4. For the Deploy workflow, in the repository's Settings → Secrets and variables → Actions:
     variable DEPLOY_TARGET_$upper       deploy@$HOST
     variable DEPLOY_KNOWN_HOSTS_$upper  $HOST $(cut -d' ' -f1,2 /etc/ssh/ssh_host_ed25519_key.pub)
     secret   DEPLOY_SSH_KEY_$upper      the whole of $KEY (cat $KEY)
   then delete $KEY: the server keeps only its public half.
DONE

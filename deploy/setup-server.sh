#!/bin/sh
# Sets up a fresh Ubuntu server (24.04 or later) to run AIShiteru Core.
# Run as root with this directory copied to the server (docs/deploying.md):
#
#   sh deploy/setup-server.sh lms-staging.example.edu staging
#
# The environment, staging or production, names the GitHub settings it prints.
#
# It installs Docker, PostgreSQL and Caddy; creates the database, the env file
# with a generated database password and SIGNING_KEY, the data and backup
# directories and a nightly backup; installs aishiteru-deploy and aishiterud;
# points Caddy at the server for HTTPS; opens ports 80 and 443 in ufw when ufw
# is on; and makes an SSH user, deploy, that can do one thing: run
# aishiteru-deploy, for the Deploy workflow.
#
# Run again, it installs the scripts in this directory over the old ones and
# leaves everything else as it is: the env file, the database, Caddy's
# configuration and deploy's key. That is how a newer aishiteru-deploy reaches
# the server.
set -eu

HOST=${1:-}
ENVIRONMENT=${2:-}
usage() { echo "usage: setup-server.sh HOSTNAME staging|production, e.g. lms-staging.example.edu staging" >&2; exit 2; }
case $HOST in '' | *[!A-Za-z0-9.-]* | .* | -*) usage ;; esac
# It names the GitHub settings this server needs; guessing would name the
# other environment's.
case $ENVIRONMENT in staging | production) ;; *) usage ;; esac
[ "$(id -u)" = 0 ] || { echo "run this as root (sudo -i)" >&2; exit 1; }
here=$(cd "$(dirname "$0")" && pwd)
ENV_FILE=/etc/aishiteru/aishiteru.env
KEY=/root/aishiteru-deploy-key
say() { printf '\n== %s\n' "$*"; }
systemd() { [ -d /run/systemd/system ]; }

say "Packages"
# A server that has just booted is often still updating itself, and apt-get,
# unlike apt, does not wait for the lock.
if command -v cloud-init >/dev/null 2>&1; then cloud-init status --wait >/dev/null 2>&1 || true; fi
pkgs="postgresql caddy openssh-server cron curl"
command -v docker >/dev/null 2>&1 || pkgs="docker.io $pkgs"
i=0
until apt-get update -q; do
  i=$((i + 1))
  [ "$i" -lt 20 ] || { echo "apt-get update kept failing: run this again in a while" >&2; exit 1; }
  echo "apt is busy, most likely the server updating itself: trying again in 30 seconds"
  sleep 30
done
# shellcheck disable=SC2086 # a list of package names
DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=600 install -y -q $pkgs
if systemd; then systemctl enable --now docker postgresql caddy ssh cron; fi

say "Database and $ENV_FILE"
role=$(runuser -u postgres -- psql -tAc "SELECT 1 FROM pg_roles WHERE rolname = 'aishiteru'")
if [ -e "$ENV_FILE" ]; then
  echo "$ENV_FILE is there already: left as it is"
  [ "$role" = 1 ] || echo "warning: there is no database role aishiteru; $ENV_FILE may be for another database" >&2
elif [ "$role" = 1 ]; then
  cat >&2 <<MSG
The database role aishiteru exists, but $ENV_FILE does not: an earlier run
stopped half way. If this server holds no data yet, remove them and run this
again:

  runuser -u postgres -- dropdb --if-exists aishiteru
  runuser -u postgres -- dropuser aishiteru

Otherwise give the role a new password (openssl rand -hex 24), with psql:
ALTER ROLE aishiteru PASSWORD '...'; and write $ENV_FILE, mode 600, with the
lines this script writes: DATABASE_URL, HTTP_ADDR, PUBLIC_URL, TRUSTED_PROXIES,
SIGNING_KEY, BLOB_STORE and BLOB_FS_ROOT (see the script).
MSG
  exit 1
else
  # Hex only, so that neither the SQL nor the URL needs anything escaped.
  pw=$(openssl rand -hex 24)
  install -d -m 700 /etc/aishiteru
  (umask 077 && cat > "$ENV_FILE.new" <<ENVEOF)
DATABASE_URL=postgres://aishiteru:$pw@127.0.0.1:5432/aishiteru
HTTP_ADDR=127.0.0.1:8080
PUBLIC_URL=https://$HOST
TRUSTED_PROXIES=127.0.0.1/32
SIGNING_KEY=$(openssl rand -hex 32)
BLOB_STORE=fs
BLOB_FS_ROOT=/data/blobs
ENVEOF
  # On psql's input, not its command line, where anyone could read the
  # password; and kept out of the server's log should the statement fail.
  printf "SET log_min_error_statement = panic;\nCREATE ROLE aishiteru LOGIN PASSWORD '%s';\nCREATE DATABASE aishiteru OWNER aishiteru;\n" "$pw" |
    runuser -u postgres -- psql -q -v ON_ERROR_STOP=1
  mv "$ENV_FILE.new" "$ENV_FILE"
  echo "wrote $ENV_FILE (keep a copy somewhere safe: SIGNING_KEY must not change)"
fi

say "Directories, scripts and the nightly backup"
# The image runs as user 65532 (distroless nonroot).
install -d -o 65532 -g 65532 -m 750 /srv/aishiteru/data
install -d -o postgres -g postgres -m 700 /var/backups/aishiteru
install -m 755 "$here/aishiteru-deploy" "$here/aishiterud" /usr/local/bin/
echo "installed aishiteru-deploy and aishiterud in /usr/local/bin"
cat > /etc/cron.d/aishiteru-backup <<'CRONEOF'
# AIShiteru Core: a database backup every night, one per weekday (seven kept).
# A job in /etc/cron.d gets no /usr/sbin, where runuser is, unless it says so.
PATH=/usr/sbin:/usr/bin:/sbin:/bin
0 3 * * * root f=/var/backups/aishiteru/daily-$(date +\%u).dump; runuser -u postgres -- pg_dump -Fc -f "$f.part" aishiteru && mv "$f.part" "$f"
CRONEOF

say "Caddy (HTTPS for $HOST)"
# Caddy's admin API listens on localhost:2019 unless told otherwise, and the
# app shares the host's network: it would be able to rewrite Caddy's
# configuration. On a socket in Caddy's own directory, only Caddy can.
site=$(printf '{\n\tadmin unix//var/lib/caddy/admin.sock\n}\n\n%s {\n\treverse_proxy 127.0.0.1:8080\n}\n' "$HOST")
caddyfile=/etc/caddy/Caddyfile
if grep -q "^$HOST {" "$caddyfile" 2>/dev/null; then
  echo "$caddyfile already serves $HOST"
elif [ ! -e "$caddyfile" ] || grep -q 'root \* /usr/share/caddy' "$caddyfile"; then
  # The file the package ships is a demo page on :80.
  [ ! -e "$caddyfile" ] || cp "$caddyfile" "$caddyfile.orig"
  printf '%s\n' "$site" > "$caddyfile"
  # A restart, not a reload: the running Caddy still has its admin API on TCP.
  if systemd; then systemctl restart caddy; fi
  echo "wrote $caddyfile; Caddy gets the certificate once $HOST points at this server"
else
  echo "$caddyfile has been changed by hand: make it this yourself, then systemctl restart caddy" >&2
  printf '%s\n' "$site" >&2
fi

say "Firewall"
if command -v ufw >/dev/null 2>&1 && ufw status | grep -q '^Status: active'; then
  # One by one, so that set -e stops at one that fails.
  ufw allow 22/tcp >/dev/null
  ufw allow 80/tcp >/dev/null
  ufw allow 443/tcp >/dev/null
  echo "ufw: 22, 80 and 443 allowed"
else
  echo "ufw is off; a firewall of your provider's must allow 22, 80 and 443"
fi
sshd_cfg=$(/usr/sbin/sshd -T 2>/dev/null || true)
if printf '%s\n' "$sshd_cfg" | grep -qx 'passwordauthentication yes'; then
  echo "warning: SSH takes passwords, and port 22 is open to the internet for the Deploy workflow." >&2
  echo "  Once you log in with a key: echo 'PasswordAuthentication no' > /etc/ssh/sshd_config.d/00-no-passwords.conf" >&2
  echo "  (sshd keeps the first value it reads, and cloud-init's 50-cloud-init.conf may say yes)," >&2
  echo "  systemctl restart ssh, and check that sshd -T | grep -i passwordauthentication says no" >&2
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
home=$(getent passwd deploy | cut -d: -f6)
install -d -o deploy -g deploy -m 700 "$home/.ssh"
if grep -q 'aishiteru-deploy@' "$home/.ssh/authorized_keys" 2>/dev/null; then
  # The key in GitHub keeps working. To replace it: docs/deploying.md.
  echo "deploy's SSH key is set up already: left as it is"
else
  [ -e "$KEY" ] || ssh-keygen -q -t ed25519 -N '' -C "aishiteru-deploy@$HOST" -f "$KEY"
  # The key can do nothing but this: no shell, no forwarding, and the command
  # it asks for is only ever the image aishiteru-deploy is given.
  # shellcheck disable=SC2016 # $SSH_ORIGINAL_COMMAND is for sshd to expand
  printf 'restrict,command="sudo -n /usr/local/bin/aishiteru-deploy \\"$SSH_ORIGINAL_COMMAND\\"" %s\n' "$(cat "$KEY.pub")" > "$home/.ssh/authorized_keys"
  chown deploy:deploy "$home/.ssh/authorized_keys"
  chmod 600 "$home/.ssh/authorized_keys"
  echo "made deploy's SSH key"
fi

upper=$(echo "$ENVIRONMENT" | tr '[:lower:]' '[:upper:]')
say "Done. What is left"
cat <<DONE
1. Let this server pull the image (a GitHub token, classic, with read:packages):
     docker login ghcr.io -u <GitHub user name>
2. Start it, with the image of the latest green push to main (the CI run's
   publish / image job, or the package's page, names it), or of a release:
     aishiteru-deploy ghcr.io/aishie-education/aishie-core:sha-<commit>
3. Create the first administrator, then restart so the background jobs start:
     read -rsp 'Password (10 characters or more): ' PW; echo
     printf '%s\n' "\$PW" | aishiterud bootstrap --name "Your Name" --email you@example.edu --password-stdin; unset PW
     docker restart aishiteru
4. For the Deploy workflow, in the repository's Settings → Secrets and variables → Actions:
     variable DEPLOY_TARGET_$upper       deploy@$HOST
     variable DEPLOY_KNOWN_HOSTS_$upper  $HOST $(cut -d' ' -f1,2 /etc/ssh/ssh_host_ed25519_key.pub)
DONE
if [ -e "$KEY" ]; then
  cat <<DONE
     secret   DEPLOY_SSH_KEY_$upper      the whole of $KEY (cat $KEY)
   then delete $KEY: the server keeps only its public half.
DONE
fi

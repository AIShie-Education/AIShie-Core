#!/usr/bin/env bash
# aishiteru-deploy against stand-ins for docker, pg_dump (by way of runuser),
# curl, flock and sleep, which record what they are asked to do:
#
#   make script-test
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

IMG=ghcr.io/aishie-education/aishie-core:0.2.0
OLD=ghcr.io/aishie-education/aishie-core:0.1.0

# The stand-ins. Each appends its command line to $CALLS; docker keeps the
# image of the container named aishiteru in $STATE/container. An image's id
# is its name, unless $STATE/ids says otherwise ("name id" lines: one image
# under two names), and its version is $VERSION_OUT, unless $STATE/versions
# says otherwise ("name version line").
mkdir -p "$work/bin"
cat > "$work/bin/docker" <<'EOF'
#!/usr/bin/env bash
echo "docker $*" >> "$CALLS"
id_of() { awk -v n="$1" '$1 == n { print $2; f = 1 } END { if (!f) print "id:" n }' "$STATE/ids" 2>/dev/null || echo "id:$1"; }
version_of() {
  v=$(awk -v n="$1" '$1 == n { $1 = ""; sub(/^ /, ""); print }' "$STATE/versions" 2>/dev/null)
  echo "${v:-$VERSION_OUT}"
}
case $1 in
  pull) exit "${PULL_FAIL:-0}" ;;
  inspect)
    [ -e "$STATE/container" ] || exit 1
    case $3 in '{{.Image}}') id_of "$(cat "$STATE/container")" ;; *) cat "$STATE/container" ;; esac ;;
  image) [ "$2" = inspect ] && id_of "${*: -1}" ;;
  stop) : ;;
  rm) rm -f "$STATE/container" ;;
  run)
    last=${*: -1}
    if [ "$2" = -d ]; then
      [ ! -e "$STATE/container" ] || { echo "docker: the name aishiteru is taken" >&2; exit 125; }
      echo "${*: -1}" > "$STATE/container"
    elif [ "$last" = version ]; then
      version_of "${*: -2:1}"
    elif [ "$last" = up ]; then
      exit "${MIGRATE_FAIL:-0}"
    fi ;;
esac
EOF
cat > "$work/bin/runuser" <<'EOF'
#!/usr/bin/env bash
# Like pg_dump, it writes the file it is given, and a failing one leaves it.
echo "runuser $*" >> "$CALLS"
while [ $# -gt 0 ]; do [ "$1" = -f ] && { echo partial > "$2"; break; }; shift; done
exit "${BACKUP_FAIL:-0}"
EOF
cat > "$work/bin/curl" <<'EOF'
#!/usr/bin/env bash
echo "curl $*" >> "$CALLS"
printf '%s\n' "$HEALTH_JSON"
EOF
printf '#!/bin/sh\nexit 0\n' > "$work/bin/flock"
printf '#!/bin/sh\nexit 0\n' > "$work/bin/sleep"
chmod +x "$work/bin/"*

failed=0
fail() { echo "FAIL $case: $*" >&2; failed=1; }

# setup CASE: a fresh server, the env file, and a clean record.
setup() {
  case=$1
  export STATE=$work/$case CALLS=$work/$case/calls
  mkdir -p "$STATE/backups"
  : > "$CALLS"
  printf 'DATABASE_URL=postgres://x\nHTTP_ADDR=127.0.0.1:8080\n' > "$STATE/env"
  export AISHITERU_ENV_FILE=$STATE/env AISHITERU_DATA_DIR=$STATE/data \
    AISHITERU_BACKUP_DIR=$STATE/backups AISHITERU_LOG_FILE=$STATE/log \
    AISHITERU_LOCK_FILE=$STATE/lock AISHITERU_HEALTH_TRIES=3
  export VERSION_OUT="v0.2.0 (abc1234, 2026-09-25T04:10:07Z)"
  export HEALTH_JSON='{"status":"ok","version":"v0.2.0","commit":"abc1234","schema_version":4,"schema_latest":4}'
  unset PULL_FAIL MIGRATE_FAIL BACKUP_FAIL
}
deploy() { PATH="$work/bin:$PATH" "$here/aishiteru-deploy" "$@" > "$STATE/out" 2>&1; }
called() { grep -q -- "$1" "$CALLS"; }
# line PATTERN: the first line of the record that matches, 0 if none.
line() { grep -n -- "$1" "$CALLS" | head -n 1 | cut -d: -f1 || true; }

# Anything that is not this repository's image is refused before anything runs.
for bad in "" "nginx:latest" "ghcr.io/aishie-education/aishie-core" \
  "ghcr.io/aishie-education/aishie-core-evil:1" "ghcr.io/other/aishiteru-core:1" \
  "$IMG;id" "$IMG id" "$IMG\$(id)" "$IMG\`id\`" "$IMG|id" "$IMG&id" "$IMG'x"; do
  setup refused
  if deploy "$bad"; then fail "accepted «$bad»"; fi
  [ ! -s "$CALLS" ] || fail "ran something for «$bad»: $(paste -sd ';' "$CALLS")"
done

# By digest, as the Deploy workflow calls it.
setup by-digest
deploy "ghcr.io/aishie-education/aishie-core@sha256:$(printf 'a%.0s' $(seq 64))" || fail "refused a digest: $(cat "$STATE/out")"

# A first deploy: backup, migrate, seed, start, and wait for the new version.
setup first
deploy "$IMG" || fail "exit $?: $(cat "$STATE/out")"
for step in "docker pull $IMG" "runuser -u postgres -- pg_dump" "migrate up" "seed" "docker run -d --name aishiteru"; do
  called "$step" || fail "no «$step»"
done
[ "$(line 'pg_dump')" -lt "$(line 'migrate up')" ] || fail "migrated before the backup"
grep -q -- "--sig-proxy=false .* migrate up" "$CALLS" || fail "migrate up passes a Ctrl-C on: $(grep 'migrate up' "$CALLS")"
[ "$(line 'docker run -d')" -lt "$(line 'docker image prune')" ] || fail "images not pruned after the deploy"
ls "$STATE/backups"/deploy-*.dump > /dev/null 2>&1 || fail "no backup kept"
! ls "$STATE/backups"/*.part > /dev/null 2>&1 || fail "a .part file left behind"
[ "$(line 'migrate up')" -lt "$(line 'docker run -d')" ] || fail "started before migrating"
! called "docker stop" || fail "stopped a container that was not there"
[ "$(cat "$STATE/container")" = "$IMG" ] || fail "running $(cat "$STATE/container")"
called "curl -fsS http://127.0.0.1:8080/healthz" || fail "health checked elsewhere"
grep -q "none -> $IMG" "$STATE/log" || fail "log: $(cat "$STATE/log")"

# An upgrade: the old container goes only after the migration.
setup upgrade
echo "$OLD" > "$STATE/container"
deploy "$IMG" || fail "exit $?: $(cat "$STATE/out")"
[ "$(line 'migrate up')" -lt "$(line 'docker stop aishiteru')" ] || fail "stopped the old version before migrating"
[ "$(cat "$STATE/container")" = "$IMG" ] || fail "running $(cat "$STATE/container")"
grep -q "$OLD -> $IMG" "$STATE/log" || fail "log: $(cat "$STATE/log")"

# A migration that fails leaves the old version running and touched nothing.
setup migrate-fails
echo "$OLD" > "$STATE/container"
if MIGRATE_FAIL=1 deploy "$IMG"; then fail "went on after a failed migration"; fi
! called "docker stop" || fail "stopped the old version"
! called "docker run -d" || fail "started the new version"
! called " seed" || fail "seeded after a failed migration"
[ "$(cat "$STATE/container")" = "$OLD" ] || fail "running $(cat "$STATE/container")"
grep -q "migrate up failed" "$STATE/out" || fail "said: $(cat "$STATE/out")"
! called "image prune" || fail "pruned after a failed deploy"

# No backup, no migration, and no half a backup left among the good ones.
setup backup-fails
echo "$OLD" > "$STATE/container"
if BACKUP_FAIL=1 deploy "$IMG"; then fail "went on without a backup"; fi
! called "migrate up" || fail "migrated without a backup"
[ -z "$(find "$STATE/backups" -type f)" ] || fail "left $(find "$STATE/backups" -type f)"

# A new version that never reports healthy is a failure, named.
setup unhealthy
HEALTH_JSON='{"status":"ok","version":"v0.1.0","commit":"0ld0ld0"}'
if deploy "$IMG"; then fail "passed while the old version answered"; fi
grep -q "did not report healthy" "$STATE/out" || fail "said: $(cat "$STATE/out")"
[ "$(grep -c '^curl' "$CALLS")" = 3 ] || fail "asked $(grep -c '^curl' "$CALLS") times, not 3"
called "docker logs --tail" || fail "did not show the failed start's log"

# ...and the version before, if there was another, is started again, and
# said to be running only once it reports healthy.
setup unhealthy-rollback
echo "$OLD" > "$STATE/container"
echo "$OLD v0.1.0 (0ld0ld0, 2026-09-01T00:00:00Z)" > "$STATE/versions"
HEALTH_JSON='{"status":"ok","version":"v0.1.0","commit":"0ld0ld0"}'
if deploy "$IMG"; then fail "passed while the new version was not healthy"; fi
[ "$(cat "$STATE/container")" = "$OLD" ] || fail "left $(cat "$STATE/container") running, not $OLD"
grep -q "rolled back" "$STATE/log" || fail "log: $(cat "$STATE/log")"
grep -q "rolled back: $OLD is running again" "$STATE/out" || fail "said: $(tail -n 1 "$STATE/out")"
! called "image prune" || fail "pruned the image rolled back to"

# A version before that does not come up either (the env file, most likely)
# is not reported as running.
setup unhealthy-rollback-too
echo "$OLD" > "$STATE/container"
echo "$OLD v0.1.0 (0ld0ld0, 2026-09-01T00:00:00Z)" > "$STATE/versions"
HEALTH_JSON='{"status":"starting"}'
if deploy "$IMG"; then fail "passed while nothing was healthy"; fi
grep -q "did not report healthy either" "$STATE/out" || fail "said: $(tail -n 1 "$STATE/out")"

# The same image under another name (:sha- by hand, then by digest) is not a
# version to go back to.
setup unhealthy-alias
SHA_NAME=ghcr.io/aishie-education/aishie-core:sha-abc1234
DIGEST_NAME="ghcr.io/aishie-education/aishie-core@sha256:$(printf 'd%.0s' $(seq 64))"
echo "$SHA_NAME" > "$STATE/container"
printf '%s same\n%s same\n' "$SHA_NAME" "$DIGEST_NAME" > "$STATE/ids"
HEALTH_JSON='{"status":"starting"}'
if deploy "$DIGEST_NAME"; then fail "passed while not healthy"; fi
[ "$(grep -c 'docker run -d' "$CALLS")" = 1 ] || fail "started $(grep -c 'docker run -d' "$CALLS") containers for one image"

# The same image again (a change to the env file) has nothing to go back to.
setup unhealthy-same
echo "$IMG" > "$STATE/container"
HEALTH_JSON='{"status":"starting"}'
if deploy "$IMG"; then fail "passed while not healthy"; fi
[ "$(grep -c 'docker run -d' "$CALLS")" = 1 ] || fail "started $(grep -c 'docker run -d' "$CALLS") containers"

# HTTP_ADDR decides where /healthz is asked.
for pair in ":9090=127.0.0.1:9090" "0.0.0.0:9091=127.0.0.1:9091" "10.0.0.5:9092=10.0.0.5:9092"; do
  setup "addr-${pair%%=*}"
  printf 'HTTP_ADDR=%s\n' "${pair%%=*}" > "$STATE/env"
  deploy "$IMG" || fail "exit $?: $(cat "$STATE/out")"
  called "curl -fsS http://${pair#*=}/healthz" || fail "asked $(grep '^curl' "$CALLS" | head -n 1)"
done

# The newest ten deploy backups stay; the daily ones are not touched.
setup prune
for n in $(seq -w 1 12); do touch -t "202601${n}0000" "$STATE/backups/deploy-202601$n-000000.dump"; done
touch "$STATE/backups/daily-1.dump"
deploy "$IMG" || fail "exit $?: $(cat "$STATE/out")"
kept=$(find "$STATE/backups" -name 'deploy-*.dump' | wc -l)
[ "$kept" -eq 10 ] || fail "kept $kept deploy backups, not 10"
[ ! -e "$STATE/backups/deploy-20260101-000000.dump" ] || fail "kept the oldest"
[ -e "$STATE/backups/daily-1.dump" ] || fail "removed a daily backup"

# No env file: nothing runs.
setup no-env
rm "$STATE/env"
if deploy "$IMG"; then fail "ran without $AISHITERU_ENV_FILE"; fi
[ ! -s "$CALLS" ] || fail "ran something: $(paste -sd ';' "$CALLS")"

[ "$failed" = 0 ] && echo "aishiteru-deploy: ok"
exit "$failed"

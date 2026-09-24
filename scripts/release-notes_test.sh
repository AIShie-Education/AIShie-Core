#!/usr/bin/env bash
# release-notes.sh against a scratch repository of its own: tags, pre-release
# tags in between, and releases with no new migration.
#
#   make script-test
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(mktemp -d)
trap 'rm -rf "$repo"' EXIT

# Nobody's git configuration, signing or hooks may change what is tested.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
g() { git -C "$repo" -c user.name=test -c user.email=test@example.invalid \
  -c commit.gpgSign=false -c tag.gpgSign=false "$@"; }
commit() {
  for f in "$@"; do mkdir -p "$repo/$(dirname "$f")" && echo "$f" > "$repo/$f"; done
  g add -A && g commit -q -m "add $*"
}

failed=0
expect() {
  local tag=$1 prev=$2 migrations=$3 got
  got=$(cd "$repo" && "$here/release-notes.sh" "$tag")
  if [ "$got" != "$(printf 'prev=%s\nmigrations=%s' "$prev" "$migrations")" ]; then
    printf 'FAIL %s\n  got:  %s\n  want: prev=%s migrations=%s\n' "$tag" "$(echo "$got" | paste -sd ' ')" "$prev" "$migrations" >&2
    failed=1
  fi
}

g init -q -b main
commit src/migrations/0001_init.up.sql src/migrations/0001_init.down.sql
g tag v0.9.0
expect v0.9.0 "" "0001_init.up.sql"

commit src/migrations/0002_more.up.sql src/migrations/0002_more.down.sql
g tag v1.0.0-rc.1
expect v1.0.0-rc.1 v0.9.0 "0002_more.up.sql"

commit src/migrations/0003_again.up.sql src/migrations/0003_again.down.sql
g tag v1.0.0-rc.2
expect v1.0.0-rc.2 v1.0.0-rc.1 "0003_again.up.sql"

# The stable release after its pre-releases lists everything since the last
# stable one, not "no schema changes".
commit README.md
g tag v1.0.0
expect v1.0.0 v0.9.0 "0002_more.up.sql 0003_again.up.sql"

commit docs/notes.md
g tag v1.0.1
expect v1.0.1 v1.0.0 ""

# A commit that is no tag, as publish.yml asks about main.
commit src/migrations/0004_later.up.sql
expect "$(g rev-parse HEAD)" v1.0.1 "0004_later.up.sql"

[ "$failed" = 0 ] && echo "release-notes.sh: ok"
exit "$failed"

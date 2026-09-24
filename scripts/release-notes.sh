#!/usr/bin/env bash
# What the notes of a release need from git: the tag it is compared with, and
# the migrations added since. release.yml and publish.yml use it.
#
#   scripts/release-notes.sh TAG [COMMIT]     (COMMIT defaults to TAG)
#
# Prints, in the form $GITHUB_OUTPUT takes:
#
#   prev=<the tag compared with; empty if there is none>
#   migrations=<the *.up.sql files added since, space-separated; all of them if there is no prev>
#
# A stable tag (no hyphen) is compared with the stable tag before it, not with
# a pre-release in between: someone upgrading from v0.9.0 to v1.0.0 has to run
# what v1.0.0-rc.1 brought too. A pre-release, or a commit that is no tag, is
# compared with the tag just before it; a commit counts as stable.
set -euo pipefail

tag=${1:?usage: scripts/release-notes.sh TAG [COMMIT]}
commit=${2:-$tag}

case "$tag" in
  *-*) exclude=() ;;
  *) exclude=(--exclude '*-*') ;;
esac
prev=$(git describe --tags --abbrev=0 --match 'v[0-9]*' "${exclude[@]}" "$commit^" 2>/dev/null || true)

if [ -n "$prev" ]; then
  list=$(git diff --name-only --diff-filter=A "$prev" "$commit" -- 'src/migrations/*.up.sql')
else
  list=$(git ls-tree -r --name-only "$commit" -- src/migrations/ | grep '\.up\.sql$' || true)
fi
list=$(printf '%s\n' "$list" | sed '/^$/d' | xargs -r -n1 basename | sort | paste -sd ' ')

echo "prev=$prev"
echo "migrations=$list"

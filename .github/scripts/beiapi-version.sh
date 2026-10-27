#!/usr/bin/env bash
# Derive the fork revision from an explicitly reviewed upstream integration.
set -euo pipefail

fail() { printf 'beiapi-version: %s\n' "$*" >&2; exit 1; }
[[ $# -eq 0 ]] || fail 'Usage: bash .github/scripts/beiapi-version.sh'
cd "$(git rev-parse --show-toplevel)"
[[ $(git rev-parse --is-shallow-repository) == false ]] || fail 'Full Git history is required (fetch-depth: 0).'

upstream_version= upstream_commit= integration_commit=
while IFS='=' read -r key value; do
  case "$key" in
    upstream_version) upstream_version=$value ;;
    upstream_commit) upstream_commit=$value ;;
    integration_commit) integration_commit=$value ;;
    *) fail "Unknown version-base field: $key" ;;
  esac
done < .github/beiapi-version-base
[[ $upstream_version =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z]+(\.[0-9A-Za-z]+)*)?$ ]] || fail 'Invalid upstream release version.'
[[ $upstream_commit =~ ^[0-9a-f]{40}$ && $integration_commit =~ ^[0-9a-f]{40}$ ]] || fail 'Full upstream and integration commit IDs are required.'
tag_commit=$(git rev-parse --verify "refs/tags/${upstream_version}^{commit}") || fail 'Fetch the upstream release tags.'
[[ $tag_commit == "$upstream_commit" ]] || fail 'Upstream tag does not match the recorded source commit.'

# The upstream tag need not be an ancestor: selective integration preserves
# intentional fork exclusions. The local anchor must be on the first-parent line.
revision=$(git rev-list --first-parent HEAD | awk -v anchor="$integration_commit" '
  $0 == anchor { found = 1; print NR - 1 }
  END { if (!found) exit 1 }
') || fail 'Integration commit is not on the current first-parent history.'

suffix=
if [[ -n $(git status --porcelain --untracked-files=normal) ]]; then
  suffix=-dirty
fi
separator=-
[[ $upstream_version != *-* ]] || separator=.
printf '%s%sbeiapi.%s%s\n' "$upstream_version" "$separator" "$revision" "$suffix"

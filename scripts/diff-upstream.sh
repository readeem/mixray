#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
export GOWORK=off

# shellcheck source=scripts/lib.sh
source "$root/scripts/lib.sh"

target_module="github.com/readeem/mixray"
module=$(jq -er '.mihomo.module' upstream.json)
version=$(jq -er '.mihomo.version' upstream.json)
sum=$(jq -er '.mihomo.sum' upstream.json)
gomod_sum=$(jq -er '.mihomo.gomod_sum' upstream.json)

work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT

prepare_upstream_snapshot "$module" "$version" "$sum" "$gomod_sum" "$work/base" "$target_module" >/dev/null

# Show diffs for all upstream-tracked files (excluding go.mod/go.sum).
rc=0
while IFS= read -r rel; do
  case "$rel" in
    go.mod|go.sum) continue ;;
  esac
  if [[ ! -f $rel ]]; then
    printf 'deleted: %s\n' "$rel"
    rc=1
  elif ! cmp -s "$work/base/$rel" "$rel"; then
    git diff --no-index -- "$work/base/$rel" "$rel" || true
    rc=1
  fi
done < <(cd "$work/base" && find . -type f | sed 's|^\./||' | sort)

exit "$rc"

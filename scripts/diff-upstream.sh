#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
export GOWORK=off

# shellcheck source=scripts/lib.sh
source "$root/scripts/lib.sh"

root_module="github.com/readeem/mixray"
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT

rc=0

diff_tree() {
  local mode=$1 label=$2 module=$3 version=$4 sum=$5 gomod_sum=$6 dest_prefix=$7 target_module=$8
  local base="$work/$label"
  prepare_snapshot "$mode" "$module" "$version" "$sum" "$gomod_sum" "$base" "$target_module" >/dev/null

  while IFS= read -r rel; do
    case "$rel" in
      go.mod|go.sum) continue ;;
    esac
    local local_path="$rel"
    [[ -n $dest_prefix ]] && local_path="$dest_prefix/$rel"
    if [[ ! -f $local_path ]]; then
      printf 'deleted: %s\n' "$local_path"
      rc=1
    elif ! cmp -s "$base/$rel" "$local_path"; then
      git diff --no-index -- "$base/$rel" "$local_path" || true
      rc=1
    fi
  done < <(cd "$base" && find . -type f | sed 's|^\./||' | sort)
}

diff_tree root mihomo \
  "$(jq -er '.mihomo.module' upstream.json)" \
  "$(jq -er '.mihomo.version' upstream.json)" \
  "$(jq -er '.mihomo.sum' upstream.json)" \
  "$(jq -er '.mihomo.gomod_sum' upstream.json)" \
  "" "$root_module"

while IFS=$'\t' read -r name module version sum gomod_sum path; do
  [[ -n $name ]] || continue
  diff_tree vendored "vendored-$name" \
    "$module" "$version" "$sum" "$gomod_sum" \
    "$path" "$root_module/$path"
done < <(jq -r '.vendored // {} | to_entries[] | [.key, .value.module, .value.version, .value.sum, .value.gomod_sum, .value.path] | @tsv' upstream.json)

exit "$rc"

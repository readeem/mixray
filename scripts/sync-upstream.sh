#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
export GOWORK=off

# shellcheck source=scripts/lib.sh
source "$root/scripts/lib.sh"

root_module="github.com/readeem/mixray"

# Usage:
#   sync-upstream.sh [vX.Y.Z]              # sync mihomo (default)
#   sync-upstream.sh <vendored-name> <ver> # sync a vendored module in upstream.json
mode=root
name=mihomo
target_version=""

if (( $# == 0 )); then
  :
elif (( $# == 1 )); then
  if jq -e --arg n "$1" '.vendored[$n] != null' upstream.json >/dev/null; then
    printf 'usage: %s %s <version>\n' "$0" "$1" >&2
    exit 2
  fi
  target_version=$1
elif (( $# == 2 )); then
  if [[ $1 == mihomo ]]; then
    target_version=$2
  elif jq -e --arg n "$1" '.vendored[$n] != null' upstream.json >/dev/null; then
    mode=vendored
    name=$1
    target_version=$2
  else
    printf 'Unknown upstream target: %s\n' "$1" >&2
    exit 2
  fi
else
  printf 'usage: %s [target] [version]\n' "$0" >&2
  exit 2
fi

if [[ $mode == root ]]; then
  jq_path='.mihomo'
  dest_prefix=""
  target_module="$root_module"
else
  jq_path=".vendored[\"$name\"]"
  dest_prefix=$(jq -er "$jq_path.path" upstream.json)
  target_module="$root_module/$dest_prefix"
fi

module=$(jq -er "$jq_path.module" upstream.json)
repo=$(jq -er "$jq_path.repo" upstream.json)
current_version=$(jq -er "$jq_path.version" upstream.json)
current_sum=$(jq -er "$jq_path.sum" upstream.json)
current_gomod_sum=$(jq -er "$jq_path.gomod_sum" upstream.json)

if [[ -z $target_version ]]; then
  target_version=$(git ls-remote --tags --sort=-v:refname "https://github.com/$repo.git" 'v1.*' \
    | awk -F/ '!found && $3 !~ /\^\{\}$/ && $3 ~ /^v[0-9]+\.[0-9]+\.[0-9]+$/ { print $3; found=1 }')
  [[ -n $target_version ]] || { printf 'Could not discover latest %s release tag\n' "$repo" >&2; exit 1; }
fi

if [[ $target_version == "$current_version" ]]; then
  printf 'Already at %s@%s\n' "$module" "$current_version" >&2
  exit 0
fi

work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT

prepare_snapshot "$mode" "$module" "$current_version" "$current_sum" "$current_gomod_sum" "$work/base" "$target_module" >/dev/null
IFS=$'\t' read -r next_sum next_gomod_sum next_src < <(prepare_snapshot "$mode" "$module" "$target_version" "" "" "$work/next" "$target_module")

mapfile -t files < <(
  {
    (cd "$work/base" && find . -type f | sed 's|^\./||')
    (cd "$work/next" && find . -type f | sed 's|^\./||')
  } | sort -u
)

conflicts=()
for rel in "${files[@]}"; do
  case "$rel" in
    go.mod|go.sum) continue ;;
  esac
  local_path="$rel"
  [[ -n $dest_prefix ]] && local_path="$dest_prefix/$rel"

  in_base=false
  in_next=false
  [[ -f $work/base/$rel ]] && in_base=true
  [[ -f $work/next/$rel ]] && in_next=true

  if $in_base && $in_next; then
    if cmp -s "$work/base/$rel" "$work/next/$rel"; then
      continue
    fi
    if [[ ! -f $local_path ]]; then
      printf 'Upstream modified locally deleted file: %s\n' "$local_path" >&2
      conflicts+=("$local_path")
      continue
    fi
    if ! git merge-file -L "mixray" -L "upstream/$current_version" -L "upstream/$target_version" \
      "$local_path" "$work/base/$rel" "$work/next/$rel"; then
      conflicts+=("$local_path")
    fi
  elif ! $in_base && $in_next; then
    mkdir -p "$(dirname "$local_path")"
    if [[ -f $local_path ]]; then
      if ! git merge-file -L "mixray" -L "empty" -L "upstream/$target_version" \
        "$local_path" /dev/null "$work/next/$rel"; then
        conflicts+=("$local_path")
      fi
    else
      cp -p -- "$work/next/$rel" "$local_path"
    fi
  elif $in_base && ! $in_next; then
    if [[ -f $local_path ]]; then
      if cmp -s "$local_path" "$work/base/$rel"; then
        rm -- "$local_path"
      else
        printf 'Upstream deleted locally modified file: %s\n' "$local_path" >&2
        conflicts+=("$local_path")
      fi
    fi
  fi
done

# Merge upstream go.mod requirements without downgrading local versions.
declare -A local_deps=()
while IFS=$'\t' read -r p v; do
  [[ -n $p ]] && local_deps["$p"]=$v
done < <(go mod edit -json | jq -r '.Require // [] | .[] | "\(.Path)\t\(.Version)"')

declare -A vendored_modules=()
while IFS= read -r vm; do
  [[ -n $vm ]] && vendored_modules["$vm"]=1
done < <(jq -r '.vendored // {} | .[].module' upstream.json)

upgrades=()
while IFS=$'\t' read -r p v; do
  [[ -n $p ]] || continue
  [[ -n ${vendored_modules[$p]:-} ]] && continue
  cur=${local_deps[$p]:-}
  if [[ -z $cur ]]; then
    upgrades+=("$p@$v")
  elif [[ $cur != "$v" ]] && [[ $(printf '%s\n%s\n' "$cur" "$v" | sort -V | tail -n1) == "$v" ]]; then
    upgrades+=("$p@$v")
  fi
done < <(go mod edit -json "$next_src/go.mod" | jq -r '.Require // [] | .[] | "\(.Path)\t\(.Version)"')

if (( ${#upgrades[@]} )); then
  go get "${upgrades[@]}"
fi
go mod tidy

jq --arg v "$target_version" --arg s "$next_sum" --arg g "$next_gomod_sum" \
  "$jq_path.version = \$v | $jq_path.sum = \$s | $jq_path.gomod_sum = \$g" \
  upstream.json > upstream.json.tmp
mv upstream.json.tmp upstream.json

if (( ${#conflicts[@]} )); then
  printf 'Sync %s %s -> %s completed with conflicts in:\n' "$name" "$current_version" "$target_version" >&2
  printf '  %s\n' "${conflicts[@]}" >&2
  exit 1
fi

printf 'Synced %s (%s): %s -> %s\n' "$name" "$module" "$current_version" "$target_version" >&2

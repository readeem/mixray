#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
export GOWORK=off

# shellcheck source=scripts/lib.sh
source "$root/scripts/lib.sh"

target_module="github.com/readeem/mixray"
module=$(jq -er '.mihomo.module' upstream.json)
repo=$(jq -er '.mihomo.repo' upstream.json)
current_version=$(jq -er '.mihomo.version' upstream.json)
current_sum=$(jq -er '.mihomo.sum' upstream.json)
current_gomod_sum=$(jq -er '.mihomo.gomod_sum' upstream.json)

if (( $# > 1 )); then
  printf 'usage: %s [vX.Y.Z]\n' "$0" >&2
  exit 2
fi

if (( $# == 1 )); then
  target_version=$1
else
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

prepare_upstream_snapshot "$module" "$current_version" "$current_sum" "$current_gomod_sum" "$work/base" "$target_module" >/dev/null
IFS=$'\t' read -r next_sum next_gomod_sum < <(prepare_upstream_snapshot "$module" "$target_version" "" "" "$work/next" "$target_module")

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
  in_base=false
  in_next=false
  [[ -f $work/base/$rel ]] && in_base=true
  [[ -f $work/next/$rel ]] && in_next=true

  if $in_base && $in_next; then
    if cmp -s "$work/base/$rel" "$work/next/$rel"; then
      continue
    fi
    if [[ ! -f $rel ]]; then
      printf 'Upstream modified locally deleted file: %s\n' "$rel" >&2
      conflicts+=("$rel")
      continue
    fi
    if ! git merge-file -L "mixray" -L "upstream/$current_version" -L "upstream/$target_version" \
      "$rel" "$work/base/$rel" "$work/next/$rel"; then
      conflicts+=("$rel")
    fi
  elif ! $in_base && $in_next; then
    mkdir -p "$(dirname "$rel")"
    if [[ -f $rel ]]; then
      if ! git merge-file -L "mixray" -L "empty" -L "upstream/$target_version" \
        "$rel" /dev/null "$work/next/$rel"; then
        conflicts+=("$rel")
      fi
    else
      cp -p -- "$work/next/$rel" "$rel"
    fi
  elif $in_base && ! $in_next; then
    if [[ -f $rel ]]; then
      if cmp -s "$rel" "$work/base/$rel"; then
        rm -- "$rel"
      else
        printf 'Upstream deleted locally modified file: %s\n' "$rel" >&2
        conflicts+=("$rel")
      fi
    fi
  fi
done

# Merge upstream go.mod requirements without downgrading local versions.
declare -A local_deps=()
while IFS=$'\t' read -r p v; do
  [[ -n $p ]] && local_deps["$p"]=$v
done < <(go mod edit -json | jq -r '.Require // [] | .[] | "\(.Path)\t\(.Version)"')

upgrades=()
while IFS=$'\t' read -r p v; do
  [[ -n $p ]] || continue
  cur=${local_deps[$p]:-}
  if [[ -z $cur ]]; then
    upgrades+=("$p@$v")
  elif [[ $cur != "$v" ]] && [[ $(printf '%s\n%s\n' "$cur" "$v" | sort -V | tail -n1) == "$v" ]]; then
    upgrades+=("$p@$v")
  fi
done < <(go mod edit -json "$work/next/go.mod" | jq -r '.Require // [] | .[] | "\(.Path)\t\(.Version)"')

if (( ${#upgrades[@]} )); then
  go get "${upgrades[@]}"
fi
go mod tidy

jq --arg v "$target_version" --arg s "$next_sum" --arg g "$next_gomod_sum" \
  '.mihomo.version = $v | .mihomo.sum = $s | .mihomo.gomod_sum = $g' \
  upstream.json > upstream.json.tmp
mv upstream.json.tmp upstream.json

if (( ${#conflicts[@]} )); then
  printf 'Sync %s -> %s completed with conflicts in:\n' "$current_version" "$target_version" >&2
  printf '  %s\n' "${conflicts[@]}" >&2
  exit 1
fi

printf 'Synced %s: %s -> %s\n' "$module" "$current_version" "$target_version" >&2

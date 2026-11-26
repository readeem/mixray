#!/usr/bin/env bash
set -euo pipefail

# Shared helpers for preparing a rewritten upstream mihomo snapshot.
is_skipped_upstream() {
  case "$1" in
    .git|.github|.gitignore|.golangci.yaml|Dockerfile|LICENSE|Makefile|Meta.png|README.md|check_amd64.sh|docker|docs|flake.lock|flake.nix)
      return 0 ;;
    *)
      return 1 ;;
  esac
}

prepare_upstream_snapshot() {
  local module=$1 version=$2 want_sum=$3 want_gomod_sum=$4 dest=$5 target_module=$6
  local meta got_sum got_gomod_sum src entry name file

  meta=$(go mod download -json "$module@$version")
  got_sum=$(jq -er .Sum <<<"$meta")
  got_gomod_sum=$(jq -er .GoModSum <<<"$meta")
  src=$(jq -er .Dir <<<"$meta")

  if [[ -n $want_sum && $got_sum != "$want_sum" ]]; then
    printf 'Sum mismatch for %s@%s: want %s, got %s\n' "$module" "$version" "$want_sum" "$got_sum" >&2
    return 1
  fi
  if [[ -n $want_gomod_sum && $got_gomod_sum != "$want_gomod_sum" ]]; then
    printf 'GoModSum mismatch for %s@%s: want %s, got %s\n' "$module" "$version" "$want_gomod_sum" "$got_gomod_sum" >&2
    return 1
  fi

  mkdir -p "$dest"
  shopt -s dotglob nullglob
  for entry in "$src"/*; do
    name=$(basename "$entry")
    if is_skipped_upstream "$name"; then
      continue
    fi
    cp -R -- "$entry" "$dest/$name"
    chmod -R u+w -- "$dest/$name"
  done
  shopt -u dotglob nullglob

  while IFS= read -r -d '' file; do
    sed -i \
      -e "s|\"$module|\"$target_module|g" \
      -e "s|//go:linkname \([^ ]*\) $module/|//go:linkname \1 $target_module/|g" \
      "$file"
  done < <(find "$dest" -name '*.go' -type f -print0)

  printf '%s\t%s\n' "$got_sum" "$got_gomod_sum"
}

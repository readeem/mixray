#!/usr/bin/env bash
set -euo pipefail

# Shared helpers for preparing rewritten upstream snapshots (root mihomo and vendored modules).
is_skipped_root() {
  case "$1" in
    .git|.github|.gitignore|.golangci.yaml|Dockerfile|LICENSE|Makefile|Meta.png|README.md|check_amd64.sh|docker|docs|flake.lock|flake.nix)
      return 0 ;;
    *)
      return 1 ;;
  esac
}

is_skipped_vendored() {
  case "$1" in
    .git|.github|.gitignore|.golangci.yaml|.golangci.yml|CODE_OF_CONDUCT.md|Makefile|README.md|SECURITY.md|go.mod|go.sum|testing)
      return 0 ;;
    *)
      return 1 ;;
  esac
}

prepare_snapshot() {
  local mode=$1 module=$2 version=$3 want_sum=$4 want_gomod_sum=$5 dest=$6 target_module=$7
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
    if [[ $mode == root ]]; then
      is_skipped_root "$name" && continue
    else
      is_skipped_vendored "$name" && continue
    fi
    cp -R -- "$entry" "$dest/$name"
    chmod -R u+w -- "$dest/$name"
  done
  shopt -u dotglob nullglob

  if [[ $mode == vendored ]]; then
    rm -rf -- \
      "$dest/infra/vformat" "$dest/infra/vprotogen" \
      "$dest/main/commands" "$dest/main/confloader/external" "$dest/main/distro" "$dest/main/json" "$dest/main/toml" "$dest/main/yaml" \
      "$dest/main/main.go" "$dest/main/run.go" "$dest/main/version.go"
    find "$dest" -name '*_test.go' -type f -delete
  fi

  while IFS= read -r -d '' file; do
    sed -i \
      -e "s|\"$module|\"$target_module|g" \
      -e "s|//go:linkname \([^ ]*\) $module/|//go:linkname \1 $target_module/|g" \
      "$file"
  done < <(find "$dest" -name '*.go' -type f -print0)

  printf '%s\t%s\t%s\n' "$got_sum" "$got_gomod_sum" "$src"
}

prepare_upstream_snapshot() {
  prepare_snapshot root "$@"
}

prepare_vendored_snapshot() {
  prepare_snapshot vendored "$@"
}

#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
export GOWORK=off

upstream_version=$(jq -er '.mihomo.version' upstream.json)
release_version=${1:-${MIXRAY_VERSION:-dev}}
build_time=$(date -u +%Y-%m-%dT%H:%M:%SZ)

if [[ $release_version == dev ]]; then
  full_version="${upstream_version}+mixray.dev"
else
  full_version="${upstream_version}+mixray.${release_version#v}"
fi

ldflags="-s -w -X github.com/readeem/mixray/constant.Version=$full_version -X github.com/readeem/mixray/constant.BuildTime=$build_time"

rm -rf dist
mkdir -p dist

targets=(
  linux/amd64
  linux/arm64
  windows/amd64
  windows/arm64
  darwin/amd64
  darwin/arm64
)

work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT

for target in "${targets[@]}"; do
  os=${target%/*}
  arch=${target#*/}
  ext=""
  if [[ $os == windows ]]; then
    ext=".exe"
  fi
  bin="mixray${ext}"
  stage="$work/${os}-${arch}"
  mkdir -p "$stage"

  printf 'Building %s/%s (%s)\n' "$os" "$arch" "$full_version" >&2
  GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath -ldflags "$ldflags" -o "$stage/$bin" .

  base="mixray-${os}-${arch}-${release_version}"
  if [[ $os == windows ]]; then
    (cd "$stage" && zip -q -r "$root/dist/${base}.zip" "$bin")
  else
    tar -C "$stage" -czf "$root/dist/${base}.tar.gz" "$bin"
  fi
done

(cd dist && sha256sum mixray-* > SHA256SUMS)
printf 'Built %d archives in dist/\n' "${#targets[@]}" >&2

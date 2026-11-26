#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
export GOWORK=off

# Check pinned upstream modules by their upstream identity, since rewritten
# module paths are invisible to govulncheck's source/binary scanners.
pins=("$(jq -er '.mihomo | "\(.module)@\(.version)"' upstream.json)")
while IFS= read -r pin; do
  [[ -n $pin ]] && pins+=("$pin")
done < <(jq -r '.vendored // {} | to_entries[] | "\(.value.module)@\(.value.version)"' upstream.json)

advisories=$(govulncheck -mode query -format json "${pins[@]}" | jq -s '[.[] | .osv? | select(.) | {id, summary}]')
if [[ $advisories != '[]' ]]; then
  printf 'Advisories for pinned upstream module versions:\n%s\n' "$advisories" >&2
  exit 1
fi

work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
CGO_ENABLED=0 go build -trimpath -o "$work/mixray" .
govulncheck -mode binary "$work/mixray"

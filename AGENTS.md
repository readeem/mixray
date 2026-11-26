# mixray

`mixray` is a self-contained Go module (`github.com/readeem/mixray`) and standalone cross-platform binary tracking upstream `MetaCubeX/mihomo` releases via 3-way merge.

## Upstream sync model

- `upstream.json` records the currently integrated upstream `mihomo` release tag and its verified Go checksums (`sum`, `gomod_sum`).
- `scripts/sync-upstream.sh [vX.Y.Z]` downloads the pinned base and target upstream releases (rewritten to `github.com/readeem/mixray`), 3-way merges upstream's diff into the working tree with `git merge-file`, and merges newer `go.mod` requirements without downgrading local dependency versions.
- `scripts/diff-upstream.sh` shows every difference against the pinned upstream release.

## Verifying

Run `mise run check` and `mise run vulncheck` before handoff.

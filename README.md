# mixray

Rule-based proxy core built on upstream [MetaCubeX/mihomo](https://github.com/MetaCubeX/mihomo) (routing, rules, DNS, TUN, external controller) with native Xray-core outbound support, packaged as both a standalone cross-platform binary and an importable Go module (`github.com/readeem/mixray`).

## License & Attribution

Licensed under the [GNU General Public License v3.0](LICENSE).
Built from upstream [MetaCubeX/mihomo](https://github.com/MetaCubeX/mihomo) (GPL-3.0). `mixray` is an independent downstream project and is not affiliated with MetaCubeX.

## Development

Requires [mise](https://mise.jdx.dev/) (pins Go and `govulncheck`):

```bash
mise install
mise run check          # Vet, cross-compile check, and test
mise run vulncheck      # Scan binary and upstream pins for vulnerabilities
mise run diff-upstream  # Inspect changes against the pinned upstream release
mise run sync           # 3-way merge the latest upstream mihomo release
mise run build          # Build linux/windows/darwin release archives into dist/
```

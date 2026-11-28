<div align="center">

<h1>
  <img src="assets/logo.svg" alt="" height="32">
  mixray
</h1>

A mixed combination of [MetaCubeX/mihomo](https://github.com/MetaCubeX/mihomo) and Xray-core in one binary.

</div>

## License & Attribution

The software is licensed under the [GNU General Public License v3.0](LICENSE).
Built from upstream [MetaCubeX/mihomo](https://github.com/MetaCubeX/mihomo) (GPL-3.0). `mixray` is an independent downstream project and is not affiliated with MetaCubeX.

[The project icon](assets/logo.svg) is Readeem’s personal logo and is subject to separate proprietary terms. © 2026 Readeem. All rights reserved.

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

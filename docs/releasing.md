# Releasing

Pushing a `v*` tag runs the [release workflow](../.github/workflows/release.yml), which builds with GoReleaser ([.goreleaser.yml](../.goreleaser.yml)) and publishes:

| What | Where |
|------|-------|
| `pgglass_<version>_<os>_<arch>.tar.gz` for macOS and Linux, amd64 and arm64, built with `CGO_ENABLED=0` | GitHub release |
| An SPDX SBOM per archive (`<archive>.sbom.json`) and `checksums.txt`, the SHA-256 of both | GitHub release |
| The image `ghcr.io/jpgomesf/pgglass:<version>` and `:latest` for linux/amd64 and linux/arm64, on distroless static (nonroot), with an SBOM attestation | GHCR |
| Build-provenance attestations for every file in `checksums.txt` and for the image | GitHub, and GHCR for the image |

A pre-release tag, such as `v1.1.0-rc.1`, publishes a GitHub pre-release and the image tag `1.1.0-rc.1`, and moves neither GitHub's latest release nor the `latest` image, so `install.sh` keeps installing the last stable release.

The release job is the only one with write permissions: `contents` to create the release, `packages` to push the image and its attestation, `id-token` to sign the attestations with Sigstore, and `attestations` to store them.

Every action the workflows use is pinned to a commit SHA, GoReleaser to an exact version and the image's base to a digest, so a moved tag cannot change what a release runs. [Dependabot](../.github/dependabot.yml) proposes updates to them and to the Go modules each week, for versions out at least 7 days.

## Before the first release

1. Rename the repository to `Jpgomesf/pgglass`: `install.sh`, `server.json`, the image labels and the commands below use that name.
2. After the first release, make the image public: on GitHub, open Packages, then pgglass, Package settings, Change visibility. GHCR creates packages private, and both `docker pull` without a login and the MCP Registry need it public.

## Cut a release

1. Check that CI is green on main.
2. In [server.json](../server.json), set `version` and the tag in `packages[0].identifier` to the new version, without the `v`, and commit. `go test -run TestServerJSON .` checks that they agree.
3. Tag and push:

   ```bash
   git tag -a v1.0.0 -m v1.0.0
   git push origin v1.0.0
   ```

4. When the workflow is done, verify what it published:

   ```bash
   gh release download v1.0.0 --repo Jpgomesf/pgglass --pattern 'pgglass_1.0.0_linux_amd64.tar.gz'
   gh attestation verify pgglass_1.0.0_linux_amd64.tar.gz --repo Jpgomesf/pgglass
   gh attestation verify oci://ghcr.io/jpgomesf/pgglass:1.0.0 --repo Jpgomesf/pgglass
   ```

## Publish to the MCP Registry

Publishing is a manual step, after the release, once the image is public. The registry checks that the image's `io.modelcontextprotocol.server.name` label equals the `name` in `server.json`, and accepts each version once. Like the README's docker command, the entry runs the image with `--user` set to the `uid:gid` the client asks for: as the image's own user, uid 65532, the server could not read a `config.json` only its owner can read through a bind mount on Linux.

```bash
brew install mcp-publisher
mcp-publisher validate
mcp-publisher login github
mcp-publisher publish
```

The name is `io.github.Jpgomesf/pgglass`, with the login's capital J: the registry grants a GitHub login the namespace `io.github.<login>` in the login's exact case ([modelcontextprotocol/registry#689](https://github.com/modelcontextprotocol/registry/issues/689)).

## Try a release locally

With Docker (with buildx) and [syft](https://github.com/anchore/syft) installed:

```bash
goreleaser check
goreleaser release --snapshot --clean
```

The second command, also `make release-local`, builds the archives, SBOMs and `checksums.txt` in `dist/` and loads `ghcr.io/jpgomesf/pgglass:<version>-amd64` and `-arm64` into Docker. It publishes nothing. `go test -run TestInstallScript .` runs `install.sh` against a local fake release.

## Platforms

Releases cover macOS and Linux on amd64 and arm64. Windows is left out because of how spill files stay private: a result too large to return inline is written to a file that only its user may read, which pgglass enforces with Unix permissions, a 0700 directory, 0600 files and an owner check on its temp-directory fallback. Windows does not apply those modes, so spilled query results would be only as private as the directory's inherited ACLs, which pgglass neither sets nor checks.

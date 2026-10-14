# Releasing and verifying WaWarden

This document describes the repository settings a release depends on, how
releases are produced, what each release contains and under which licences, how
to verify one, and the checklists the maintainer follows.

## Contents

- [Repository settings](#repository-settings)
- [How a release is produced](#how-a-release-is-produced)
  - [Build and rebuild](#build-and-rebuild)
  - [What a release contains](#what-a-release-contains)
- [Licences](#licences)
- [Verifying a release](#verifying-a-release)
- [Versioning](#versioning)
  - [Supported versions](#supported-versions)
  - [Compatibility promise](#compatibility-promise)
- [Maintainer checklists](#maintainer-checklists)

## Repository settings

The approval gate and the immutability of releases come from repository settings,
not from the workflow file. When a job names an environment that does not exist,
GitHub creates it with no protection rules, so these settings are what keeps a
release from publishing without approval.

The following settings are in place. Each command below is read-only and prints
the value shown when the setting holds; run them with an authenticated `gh`. Some
of these endpoints answer only to repository administrators.

- **Private vulnerability reporting is enabled.**
  `gh api repos/dortort/wawarden/private-vulnerability-reporting --jq .enabled`
  prints `true`.
- **Releases are immutable.**
  `gh api repos/dortort/wawarden/immutable-releases --jq .enabled` prints `true`.
- **The `release` environment requires a reviewer's approval.**
  `gh api repos/dortort/wawarden/environments/release --jq '[.protection_rules[].type]'`
  prints `["required_reviewers","branch_policy"]`. Self-review is allowed: with a
  single maintainer, preventing it would block every release.
- **Administrators cannot bypass the `release` environment's protection rules.**
  `gh api repos/dortort/wawarden/environments/release --jq .can_admins_bypass`
  prints `false`.
- **The `release` environment accepts deployments only from `main`.**
  `gh api repos/dortort/wawarden/environments/release --jq .deployment_branch_policy`
  prints `{"custom_branch_policies":true,"protected_branches":false}`, and
  `gh api repos/dortort/wawarden/environments/release/deployment-branch-policies --jq '[.branch_policies[] | .type + ":" + .name]'`
  prints `["branch:main"]`.
- **`main` cannot be deleted or force-pushed, and requires a linear history.**
  `gh api repos/dortort/wawarden/rules/branches/main --jq '[.[].type]'` prints
  `["deletion","non_fast_forward","required_linear_history"]`. A linear history
  means a merge commit cannot be pushed to `main`, so contributors rebase
  (`git pull --rebase`) before pushing. The rules come from the branch ruleset:
  `gh api repos/dortort/wawarden/rulesets --jq '.[] | select(.target == "branch") | .id'`
  prints its id, and
  `gh api repos/dortort/wawarden/rulesets/<id> --jq '{enforcement, target, include: .conditions.ref_name.include, rules: [.rules[].type], bypass_actors}'`
  prints
  `{"bypass_actors":[],"enforcement":"active","include":["~DEFAULT_BRANCH"],"rules":["deletion","non_fast_forward","required_linear_history"],"target":"branch"}`:
  enforced, targeting the default branch, `main`, with no bypass actors.
- **`v*` tags cannot be deleted or moved.**
  `gh api repos/dortort/wawarden/rulesets --jq '.[] | select(.target == "tag") | .id'`
  prints the tag ruleset's id, and
  `gh api repos/dortort/wawarden/rulesets/<id> --jq '{enforcement, include: .conditions.ref_name.include, rules: [.rules[].type], bypass_actors}'`
  prints
  `{"bypass_actors":[],"enforcement":"active","include":["refs/tags/v*"],"rules":["deletion","update","non_fast_forward"]}`.
  Creating a tag is not restricted, because the release workflow creates it.
- **Actions must be pinned by commit SHA.**
  `gh api repos/dortort/wawarden/actions/permissions --jq .sha_pinning_required`
  prints `true`. GitHub refuses to run a workflow that references an action by tag
  or branch.
- **The default workflow token is read-only.**
  `gh api repos/dortort/wawarden/actions/permissions/workflow --jq .default_workflow_permissions`
  prints `read`.

Neither ruleset has bypass actors, so they apply to administrators too.

The release workflow's first job reads the `release` environment through the API
and stops unless it exists, has a required reviewer, does not let administrators
bypass its protection rules, and accepts deployments only from the branch `main`.
The workflow token cannot read the other settings: check them with the commands
above after any change to the repository settings.

## How a release is produced

- Releases are cut **only from `main`**, by the manually dispatched workflow
  `.github/workflows/release.yml`. Nobody pushes release tags by hand.
- The workflow has five jobs: `guard` (*Check version, tag, environment and
  CI*), then `build` (*Build*), then `rebuild` (*Rebuild and compare*) and
  `darwin` (*Rebuild and run on macOS*, once on each of two macOS runners)
  side by side, and last `release` (*Publish*), the publishing job. Every job
  runs only when the workflow was dispatched on `refs/heads/main`.
- The workflow input is the version. The `guard` job accepts only
  `vMAJOR.MINOR.PATCH` with decimal numbers without leading zeros and no suffix
  (`v0.1.0`, not `v0.01.0` or `v1.0.0-rc.1`), and stops when `git ls-remote`
  finds the tag in the repository or fails. The publishing job checks again
  that the tag does not exist just before it creates the release.
- The `guard` job also checks the `release` environment as described under
  [Repository settings](#repository-settings), and stops unless `ci.yml` has a
  successful `push` run on `main` for the commit being released. Dispatch only
  after CI on `main` has finished.
- The job that publishes runs in the `release` environment, so it starts only
  after the maintainer approves it. In the release workflow, the permissions that
  can write to the repository or the registry (`contents: write`,
  `packages: write`, `attestations: write`) and the OIDC token used for signing
  (`id-token: write`) are granted only to that job, so they exist only after
  approval.
- Outside the release workflow, one job holds `id-token: write`: the OpenSSF
  Scorecard job (`.github/workflows/scorecard.yml`), which needs it to publish its
  results. It runs on pushes to `main` and on a schedule, does not build or run the
  repository's code, and holds no `contents`, `packages` or `attestations` write
  permission. The other write permissions outside the release workflow are
  `security-events: write` (Scorecard and CodeQL upload code-scanning results),
  `issues: write` (the daily `govulncheck` job manages one issue), and
  `contents: write` and `pull-requests: write`, held by the job of the
  [whatsmeow bump workflow](#the-whatsmeow-bump-workflow) that pushes the
  branch `bump/whatsmeow` and opens its pull request, and runs no Go code.
- The publishing job pushes the image by digest, without a tag. Only after the
  pushed digest matches the build job's, and the index is signed and attested,
  does it add the version tag.
- The workflow creates the tag and the GitHub release itself, on the commit it
  built. The release notes, including the image index digest, are written first.
  `gh release create` then creates the release as a draft, attaches every asset,
  and only then publishes it.
- Releases are immutable (see [Repository settings](#repository-settings)): once
  published, a release's tag cannot be moved or deleted, its assets cannot be
  replaced, and its tag name cannot be reused, even if the release is deleted.
  The workflow token cannot read this setting before publishing, so the setting
  is the control: the workflow only checks the published release afterwards and
  fails if it does not report itself as immutable.

### Build and rebuild

The build is reproducible. `hack/repro-build.sh` is the single recipe used by the
release workflow, by CI and by anyone reproducing a release:

- Go binaries for `linux/amd64`, `linux/arm64`, `darwin/amd64` and
  `darwin/arm64`, built with `CGO_ENABLED=0`,
  `-trimpath`, `-buildvcs=true`, `-mod=readonly` and an empty build ID, with the
  toolchain named by the `toolchain` line in `go.mod` (`GOTOOLCHAIN=local`): the
  script stops unless `go env GOVERSION` equals that line. In `ci.yml`,
  `codeql.yml`, `release.yml`, `govulncheck-daily.yml` and `bump-whatsmeow.yml`,
  every job that runs Go also runs `hack/check-go-version.sh` right after
  setting Go up, which makes
  the same comparison in the job's own environment. The script accepts any
  `VERSION` that is a valid image tag (CI builds with `VERSION=ci`); the version
  string is injected at link time into `internal/buildinfo.Version`, and
  `wawarden version` prints it.
- Modules are pinned by `go.sum` and never vendored. The script stops when
  anything named `vendor` exists at the repository root, and builds with
  `-mod=readonly`, which fails rather than change `go.mod` or `go.sum`. A
  build downloads the modules through the Go module proxy, or whatever
  `GOPROXY` names. The go command checks each download against its hash in
  `go.sum`. With default settings, it checks a new hash against the Go
  checksum database (`sum.golang.org`) before adding it to `go.sum`. The CI
  `modules` job and an architecture test also fail when anything named `vendor`
  exists at the repository root. Another architecture test requires every
  workflow that sets up Go to set `GOFLAGS: -mod=readonly` in its top-level
  `env`, and fails on any line of such a workflow that mentions `GOFLAGS` but is
  not exactly `GOFLAGS: -mod=readonly`, and on any `-mod` flag there that is not
  written `-mod=readonly`, such as `-mod vendor`, `-mod=vendor` or `--mod=mod`.
  It reads the workflow text only, so it does not see a module mode chosen in a
  script that a workflow calls.
- Before packaging, the script checks each binary:
  - its embedded build settings show no `dev` tag, `vcs.revision` equal to the
    commit, `vcs.modified=false`, `CGO_ENABLED=0` and the right operating
    system (`GOOS`) and architecture (`GOARCH`);
  - it contains neither the marker string that only dev builds carry nor the
    marker string of the development-only fake engine;
  - `go tool nm` lists no symbol of `internal/engine/fake`;
  - it contains the version variable that the link step sets (`go tool nm`
    lists `internal/buildinfo.Version`).
- Only in its `binaries` and `oci` modes, the script also runs the binary built
  for the host's operating system and architecture, with an empty environment
  (`env -i`). `wawarden version` must print exactly the requested version, the
  commit and `dev build false`. In the release workflow, the `build` job runs
  `linux/amd64` on an `amd64` runner, the `rebuild` job runs `linux/arm64` on
  an `arm64` runner, and the `darwin` jobs run `darwin/arm64` on `macos-26`
  (Apple silicon) and `darwin/amd64` on `macos-26-intel`. All four release
  binaries are therefore run before anything is published.
- The publishing job runs the script in `push` mode, which runs none of the
  binaries it builds: that job holds the write permissions, so it runs no code
  built from the repository. It proves instead that its binaries are
  byte-identical to the ones the `build`, `rebuild` and `darwin` jobs ran. The
  image it pushes must have the `build` job's index digest, and its
  `SHA256SUMS` must equal the `build` job's, which the `rebuild` and `darwin`
  jobs reproduced.
- After building the binaries, the script runs `hack/licenses.sh`, which
  collects the licence files of the Go standard library and of every module
  linked into any of them (see [Licences](#licences)) into `dist/licenses`, and packs that directory into
  `wawarden_<version>_licenses.tar.gz` with `hack/tarball.go`. `SHA256SUMS`
  lists this archive next to the binaries, so every comparison of
  `SHA256SUMS` below covers it too.
- `SOURCE_DATE_EPOCH` is the commit time. File times inside the image are set
  to it.
- A multi-arch OCI image built by BuildKit from a digest-pinned
  `gcr.io/distroless/static-debian13:nonroot` base. The image adds the binary,
  `/wawarden`, and an empty `/data` directory owned by the runtime user with mode
  `0700` and the licence files under `/licenses` to the base, runs as
  `65532:65532` and has no shell. The image's runtime
  contract is in [`docs/configuration.md`](docs/configuration.md#container-image).
- The script creates its own temporary `docker-container` builder from a BuildKit
  image pinned by version and digest in the script, and removes it afterwards. The
  workflows pin the buildx version in `BUILDX_VERSION` and download that buildx
  release on every run instead of restoring it from the GitHub Actions cache, and
  the script stops when `BUILDX_VERSION` is set and the installed buildx reports
  another version. No build cache is used, and BuildKit's own provenance and SBOM
  attestations are switched off, because they differ between identical builds.
  Provenance is attested separately (below).
- The release jobs check out the commit shallowly and fetch no tags
  (`fetch-depth: 1`, `fetch-tags: false`), so the build sees no tag. The script
  stops if any tag is reachable from the commit.
- Every GitHub Action in the workflows is pinned by commit SHA, which the
  repository requires (see [Repository settings](#repository-settings)).

The `rebuild` job then runs the same script from scratch on a fresh runner of the
other architecture: `arm64`, where the `build` job ran on `amd64`. **The release
proceeds only when both jobs produce identical binary checksums and an identical
image index digest.** Both jobs also generate the SPDX SBOMs, and the release
stops unless the two sets are identical apart from the document namespace and the
creation time. The publishing job builds and
pushes the image a third time and stops unless the pushed digest equals the one
both builds produced.

The `darwin` jobs run the script in `binaries` mode, which builds the binaries
and the licence archive but no image, on `macos-26` and `macos-26-intel`: the
macOS runners have no Docker. Each runs the darwin binary for its runner, and
stops unless its `SHA256SUMS` equals the `build` job's. They add a third and a
fourth independent build of every binary, on another operating system.

### What a release contains

| Artifact | Where |
|---|---|
| Multi-arch image (`linux/amd64`, `linux/arm64`) | `ghcr.io/dortort/wawarden@sha256:<digest>`, also tagged `<version>`; the release notes state the index digest |
| Binaries `wawarden_<version>_linux_amd64` and `wawarden_<version>_linux_arm64`, static executables, and from `v1.1.0` on `wawarden_<version>_darwin_amd64` and `wawarden_<version>_darwin_arm64`, which run on macOS 13 Ventura or later | Release assets |
| From `v0.2.0` on, the licence files of the Go standard library, of WaWarden and of every module linked into any of the binaries, `wawarden_<version>_licenses.tar.gz` | Release asset, and `/licenses` in the image |
| `SHA256SUMS` for the binaries and, from `v0.2.0` on, the licence archive | Release asset |
| SPDX 2.3 SBOMs for the binaries, `wawarden_<version>_<os>_<arch>.spdx.json`, the `darwin` ones from `v1.1.0` on | Release assets, and attested to each binary in GitHub's attestation store |
| Build provenance attestations (SLSA, via `actions/attest-build-provenance`): one for the image index, and one whose subjects are the files `SHA256SUMS` lists | GitHub's attestation store; the image attestation is also pushed to the registry, and the files' Sigstore bundle is the release asset `wawarden_<version>_provenance.sigstore.json` |
| Keyless cosign signature on the image index, made with cosign v3.1.3 | Registry, next to the image |

The SBOMs list the Go modules and the Go standard library compiled into each
binary. No SBOM covers the image as a whole, so the packages of the distroless
base image are not listed; no image-level SBOM is produced.

Binary sizes, measured on 2026-10-14 with
`CGO_ENABLED=0 go build -trimpath ./cmd/wawarden` and the target's `GOOS` and
`GOARCH`: about 31.8 MiB for `linux/amd64`, 30.2 MiB for `linux/arm64`,
32.2 MiB for `darwin/amd64` and 30.8 MiB for `darwin/arm64`. Each release's
assets give the exact sizes.

Image sizes were measured on 2026-10-14 from the registry manifests of
`v1.0.0`: the compressed layers add up to about 17.1 MiB (`linux/amd64`) and
16.1 MiB (`linux/arm64`), and the uncompressed layers of `linux/arm64` to about
33.8 MiB.

Memory was measured with `hack/repro-build.sh` on the code of `v0.2.0`, and has
not been measured again since:

- an unpaired `serve` left idle for two minutes in that image on
  `linux/arm64`, with no network, a read-only root filesystem and a named
  volume on `/data`, held a resident set of about 22.5 MiB. That was 8.0 MiB of
  its own memory and 14.5 MiB of the binary's mapped pages; `docker stats`
  reported 9.9 MiB for the container.

A paired service needs more memory. It grows with message traffic and with
history sync, whose blobs `WAWARDEN_HISTORY_MAX_BYTES` caps.

Signing and attesting use GitHub's OIDC identity for the release workflow on
`main`. No long-lived signing key exists.

## Licences

WaWarden is licensed under `GPL-3.0-or-later` from `v0.2.0` on; `v0.1.0` was
released under the MIT licence. From `v0.2.0` on, the image's
`org.opencontainers.image.licenses` label, set in the `Dockerfile`, and the same
annotation on the image index, set by `hack/repro-build.sh`, are
`GPL-3.0-or-later`.

The binaries link the Go standard library, under Go's BSD-style licence and
patent grant. From `v0.2.0` on, they also link third-party Go modules, each
under its own licence:

| Licence | Modules |
|---|---|
| MPL-2.0 | The protocol library `go.mau.fi/whatsmeow` and its `go.mau.fi/util` |
| GPL-3.0 | The Signal library, `go.mau.fi/libsignal` |
| Apache-2.0 | `github.com/petermattis/goid`, and the MCP library `github.com/modelcontextprotocol/go-sdk` |
| MIT | The MCP library's code contributed before its move to Apache-2.0, unless its authors relicensed it (its licence file carries both texts), and several other modules |
| ISC | `github.com/coder/websocket` |
| BSD-3-Clause | The rest, the SQLite driver and its C library translation included |

[`README.md`](README.md#licence) lists every module. GPL-3.0 is why WaWarden
itself is GPL-3.0-or-later from `v0.2.0` on. Every release ships their licence
texts, the GPL-3.0 and MPL-2.0 texts included, and WaWarden's own:

- `hack/licenses.sh` lists, with `go list -deps`, the modules whose packages are
  compiled into `./cmd/wawarden` for `linux` and `darwin`, each on `amd64` and
  `arm64`, and copies
  every regular file at the root of each module whose name starts with
  `LICENSE`, `LICENCE`, `COPYING`, `NOTICE`, `PATENTS` or `UNLICENSE`, in any
  letter case, to `dist/licenses/<module path>@<version>/`: from the Go module
  cache for the third-party modules, and from the repository root, without a
  version, for WaWarden. The script stops when a linked module ships none of
  these files.
- The standard library's `LICENSE` and `PATENTS` are committed under
  `hack/stdlib-licenses/<Go version>/`, because not every Go installation
  carries them (Homebrew's does not). The script copies them to
  `dist/licenses/std@<Go version>/` for the version that the `toolchain` line
  of `go.mod` names, and stops when the local Go is another version, when that
  directory is missing, or when either file differs from the one in the local
  Go installation, where that one exists. A toolchain update therefore comes
  with a new directory, copied from the new Go distribution, and the removal
  of the old one.
- Files get mode `0444`, directories `0755`, and every timestamp is set to the
  commit time. The script can be run on its own; it writes nothing outside
  `dist/licenses`.
- The image carries that same directory as `/licenses`, so it also holds the
  licences of the modules that only the macOS binaries link.
- `hack/tarball.go`, run with `go run`, packs the directory into
  `wawarden_<version>_licenses.tar.gz`: entries in lexical order, mode `0444`
  or `0755`, owner and group `0` without names, the commit time as every
  modification time, and a gzip header without a name or a time. Its output
  depends only on the files and the Go toolchain, which `go.mod` pins, so the
  archive is reproducible like the binaries. The release attaches it, and
  `SHA256SUMS` lists it.
- The corresponding source of a release is its tagged commit together with the
  module versions that the commit's `go.sum` pins.

## Verifying a release

Replace `<version>` with the release (for example `v1.0.0`), `<digest>` with the
image index digest from the release notes, `<os>` with `linux` or `darwin`
(the macOS binaries, from `v1.1.0` on), and `<arch>` with `amd64` or `arm64`.
Always deploy the image by digest.

The binary and licence-archive checks need no GitHub account: `gh release
download` fetches a release's files without one, and `gh attestation verify`
given `--bundle wawarden_<version>_provenance.sigstore.json`, the release's own
Sigstore bundle, checks a file without GitHub's attestation API. The image
provenance and SBOM checks and `gh release verify` read that API, so they need
an authenticated `gh` (`gh auth login`).

The `cosign` command needs cosign v2.6.3 or newer, any v3 release included; the
release signs with cosign v3.1.3. cosign v2.6.2 and older do not find the
signature, which is stored as a Sigstore bundle, and report `no signatures found`.

### Image provenance

```sh
gh attestation verify oci://ghcr.io/dortort/wawarden@sha256:<digest> \
  --repo dortort/wawarden \
  --cert-identity https://github.com/dortort/wawarden/.github/workflows/release.yml@refs/heads/main
```

This checks that the image was built by `release.yml` running on `main` of this
repository: `--cert-identity` compares the signing certificate's identity with
the whole value, exactly. Do not replace it with `--signer-workflow`: `gh`
matches that flag as a prefix, so `...@refs/heads/main` would also accept a run
on a branch whose name starts with `main`, such as `main-x`. The binary and SBOM
checks below use the same exact identity.

The image is public, so neither `gh` nor `cosign` needs a registry login. Both
use a `ghcr.io` login if your Docker configuration holds one; if a stale one
makes the fetch fail, run `docker logout ghcr.io`.

Use the index digest. Attestations and the signature are attached to the
multi-arch index, not to the per-platform manifests.

### Image signature

```sh
cosign verify ghcr.io/dortort/wawarden@sha256:<digest> \
  --certificate-identity https://github.com/dortort/wawarden/.github/workflows/release.yml@refs/heads/main \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

### Binaries

Download the binaries, `SHA256SUMS` and
`wawarden_<version>_provenance.sigstore.json` from the release, as
[Install](docs/getting-started.md#install) shows, then check the checksums.
`--ignore-missing` skips the files of the release you did not download. On
Linux:

```sh
sha256sum --ignore-missing -c SHA256SUMS
```

On macOS:

```sh
shasum -a 256 --ignore-missing -c SHA256SUMS
```

Then check the provenance of each binary against the release's bundle:

```sh
gh attestation verify wawarden_<version>_<os>_<arch> \
  --repo dortort/wawarden \
  --bundle wawarden_<version>_provenance.sigstore.json \
  --cert-identity https://github.com/dortort/wawarden/.github/workflows/release.yml@refs/heads/main
```

The bundle holds the attestation whose subjects are the files `SHA256SUMS`
lists. Without `--bundle`, the same command fetches the attestation from
GitHub's attestation API instead, which needs `gh auth login`.

The same command, given `wawarden_<version>_licenses.tar.gz`, checks the
provenance of the licence archive of a release from `v0.2.0` on.

`go version -m <binary>` prints the build settings embedded in a binary. A release
binary shows `CGO_ENABLED=0`, `-trimpath=true`, `vcs.revision` equal to the
release commit and `vcs.modified=false`, and no `-tags` setting containing `dev`.
On a host of that operating system and architecture, `wawarden version` prints
the release version and `dev build false`.

### SBOMs

```sh
gh attestation verify wawarden_<version>_<os>_<arch> \
  --repo dortort/wawarden \
  --cert-identity https://github.com/dortort/wawarden/.github/workflows/release.yml@refs/heads/main \
  --predicate-type https://spdx.dev/Document/v2.3
```

`gh attestation verify` checks SLSA provenance unless `--predicate-type` names
another type. This command checks that the release workflow attested an SPDX 2.3
SBOM for that binary. The release ships no bundle of the SBOM attestations, so
this command reads GitHub's attestation API and needs `gh auth login`.

### The release record

```sh
gh release verify <version> --repo dortort/wawarden
gh release verify-asset <version> SHA256SUMS --repo dortort/wawarden
```

These check the attestation GitHub creates when an immutable release is
published. Both need an authenticated `gh`.

### Reproducing a release

You need:

- git;
- the Go toolchain named by the `toolchain` line in the release's `go.mod`,
  installed locally: the script sets `GOTOOLCHAIN=local` and stops if the local Go
  is any other version;
- rootful Docker with the buildx plugin, at the buildx version pinned in the
  release's `.github/workflows/release.yml` (`BUILDX_VERSION`); rootless Docker
  and Podman have not been tested;
- `jq` and `tar`. On macOS, the system `touch`, `shasum` and `sha256sum` are
  enough; GNU coreutils are not needed;
- access to the Go module proxy, or to whatever `GOPROXY` names. Modules are
  not vendored, so the script downloads them, and the go command checks them
  against the release's `go.sum`.

The script builds with its own temporary `docker-container` builder, created from
the BuildKit image pinned in the script, so the default builder of your Docker
installation is not used. Leave `BUILDKIT_IMAGE` unset. Set `BUILDX_VERSION` to the
pinned buildx version, and the script refuses to run with any other.

Fetch exactly the release commit, **without tags**, and build:

```sh
mkdir wawarden-<version> && cd wawarden-<version>
git init -q
git fetch --depth 1 --no-tags https://github.com/dortort/wawarden refs/tags/<version>
git checkout -q --detach FETCH_HEAD
BUILDX_VERSION=<buildx version> VERSION=<version> hack/repro-build.sh
```

`dist/SHA256SUMS` must be identical to the release's `SHA256SUMS`, and the image
index digest the script prints last must equal `<digest>`.

`VERSION=<version> hack/repro-build.sh binaries`, in the same checkout, builds
no image: it needs only git and the Go toolchain, also on macOS, and reproduces
the binaries and the licence archive. Its `dist/SHA256SUMS` must be identical to
the release's.

Three details decide whether the result matches:

- **Tags.** Since Go 1.24, `go build` stamps the main module's version from the
  repository's tags: the tag on the commit, or a pseudo-version based on the most
  recent tag reachable from it. The release checkout fetches no tags, so the
  release build sees none. A full clone, or `git clone --branch <version>`, sees
  tags and would produce different binaries, so the script stops when any tag is
  reachable from the checked-out commit; use the fetch above.
- **A clean tree.** Any modified or untracked file that git does not ignore marks
  the build `vcs.modified=true` and changes the binaries. The script stops on a
  dirty tree. Write nothing into the checkout except under ignored paths such as
  `dist/`.
- **The builder.** A different BuildKit or buildx version can produce a different
  image digest from identical binaries.

If a checksum or the digest differs, check the builder versions, the Go toolchain
and that the checkout is clean and tag-free before reporting a release-integrity
issue. Report it as described in [`SECURITY.md`](SECURITY.md) if the difference
remains.

## Versioning

- `v0.1.0`, `v0.2.0` and `v0.3.0` were development releases. Each raised the
  minor version, replaced the one before it and promised no compatibility.
- `v1.0.0` was the first release under the compatibility promise below. It
  completed sending, scheduled backups with retention, audit retention and
  master key rotation. It also completed the documentation set: the threat
  model, the configuration reference, the hardening guide, the agent guidance,
  the ban-risk notes, the runbook, `SECURITY.md` and this document.
- From `v1.0.0` on, a patch release fixes, a minor release adds, and a major
  release is the only one that removes or changes.

The [changelog](CHANGELOG.md) lists what each release changed.

### Supported versions

Only the latest minor release line receives security fixes, released as patch
versions of that line. Nothing is backported to an older line. Minor releases
within v1 keep the documented interfaces compatible, so upgrading to the latest
minor release is how you receive a fix. The v0.x releases promised no
compatibility, and each replaced the one before it.

| Version | Supported |
|---|---|
| Latest minor line (`vX.Y.*` of the newest release) | Yes |
| Anything older | No |

[`SECURITY.md`](SECURITY.md) says how to report a vulnerability.

### Compatibility promise

From `v1.0.0` on, no release within v1.x removes, renames or changes the
meaning of:

- the routes of the client, admin and health listeners, with their
  parameters, response fields and error codes;
- the MCP tool names and their argument schemas;
- the environment variable names and the values they accept;
- the subcommands, their flags and the exit codes;
- the notification event names and their fields;
- the Prometheus and CloudWatch EMF metric names.

A minor release may add optional parameters, response fields, tools, routes,
variables, flags, events and metrics. A removal or other breaking change needs
`v2.0.0`, announced as a deprecation in the release notes at least one minor
release ahead. A security fix may tighten behaviour in a patch release, with an
advisory.

Outside the promise:

- Behaviour that depends on WhatsApp, the protocol library or the owner's
  phone.
- Downgrades. Schema migrations go forward only: a release may upgrade the
  archive and the device store, and refuses to open an archive written by a
  newer release. Take a backup before upgrading (see
  [the runbook](docs/runbook.md#upgrading)).

#### How the promise is tested

- `TestV1SurfaceMatchesTheGolden` in `internal/archtest` tests the names in
  the promise.
- `TestMCPToolListMatchesTheGoldenFiles` in `internal/api` tests the MCP
  tools' schemas.
- The REST response fields, the error codes, the values the variables accept
  and the event fields are checked in review.

`TestV1SurfaceMatchesTheGolden` derives a sorted list of the v1 surface from
the source of a release build and the committed MCP `tools/list` golden files.
It compares that list line by line with
`internal/archtest/testdata/v1-surface.txt`, which holds:

- every route of the client, admin and health listeners, with its class;
- every MCP tool, with its input-schema property and required names for each
  client class;
- every environment variable that `config` accepts;
- every subcommand, with its flags and their types, and the command run when
  none is named;
- the exit codes;
- the notification event names;
- the Prometheus and CloudWatch EMF metric names.

Any difference fails the test, an addition included. The test also records a
floor for each category's entry count. A category that falls below it fails
even after regeneration, so neither a net removal nor a scan that stops seeing
entries passes by regenerating alone. A rename, or a removal offset by an
addition in the same category, passes once regenerated and must be caught in
review.

A change that adds to the surface regenerates the file after review, and
commits it with the change:

```sh
go test ./internal/archtest -run TestV1SurfaceMatchesTheGolden -update-v1-surface
```

A line that the regenerated file drops or alters is a breaking change.

## Maintainer checklists

### Cutting a release

- [ ] Every command under [Repository settings](#repository-settings) prints the
      expected value.
- [ ] `main` is green: CI, the reproducibility job and `govulncheck`. The workflow
      refuses a commit without a successful `ci.yml` run on `push`; CodeQL and
      Scorecard results are checked by hand.
- [ ] `hack/offline-test.sh` passes on the commit to release: the whole suite,
      release and dev builds, runs in a container without a network.
- [ ] Choose the version under [Versioning](#versioning) and check that the tag
      does not exist.
- [ ] On the commit to release, read the release-state statements:
      `CHANGELOG.md`, [Supported versions](#supported-versions), the Scope
      section of `SECURITY.md` and the "Since" fields of the threat model.
      `CHANGELOG.md` has a section for the version being cut. The "Since"
      fields name it for each control it introduces, and no sentence says
      which release is the latest or what is planned.
- [ ] The build, rebuild and darwin jobs list
      `wawarden_<version>_licenses.tar.gz` in identical `SHA256SUMS`, and it
      holds a directory for the standard library of the `go.mod` toolchain, for
      WaWarden and for every module in `go.mod` that any of the binaries link
      (see [Licences](#licences)).
- [ ] Dispatch the workflow on `main`:
      `gh workflow run release.yml --ref main -f version=<version>`.
- [ ] Before approving the `release` environment, read the run: the commit is the
      intended tip of `main`, the version is right, the build, rebuild and
      darwin jobs reported identical checksums, and the build and rebuild jobs
      an identical index digest.
- [ ] Approve. Watch the publishing job through signing, attestation and release
      creation.
- [ ] The `ghcr.io/dortort/wawarden` package is still public, so that anyone
      can pull and verify it: pulling the new image by digest works without
      `docker login`.
- [ ] From a machine other than the one used for development, run every command
      under [Verifying a release](#verifying-a-release) against the new release,
      including a reproduction.
- [ ] On a Linux host, run the steps under
      [Release binary](docs/getting-started.md#release-binary), and on a Mac
      those under [macOS](docs/getting-started.md#macos): each installs the new
      release, and `wawarden version` prints it.
- [ ] Check that the release is immutable: `gh release view <version> --repo
      dortort/wawarden --json isImmutable`. The workflow's last step fails when it
      is not.
- [ ] If the run fails before the release is published, delete the draft release,
      if one exists, and dispatch again. If the failed run left a tag, the tag
      ruleset prevents deleting it and the workflow refuses an existing tag:
      release the next patch version instead. If the run fails after publishing,
      do not try to repair the release: fix the cause and release the next patch
      version.
- [ ] If the publishing job stops because the pushed digest differs, the registry
      holds an untagged, unsigned index under that digest. Nothing refers to it;
      investigate the difference before dispatching again.
- [ ] The `wawarden` workflow of the
      [`dortort/homebrew-tap`](https://github.com/dortort/homebrew-tap) tap
      publishes the new release's formula within six hours. GitHub disables a
      public repository's scheduled workflows after 60 days without activity,
      and a disabled workflow cannot be dispatched either: when
      `gh workflow list --all --repo dortort/homebrew-tap` shows `wawarden`
      disabled, enable it first with
      `gh workflow enable wawarden.yml --repo dortort/homebrew-tap`. To publish
      the formula sooner, dispatch the workflow: `gh workflow run wawarden.yml
      --repo dortort/homebrew-tap`. The run commits the formula only after its
      checks and install tests pass.
      `gh run list --workflow wawarden.yml --repo dortort/homebrew-tap --limit 1`
      shows the latest run and its result; wait for one still in progress with
      `gh run watch <id> --repo dortort/homebrew-tap --exit-status`. Once a run
      has succeeded, on a machine with the tap installed
      (`brew tap dortort/tap`, or an earlier `brew install dortort/tap/wawarden`),
      run `brew update`: `brew info dortort/tap/wawarden` shows the new version.

### Dependency updates

Dependabot (`.github/dependabot.yml`) opens pull requests every week for:

- Go modules that have tagged releases;
- GitHub Actions, which are pinned by commit SHA;
- the digest of the base image in the `Dockerfile`.

It proposes an upstream release only once it is three days old. **Nothing is
merged automatically.** Each pull request is reviewed and merged by hand.

Dependabot never proposes `modernc.org/libc` on its own. The SQLite driver,
`modernc.org/sqlite`, requires exactly the libc version its own `go.mod`
names, so libc moves only when a driver update raises it. An architecture test
fails when the libc version in `go.mod` differs from the one the driver's
`go.mod` requires.

Dependabot does not update the following, so they are bumped by hand:

| Item | Where it is pinned |
|---|---|
| Go | The `go` and `toolchain` lines of `go.mod`. The workflows set up Go from `go.mod`; every job that runs Go in `ci.yml`, `codeql.yml`, `release.yml`, `govulncheck-daily.yml` and `bump-whatsmeow.yml` then runs `hack/check-go-version.sh`, which stops the job unless `go env GOVERSION` equals the `toolchain` version, and `hack/repro-build.sh` requires exactly that version too. Dependabot proposes no Go release, but a module update it proposes can raise the `go` line when the new version needs a newer Go; review such a pull request as a Go bump. |
| golangci-lint | The `version` input of both `golangci/golangci-lint-action` steps in `.github/workflows/ci.yml`. |
| govulncheck | The `go install golang.org/x/vuln/cmd/govulncheck@<version>` step in `.github/workflows/ci.yml` and in `.github/workflows/govulncheck-daily.yml`. |
| buildx | `BUILDX_VERSION` in the `env` block of `.github/workflows/ci.yml` and of `.github/workflows/release.yml`. |
| BuildKit | The default of `BUILDKIT_IMAGE` (version and digest) in `hack/repro-build.sh`. |
| The Go image of the offline run | `default_image` (version and digest) in `hack/offline-test.sh`. The script stops unless the image's Go equals the `toolchain` version, so bump it with Go. |
| cosign | The `cosign-release` input of the `sigstore/cosign-installer` step in `.github/workflows/release.yml`. Update the cosign version this document names with it. |

For every update:

- [ ] Review the upstream change, not only `go.mod` and `go.sum`. Modules are
      not vendored, so the pull request's own diff shows only versions and
      hashes. For a Dependabot pull request, read the release notes and
      commits it links and the upstream diff between the two versions. For
      `go.mau.fi/whatsmeow`, read the upstream commit log, the diffstat and the
      diff of the watched paths that the bump workflow described below puts
      into the pull request, and pay particular attention to connection
      handling, the device store, sending, media download and upload, pairing,
      and message decryption.
- [ ] Look for new modules, licence changes, `init` functions with side effects,
      new goroutines, new listeners and handlers registered on
      `http.DefaultServeMux`. The architecture and listener-inventory tests catch
      some of these, and a new module fails the architecture tests until it is
      added to one of their two reviewed lists: modules that source files may
      import, and modules that `go.mod` may require only as indirect
      requirements. The review is the main defence.
- [ ] `go mod verify` passes and `go mod tidy -diff` prints nothing. The
      `modules` CI job runs both, fails when anything named `vendor` exists at
      the repository root, and builds with `-mod=readonly`.
- [ ] `govulncheck` is clean.
- [ ] A bump of Go, the base image, buildx or the BuildKit image changes the
      release digests. The reproducibility job must still produce identical
      checksums from four independent builds, and an identical image digest
      from the two Linux builds. For a base image bump, also verify the
      new digest's signature as documented by the distroless project.

#### The whatsmeow bump workflow

`go.mau.fi/whatsmeow` has no tagged releases, so Dependabot cannot propose
updates for it. The `bump-whatsmeow` workflow
(`.github/workflows/bump-whatsmeow.yml`) proposes them instead, every Monday at
05:41 UTC and whenever a maintainer dispatches it from `main` with an upstream
commit (7 to 40 lowercase hex characters) or `latest`, the default. It never
merges, tags or releases. It runs two jobs:

1. The first job can only read the repository. It asks the Go module proxy for
   the target's pseudo-version, and ends without a change when `go.mod` already
   pins it. Otherwise it runs `go get`, `go mod tidy` and `go mod verify`, stops
   unless exactly `go.mod` and `go.sum` changed, writes the pull request's
   description, which states no result of the build or the tests, uploads it
   with `go.mod`, `go.sum` and the watched-path diff as the artifact
   `whatsmeow-bump`, builds the release and `dev` tag sets, and runs
   `hack/offline-test.sh`. A target that resolves to a tagged version or to
   another commit, or that needs a newer Go than the `toolchain` line of
   `go.mod` names (`GOTOOLCHAIN` is `local`), stops it.
2. The second job runs only when the first one passed, and alone holds
   `contents: write` and `pull-requests: write`. It runs no Go code. It puts a
   line in front of the artifact's description saying that the first job passed
   its checks, builds and tests, commits the artifact's `go.mod` and `go.sum`
   onto the commit the run started from, pushes the commit to the branch
   `bump/whatsmeow`, and opens the pull request from that branch, or updates the
   title and description of the open one from that branch of this repository; a
   pull request from a fork's branch of the same name is never touched. The
   branch belongs to the workflow: a run replaces it, refuses to when it holds a
   commit that the workflow did not make, and leaves it alone when it already
   pins the same version, so that an approved CI run stays valid. A closed pull
   request is not remembered: the next run opens a new one for the newest
   commit, even one that was declined. An architecture test
   (`internal/archtest/workflows_test.go`) holds the workflow to this split:
   main only, no workflow-wide permission, no Go and no `hack/` script in the
   job that can write, credentials never persisted by a checkout, the Go version
   check right after setup-go and before any Go command, no step that says
   checks passed before the tests run, and the lookup by this repository's owner
   and branch, which skips pull requests from forks however many there are.

The description carries the old and new pseudo-versions, the upstream compare
link, whether the new commit descends from the pinned one (and if not, the
commits it drops), the upstream commit log between them, a diffstat between the
two module versions' sources as the module proxy serves them, the `go.mod`
diff, and the full diff between those sources of the watched paths: `socket/`,
`store/`, `send.go`, `download.go`, `upload.go`, `message.go`, `pair*.go`,
`handshake.go`, `msgsecret.go` and `appstate*`. Upstream text is fenced so that
it cannot end its code block. When the description would exceed GitHub's limit,
the diff is cut at a line with a note, and the complete diff is
`watched-paths.diff` in the run's `whatsmeow-bump` artifact.

When the first job fails, nothing is pushed. If it failed at the build or the
tests, its `whatsmeow-bump` artifact still holds the description, which claims
no result, and the diff. A bump that needs a change of this repository, such
as a new module for the architecture tests' reviewed lists, is made by hand on a
branch with the same commands.

Opening the pull request needs the repository setting **Allow GitHub Actions to
create and approve pull requests** (Settings, Actions, General, Workflow
permissions), which is off by default for a repository in a personal account.
`gh api repos/dortort/wawarden/actions/permissions/workflow --jq .can_approve_pull_request_reviews`
prints `true` when it is on. While it is off, GitHub refuses the workflow
token's request to open a pull request: the run fails after pushing the branch,
and its summary links the page from which a maintainer opens the pull request
by hand, with `body.md` from the artifact as its description. The setting also
lets a workflow token approve pull requests; no other workflow holds
`pull-requests: write`, and no ruleset requires an approval.

CI does not start on its own. GitHub starts no workflow run for an event that
the workflow token causes, apart from `workflow_dispatch` and
`repository_dispatch`; when a workflow opens or updates a pull request with that
token, the `pull_request` runs are created in an approval-required state
([Triggering a workflow from a workflow](https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/trigger-a-workflow#triggering-a-workflow-from-a-workflow)).
A maintainer with write access starts them with **Approve workflows to run** in
the pull request's merge box, after the workflow opens the pull request and
again after each run that pushes the branch. This needs no personal access
token, GitHub App or stored secret, and no workflow gains a permission. A pull
request that a maintainer opened by hand runs CI on its own, but later pushes
by the workflow wait for approval again.

For a bump pull request, in addition to the list above:

- [ ] Approve the CI runs as described above, and merge only after `ci` passed
      on the pull request's head commit. The `main` ruleset requires no status
      check, so GitHub does not enforce this.
- [ ] Read the whole watched-path diff, from the artifact when the description
      says it was cut, and the commit log. Treat a target that does not descend
      from the pinned commit as a downgrade or rewritten history, and find out
      why before merging.

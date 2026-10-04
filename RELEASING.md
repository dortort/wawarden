# Releasing and verifying WaWarden

This document describes the repository settings a release depends on, how
releases are produced, what each release contains, how to verify one, and the
checklists the maintainer follows.

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
- **`main` cannot be deleted or force-pushed.**
  `gh api repos/dortort/wawarden/rules/branches/main --jq '[.[].type]'` prints
  `["deletion","non_fast_forward"]`. The rules come from the branch ruleset:
  `gh api repos/dortort/wawarden/rulesets --jq '.[] | select(.target == "branch") | .id'`
  prints its id, and
  `gh api repos/dortort/wawarden/rulesets/<id> --jq '{enforcement, target, include: .conditions.ref_name.include, rules: [.rules[].type], bypass_actors}'`
  prints
  `{"bypass_actors":[],"enforcement":"active","include":["~DEFAULT_BRANCH"],"rules":["deletion","non_fast_forward"],"target":"branch"}`:
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
  prints `"read"`.

Neither ruleset has bypass actors, so they apply to administrators too.

The release workflow's first job reads the `release` environment through the API
and stops unless it exists, has a required reviewer, does not let administrators
bypass its protection rules, and accepts deployments only from the branch `main`.
The workflow token cannot read the other settings: check them with the commands
above after any change to the repository settings.

## How a release is produced

- Releases are cut **only from `main`**, by the manually dispatched workflow
  `.github/workflows/release.yml`. Nobody pushes release tags by hand.
- The workflow has four jobs, which run in this order: `guard` (*Check version,
  tag, environment and CI*), `build` (*Build*), `rebuild` (*Rebuild and
  compare*) and `release` (*Publish*), the publishing job. Every job runs only
  when the workflow was dispatched on `refs/heads/main`.
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
  `security-events: write` (Scorecard and CodeQL upload code-scanning results) and
  `issues: write` (the daily `govulncheck` job manages one issue).
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

- Go binaries for `linux/amd64` and `linux/arm64`, built with `CGO_ENABLED=0`,
  `-trimpath`, `-buildvcs=true` and an empty build ID, from the vendored modules
  once the module has dependencies, with the toolchain named by the `toolchain`
  line in `go.mod` (`GOTOOLCHAIN=local`): the script stops unless
  `go env GOVERSION` equals that line. In `ci.yml`, `codeql.yml` and
  `release.yml`, every job that runs Go also runs `hack/check-go-version.sh`
  right after setting Go up, which makes the same comparison in the job's own
  environment. The script accepts any `VERSION` that is a valid image tag (CI
  builds with `VERSION=ci`); the version string is injected at link time into
  `internal/buildinfo.Version`, and `wawarden version` prints it.
- Before packaging, the script checks each binary: its embedded build settings
  must show no `dev` tag, `vcs.revision` equal to the commit, `vcs.modified=false`,
  `CGO_ENABLED=0` and the right architecture; the binary must not contain the
  marker string that only dev builds carry; and it must contain the version
  variable that the link step sets (`go tool nm` lists
  `internal/buildinfo.Version`). On a Linux host, and only in its `binaries`
  and `oci` modes, the script also runs the binary built for the host's
  architecture, with an empty environment (`env -i`): `wawarden version` must
  print exactly the requested version, the commit and `dev build false`. In the
  release workflow, the `build` job runs the `amd64` binary on an `amd64` runner
  and the `rebuild` job runs the `arm64` binary on an `arm64` runner, so both
  release binaries are run before anything is published. The publishing job
  runs the script in `push` mode, which runs none of the binaries it builds:
  that job holds the write permissions, so it runs no code built from the
  repository. It proves instead that its binaries are byte-identical to the
  ones the two other jobs ran: the image it pushes must have the `build` job's
  index digest, and its `SHA256SUMS` must equal the `build` job's, which the
  `rebuild` job reproduced.
- `SOURCE_DATE_EPOCH` is the commit time. File times inside the image are set
  to it.
- A multi-arch OCI image built by BuildKit from a digest-pinned
  `gcr.io/distroless/static-debian13:nonroot` base. The image adds the binary,
  `/wawarden`, and an empty `/data` directory owned by the runtime user with mode
  `0700` to the base, runs as `65532:65532` and has no shell. The image's runtime
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

### What a release contains

| Artifact | Where |
|---|---|
| Multi-arch image (`linux/amd64`, `linux/arm64`) | `ghcr.io/dortort/wawarden@sha256:<digest>`, also tagged `<version>`; the release notes state the index digest |
| Binaries `wawarden_<version>_linux_amd64` and `wawarden_<version>_linux_arm64` | Release assets |
| `SHA256SUMS` for the binaries | Release asset |
| SPDX 2.3 SBOMs for the binaries, `wawarden_<version>_linux_<arch>.spdx.json` | Release assets, and attested to each binary in GitHub's attestation store |
| Build provenance attestations (SLSA, via `actions/attest-build-provenance`) for the image index and for each binary | GitHub's attestation store; the image attestation is also pushed to the registry, and the binaries' Sigstore bundle is the release asset `wawarden_<version>_provenance.sigstore.json` |
| Keyless cosign signature on the image index, made with cosign v3.1.3 | Registry, next to the image |

The SBOMs list the Go modules and the Go standard library compiled into each
binary. No SBOM covers the image as a whole, so the packages of the distroless
base image are not listed; an image-level SBOM is planned and not produced yet.

Signing and attesting use GitHub's OIDC identity for the release workflow on
`main`. No long-lived signing key exists.

## Verifying a release

Replace `<version>` with the release (for example `v0.1.0`), `<digest>` with the
image index digest from the release notes, and `<arch>` with `amd64` or `arm64`.
Always deploy the image by digest.

Every `gh` command below needs an authenticated `gh` (`gh auth login`). The
`cosign` command needs cosign v3.1.3, the release used to sign, or a newer one.

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

`gh` reads the registry with your existing registry login; run
`docker login ghcr.io` first if verification cannot fetch the image.

Use the index digest. Attestations and the signature are attached to the
multi-arch index, not to the per-platform manifests.

### Image signature

```sh
cosign verify ghcr.io/dortort/wawarden@sha256:<digest> \
  --certificate-identity https://github.com/dortort/wawarden/.github/workflows/release.yml@refs/heads/main \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

### Binaries

Download the binaries and `SHA256SUMS` from the release, then check the
checksums. On Linux:

```sh
sha256sum --ignore-missing -c SHA256SUMS
```

On macOS:

```sh
shasum -a 256 --ignore-missing -c SHA256SUMS
```

Then check the provenance of each binary:

```sh
gh attestation verify wawarden_<version>_linux_<arch> \
  --repo dortort/wawarden \
  --cert-identity https://github.com/dortort/wawarden/.github/workflows/release.yml@refs/heads/main
```

`go version -m <binary>` prints the build settings embedded in a binary. A release
binary shows `CGO_ENABLED=0`, `-trimpath=true`, `vcs.revision` equal to the
release commit and `vcs.modified=false`, and no `-tags` setting containing `dev`.
On a Linux host of the same architecture, `wawarden version` prints the release
version and `dev build false`.

### SBOMs

```sh
gh attestation verify wawarden_<version>_linux_<arch> \
  --repo dortort/wawarden \
  --cert-identity https://github.com/dortort/wawarden/.github/workflows/release.yml@refs/heads/main \
  --predicate-type https://spdx.dev/Document/v2.3
```

`gh attestation verify` checks SLSA provenance unless `--predicate-type` names
another type. This command checks that the release workflow attested an SPDX 2.3
SBOM for that binary.

### The release record

```sh
gh release verify <version> --repo dortort/wawarden
gh release verify-asset <version> SHA256SUMS --repo dortort/wawarden
```

These check the attestation GitHub creates when an immutable release is
published.

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
  enough; GNU coreutils are not needed.

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

- `v0.x` during the milestones. Each milestone release raises the minor version;
  fixes to the latest milestone raise the patch version.
- `v1.0.0` is released when milestone M3 is complete and the v1.0 documentation
  set is complete: the threat model, configuration reference, hardening guide,
  agent guidance, ban-risk notes, `SECURITY.md` and this document.
- Only the latest minor release line is supported (see `SECURITY.md`).

## Maintainer checklists

### Cutting a release

- [ ] Every command under [Repository settings](#repository-settings) prints the
      expected value.
- [ ] `main` is green: CI, the reproducibility job and `govulncheck`. The workflow
      refuses a commit without a successful `ci.yml` run on `push`; CodeQL and
      Scorecard results are checked by hand.
- [ ] Choose the version under [Versioning](#versioning) and check that the tag
      does not exist.
- [ ] Dispatch the workflow on `main`:
      `gh workflow run release.yml --ref main -f version=<version>`.
- [ ] Before approving the `release` environment, read the run: the commit is the
      intended tip of `main`, the version is right, and the build and rebuild jobs
      reported identical checksums and an identical index digest.
- [ ] Approve. Watch the publishing job through signing, attestation and release
      creation.
- [ ] After the first release, set the `ghcr.io/dortort/wawarden` package to
      public in the package settings, so that anyone can pull and verify it.
- [ ] From a machine other than the one used for development, run every command
      under [Verifying a release](#verifying-a-release) against the new release,
      including a reproduction.
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

### Dependency updates

Dependabot (`.github/dependabot.yml`) opens pull requests every week for:

- Go modules that have tagged releases;
- GitHub Actions, which are pinned by commit SHA;
- the digest of the base image in the `Dockerfile`.

It proposes an upstream release only once it is three days old. **Nothing is
merged automatically.** Each pull request is reviewed and merged by hand.

Dependabot does not update the following, so they are bumped by hand:

| Item | Where it is pinned |
|---|---|
| Go | The `go` and `toolchain` lines of `go.mod`. The workflows set up Go from `go.mod`; every job that runs Go in `ci.yml`, `codeql.yml` and `release.yml` then runs `hack/check-go-version.sh`, which stops the job unless `go env GOVERSION` equals the `toolchain` version, and `hack/repro-build.sh` requires exactly that version too. Dependabot proposes no Go release, but a module update it proposes can raise the `go` line when the new version needs a newer Go; review such a pull request as a Go bump. |
| golangci-lint | The `version` input of both `golangci/golangci-lint-action` steps in `.github/workflows/ci.yml`. |
| govulncheck | The `go install golang.org/x/vuln/cmd/govulncheck@<version>` step in `.github/workflows/ci.yml` and in `.github/workflows/govulncheck-daily.yml`. |
| buildx | `BUILDX_VERSION` in the `env` block of `.github/workflows/ci.yml` and of `.github/workflows/release.yml`. |
| BuildKit | The default of `BUILDKIT_IMAGE` (version and digest) in `hack/repro-build.sh`. |
| cosign | The `cosign-release` input of the `sigstore/cosign-installer` step in `.github/workflows/release.yml`. Update the cosign version this document names with it. |

For every update:

- [ ] Once the module has dependencies, they are vendored: review the diff under
      `vendor/`, not only `go.mod` and `go.sum`. The `modules` CI job fails when
      `vendor/` does not match `go.mod`. For `go.mau.fi/whatsmeow`, read the
      upstream commit log between the two versions and pay particular attention
      to connection handling, the device store, sending, media download and
      upload, pairing, and message decryption.
- [ ] Look for new modules, licence changes, `init` functions with side effects,
      new goroutines, new listeners and handlers registered on
      `http.DefaultServeMux`. The architecture and listener-inventory tests catch
      some of these, and a new module fails the architecture tests until it is
      added to their allow-list; the review is the main defence.
- [ ] `go mod verify` passes and `go mod vendor` leaves no diff.
- [ ] `govulncheck` is clean.
- [ ] A bump of Go, the base image, buildx or the BuildKit image changes the
      release digests. The reproducibility job must still produce identical
      results from two independent builds. For a base image bump, also verify the
      new digest's signature as documented by the distroless project.

Planned for M1, when `go.mau.fi/whatsmeow` is added and vendored, and not present
yet:

- `whatsmeow` has no tagged releases, so Dependabot cannot propose updates for it.
  A scheduled workflow of this repository, which can also be dispatched by hand,
  will open a pull request pinning an exact commit as a pseudo-version, with the
  upstream commit log and a diffstat in its description. It never merges, tags or
  releases.
- A CI check that flags every change under the vendored `whatsmeow` paths for
  connections, the device store and media transfer (`socket/`, `store/`,
  `send.go`, `download.go`, `upload.go`).

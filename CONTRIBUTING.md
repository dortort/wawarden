# Contributing to WaWarden

This guide covers building, testing and checking a change the way CI does,
and how to write commits. Report a vulnerability privately, as
[`SECURITY.md`](SECURITY.md) describes, never in a public issue or pull
request.

## Contents

- [Toolchain](#toolchain)
- [Build](#build)
- [Test](#test)
- [Format and vet](#format-and-vet)
- [Lint](#lint)
- [Modules](#modules)
- [Vulnerability check](#vulnerability-check)
- [Fuzzing](#fuzzing)
- [Architecture tests](#architecture-tests)
- [Golden files](#golden-files)
- [Reproducible build](#reproducible-build)
- [Documentation](#documentation)
- [Documentation site](#documentation-site)
- [Commits and history](#commits-and-history)
- [What CI runs](#what-ci-runs)

## Toolchain

The build uses the Go version on the `toolchain` line of `go.mod`. With its
default settings, the `go` command (1.21 or later) downloads that version when
the installed one is older. Some distributions' Go packages, such as Fedora's
and Alpine's, set `GOTOOLCHAIN=local` in their `go.env`, which turns the
download off; `go env -w GOTOOLCHAIN=auto` turns it back on. CI checks that
`go env GOVERSION` matches the `toolchain` line:

```sh
hack/check-go-version.sh
```

Other tools, at the versions CI uses:

| Tool | Version | For |
|---|---|---|
| golangci-lint | `v2.14.0` | [Lint](#lint) |
| govulncheck | `v1.8.0` | [Vulnerability check](#vulnerability-check) |
| Docker | any recent | `hack/offline-test.sh` and the [reproducible build](#reproducible-build) |

## Build

```sh
go build ./...
go build -tags dev ./...
go build -trimpath -o wawarden ./cmd/wawarden
```

The `dev` build tag makes a development build, which adds the fake engine
(see [the fake engine](docs/internals.md#fake-engine)). Release builds
never carry it.

## Test

```sh
go test -race -count=1 ./...
go test -race -count=1 -tags dev ./...
```

The second run adds an end-to-end test that runs `serve` with the fake engine
and pairs it through the admin CLI.

No test contacts WhatsApp:

- The protocol adapter is tested with hand-built events, protobuf fixtures and
  injected transports.
- Its dialer, and every transport it builds, refuses every connection inside a
  test binary.

`hack/offline-test.sh` proves it. It fetches the modules that `go.sum` pins,
then runs both suites in a container with no network. It needs Docker.

CI also runs two longer checks:

```sh
go test -count=1 -run 'Property|AnyGrant' ./internal/store/scoped/ -rapid.checks=1000 -rapid.shrinktime=10s
go test -count=1 -run '^TestMigratingALargeArchive$' ./internal/store/ingest/
```

The first runs the scoped store's property checks 1,000 times. The second
migrates an archive of 500,000 messages. A property failure writes its case
under `internal/store/scoped/testdata/rapid`.

## Format and vet

```sh
gofmt -l $(git ls-files '*.go')
go vet ./...
go vet -tags dev ./...
```

`gofmt -l` must list nothing.

## Lint

CI runs golangci-lint `v2.14.0` with the repository's configuration, once
without and once with the `dev` tag:

```sh
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
golangci-lint run --config=.golangci.yml
golangci-lint run --config=.golangci.yml --build-tags=dev
```

[`.golangci.yml`](.golangci.yml) enables `govet`, `staticcheck`, `errcheck`,
`ineffassign`, `unused`, `gosec`, `bodyclose`, `noctx`, `depguard` and
`forbidigo`.

- **`depguard`** keeps packages behind their fences. For example,
  `internal/api` reaches the archive only through `internal/store/scoped`, and
  only `internal/store/internal/db` imports the SQLite driver.
- **`forbidigo`** bans `http.ListenAndServe`, `http.DefaultServeMux`,
  `http.Get` and similar calls, and opening sockets outside
  `internal/listeners`.

A `//nolint` directive must name its linters, and may not silence `depguard`
or `forbidigo`. The architecture tests enforce this.

## Modules

Modules are pinned by `go.sum` and never vendored. CI refuses a `vendor`
directory and checks:

```sh
go mod tidy -diff
go mod verify
GOFLAGS=-mod=readonly go build ./...
```

`go mod tidy -diff` must print nothing. A new dependency is a security
decision: the architecture tests hold the list of modules that code may
import, and a module missing from it fails `TestThirdPartyAllowList` and the
`third-party-imports` rule. [Dependency updates](RELEASING.md#dependency-updates)
describes how updates are reviewed.

## Vulnerability check

```sh
go install golang.org/x/vuln/cmd/govulncheck@v1.8.0
govulncheck ./...
```

CI runs it on every change, and a separate workflow runs it daily.

## Fuzzing

The fuzz targets guard parsers and normalisers: the chat identifier
normaliser, the sanitisers, the request decoders, cursors and message
references, tokens, history inflation and crafted protocol messages. List
them:

```sh
go test -count=1 -list '^Fuzz' ./...
```

CI runs each target for 60 seconds. Run one the same way:

```sh
go test -run='^$' -fuzz='^FuzzNormalize$' -fuzztime=60s -fuzzminimizetime=10s ./internal/policy/
```

A failure writes its input under the package's `testdata/fuzz/`. Fix the bug
and commit that file, so the case runs in every later `go test`.

## Architecture tests

`internal/archtest` parses every Go file of the module and checks it against
a list of rules. It also builds fixture packages that must fail to compile
with a specific error. The tests run in `go test ./...` like any other.
[Secure by construction](docs/threat-model.md#secure-by-construction) explains
the invariant behind each mechanism.

What the rules enforce, in groups:

- **Fences and confinement.** `internal/api` reaches the archive only through
  the scoped store. Only `internal/store/internal/db` holds a database handle.
  Only `internal/engine/wa` imports the protocol library. Only
  `internal/backup` imports age, for encryption only. Only the listed modules
  may be imported, and some packages may use the standard library only.
- **Network and goroutines.** Only `internal/listeners` opens sockets and
  builds HTTP servers. Every goroutine starts through `safego.Go`. Tests bind
  no wildcard address. Protocol client calls are limited to a reviewed list.
- **Routes and grants.** Routes and MCP tools are registered only through the
  helpers that take a policy class. Every scoped read and every send takes a
  grant first. Only the decision functions mint grants.
- **SQL.** Queries are constants, chat sets go through the one `IN` builder,
  and nothing runs `ANALYZE`. Only the retention code deletes audit rows or
  moves the audit anchor.
- **Secrets and files.** No credential literals, constant-time comparisons
  only, and only `internal/keys` touches `keys/`. Only `internal/backup` names
  `backups/`. One log writer carries every line.
- **Development code.** The fake engine compiles only with the `dev` tag, and
  nothing a release build compiles imports it.
- **Lint and workflows.** `//nolint` rules as above. Workflow `run` scripts
  hold no `${{ }}` expressions, and workflows build with read-only modules.
- **The v1 surface.** See [Golden files](#golden-files).

### Fixing a failure

A rule failure appears as a subtest, `TestArchitecture/<rule name>`, with the
file, the line and what the rule expects. Run one rule:

```sh
go test ./internal/archtest -run 'TestArchitecture/goroutines'
```

To fix it:

1. Move the code to the package the rule allows, or use the sanctioned
   helper, such as `safego.Go` for a goroutine or the scoped store for a read.
2. If the change is deliberate, such as a new dependency or a new protocol
   client call, update the rule's list in `internal/archtest` in the same
   change. Say why in the commit message, since reviewers treat it as a
   security change.
3. Never silence a rule with `//nolint`; it is refused for `depguard` and
   `forbidigo`, and archtest is not a linter.

Each rule carries its own test cases (`TestRuleCases`). A change to a rule
adds a case that it must catch.

## Golden files

Some tests compare output with committed files. Regenerate one only after
reviewing the change, and commit the regenerated file with it:

| Test | Regenerate with | Holds |
|---|---|---|
| `TestV1SurfaceMatchesTheGolden` | `go test ./internal/archtest -run TestV1SurfaceMatchesTheGolden -update-v1-surface` | `internal/archtest/testdata/v1-surface.txt`: routes, MCP tools, variables, subcommands and flags, exit codes, events and metric names |
| `TestMCPToolListMatchesTheGoldenFiles` | `go test ./internal/api -run TestMCPToolListMatchesTheGoldenFiles -update-mcp-tools` | `internal/api/testdata/mcp`: the MCP `tools/list` answers |
| scoped store query plans | `go test ./internal/store/scoped -update-plans` | `internal/store/scoped/testdata/plans`: SQLite query plans |

The v1 surface file is the [compatibility promise](RELEASING.md#versioning)
in testable form. A line that regeneration adds is a new feature. A line it
drops or changes is a breaking change, which needs `v2.0.0`. Each category
also has a floor, so a net removal fails even after regeneration.

## Reproducible build

```sh
VERSION=dev hack/repro-build.sh binaries
```

It needs a clean working tree and a commit fetched without tags, so that
`go build` stamps the same version information as a release. CI builds the
binaries and the OCI image (`oci` mode, which also needs Docker) on two
runners and compares their checksums and image digests. [Reproducing a release](RELEASING.md#reproducing-a-release)
describes the full procedure.

## Documentation

The documentation lives in [`docs/`](docs/), with
[`README.md`](README.md), [`RELEASING.md`](RELEASING.md),
[`SECURITY.md`](SECURITY.md) and [`CHANGELOG.md`](CHANGELOG.md) at the root.
A change that alters behaviour updates the document that describes it, in the
same commit.

- Use British spelling: "licence", "behaviour", "normaliser".
- Write short sentences in the present tense. Describe what is, not what is
  planned.
- Keep version history in [`CHANGELOG.md`](CHANGELOG.md), not in the
  reference documents.
- Use the terms of the [glossary](docs/glossary.md).
- Make anything an operator may link to a heading.
- Link between documents with relative links, so they work on GitHub and on
  the site.

## Documentation site

The site at <https://wawarden.dortort.dev/> is built by MkDocs with the
Material theme. Its sources are [`mkdocs.yml`](mkdocs.yml), `docs/` and the
root Markdown files. The Python packages are pinned with hashes in
[`website/requirements.txt`](website/requirements.txt).

To preview it, from the repository root, with Python 3.12 or later:

```sh
python3 -m venv .venv
.venv/bin/pip install --no-deps --require-hashes -r website/requirements.txt
.venv/bin/mkdocs serve --strict
```

Then open <http://127.0.0.1:8000/wawarden/>. `.venv/` and the build output
`site/` are ignored by Git.

CI runs the strict build on every pull request
([`.github/workflows/pages.yml`](.github/workflows/pages.yml)), and publishes
the site on every push to `main`. Any warning fails it, such as a broken link
or a missing anchor. Run the same build before pushing a documentation change:

```sh
.venv/bin/mkdocs build --strict
```

Write links as GitHub resolves them:

- Between `docs/` files, use relative `.md` paths, such as
  `runbook.md#disconnect-reasons`.
- To the root files, use paths such as `../README.md`.

[`website/hooks.py`](website/hooks.py) maps the root files to `project/`
pages on the site, and points links to other repository files at GitHub. To
update the pins, edit [`website/requirements.in`](website/requirements.in) and
run the `pip-compile` command at its top.

## Commits and history

Commit messages follow `type(scope): summary`, in the imperative and in lower
case, as `git log --oneline` shows:

```text
fix(api): give the send route a body cap that fits an escaped text
test(archtest): refuse protocol client calls outside the reviewed list
docs(runbook): say what to do on audit_prune_refused and alarm on it
```

| Type | For |
|---|---|
| `feat` | A new feature or behaviour |
| `fix` | A bug fix |
| `test` | Tests only |
| `docs` | Documentation only |
| `refactor` | A change that keeps behaviour |

The scope names the package or document, such as `api`, `store`, `engine`,
`archtest`, `runbook` or `threat-model`. Leave it out when a change spans
many. Keep each commit to one change, with its tests and documentation.

`main` requires a linear history: a merge commit cannot be pushed to it.
Rebase onto `main` before pushing (`git pull --rebase`). See
[Repository settings](RELEASING.md#repository-settings).

## What CI runs

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs on every pull
request and every push to `main`:

| Job | Runs |
|---|---|
| Build and test | Go version check, `gofmt`, build, vet and `go test -race`, each without and with `-tags dev` |
| Property checks and a large archive | The two longer tests under [Test](#test) |
| Lint | golangci-lint `v2.14.0`, without and with `--build-tags=dev` |
| Modules | No `vendor`, `go mod tidy -diff`, `go mod verify`, read-only builds |
| govulncheck | `govulncheck ./...` |
| Fuzz | Each fuzz target for 60 seconds |
| Reproducible build | Two builds, on amd64 and arm64 runners, compared bit for bit |

CodeQL and OpenSSF Scorecard run in their own workflows, and so does the
[documentation site](#documentation-site)'s strict build.

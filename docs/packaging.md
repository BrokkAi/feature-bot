# Native and npm distribution

Feature Bot uses the same distribution layout and GitHub Actions sequence as
Brokk Bug Bot. The executable is `bfb`; every package and archive includes the
Apache license, the original Bug Bot attribution, and reviewed dependency notices.
The initial source baseline is Bug Bot revision
[`e7f973b1eddf28d36ec30fdadff382a9781a1784`](https://github.com/BrokkAi/bug-bot/commit/e7f973b1eddf28d36ec30fdadff382a9781a1784).

## Packages and archives

The root npm package, `@brokkai/feature-bot`, launches one exact-version optional
dependency for the current system:

- `@brokkai/feature-bot-linux-x64`
- `@brokkai/feature-bot-linux-arm64`
- `@brokkai/feature-bot-darwin-x64`
- `@brokkai/feature-bot-darwin-arm64`

Each native release contains four archives named
`brokk-feature-bot-vVERSION-SYSTEM-ARCH.tar.gz` (Go architecture names `amd64` and
`arm64`), `checksums.txt`, and `release.json`. Native `BUILD.json` metadata records
the exact commit, version tag, and platform. npm packages are built only after
all archive checksums, contents, legal notices, and commit metadata are verified.

## Local validation

Commit preparation changes before packaging. The package command requires a
clean checkout and rejects a checkout that changes while it builds. These
commands build and install local artifacts without contacting GitHub or npm for
publication:

```sh
go test -race ./...
go vet ./...
python3 scripts/licenses.py
sh -n install.sh
python3 -m unittest discover -s scripts -p '*_test.py'
node --test --test-isolation=none npm/bfb.test.cjs
python3 scripts/package_release.py v0.1.0-rc.1 --output dist/native
python3 scripts/package_installers.py v0.1.0-rc.1 dist/native dist/packages
python3 scripts/smoke_installers.py --tag v0.1.0-rc.1 --assets dist/native --packages dist/packages
```

Use fresh output directories for another build. The npm smoke check installs the
local launcher and platform package with `--offline --ignore-scripts`, then runs
`bfb --help`. Packaging and smoke checks isolate npm configuration and cache from
the developer's account settings. Installer tests use fake downloads and verify
that checksum failure preserves the existing installation.

## GitHub Actions and account setup

`ci.yml` checks Linux and macOS, including race tests, vet, licenses, the CLI,
installer tests, launcher tests, and an offline install. Actions are pinned to
commit hashes.

Pushing an explicitly requested `v*` release tag invokes `publish-packages.yml`.
It first calls `release.yml`, which runs CI, builds verified native assets, and
publishes a GitHub release. Release-candidate tags become GitHub prereleases;
stable tags become the latest release. The package job then downloads that exact
published release, validates and smoke-tests all five npm packages, and uploads
the validated tarballs as a workflow artifact before publication. Prereleases
use the npm `next` dist-tag; stable versions use `latest`.

The repository is `BrokkAi/feature-bot`. Configure its `packages-publish`
environment and any desired release approvals. Configure npm trusted publishing
for **each of the five packages**, with owner `BrokkAi`, repository `feature-bot`,
workflow `publish-packages.yml`, and environment `packages-publish`. The workflow
grants `id-token: write` for npm OIDC. An optional `NPM_TOKEN` environment secret
supports bootstrap publication when an existing trusted publisher is unavailable.
Package account setup and publication are separate external actions; a local
build does not perform either.

For an existing published release, dispatch `publish-packages.yml` **from that
exact tag**, supplying the same `tag` input. The default `publish=false` validates
and saves packages for review. Set `publish=true` only when publication has been
requested. The native workflow itself is reusable and cannot be dispatched
directly.

`scripts/package_registry.py check dist/packages` checks version availability
and detects conflicting bytes before any upload. It does not prove authorization
to publish. `publish` submits the four platform packages before the launcher and
skips identical existing versions. `verify` later checks that every public
package matches the staged bytes; registry propagation may delay visibility.
Never replace a conflicting published version: prepare a new version instead.

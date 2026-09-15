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

See [RELEASING.md](../RELEASING.md) for the complete destination list, required
npm OIDC configuration, branch-safe preflight dispatch, authorization gates and
partial publication recovery. `publish-packages.yml` builds all native and npm
assets before publication and finalizes GitHub only after all npm packages verify.
Use `scripts/release_preflight.py` for release lifecycle checks; the lower-level
`scripts/package_registry.py` remains available for exact staged-byte checks.

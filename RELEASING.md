# Releasing feature-bot

## Destinations and dependency order

One version tag releases these destinations, in this order:

1. The Go module `github.com/BrokkAi/feature-bot`, through the immutable Git tag
   (there is no separate Go registry upload).
2. A draft GitHub release in `BrokkAi/feature-bot` stages four native archives,
   `brokk-feature-bot-vVERSION-{linux,darwin}-{amd64,arm64}.tar.gz`, plus
   `checksums.txt` and `release.json`. Each archive includes `bfb`, `BUILD.json`,
   README and all reviewed license notices.
3. npm platform packages `@brokkai/feature-bot-linux-x64`,
   `@brokkai/feature-bot-linux-arm64`, `@brokkai/feature-bot-darwin-x64`, and
   `@brokkai/feature-bot-darwin-arm64`.
4. npm launcher `@brokkai/feature-bot`, with exact-version optional dependencies
   on all four platform packages.
5. Finalize the GitHub release only after every npm package is verified.

There are no crates.io, PyPI, Maven, container, documentation deployment, update
feed or signing/notarization destinations. npm may generate its standard OIDC
provenance as part of package publication. Stable versions use npm `latest` and
GitHub latest; prereleases use npm `next` and GitHub prerelease.

## Preparation without publication

Use a topic branch and PR into `master`. Branch pushes run **CI** only.
No tag is required for preflight. After merging, fetch and check out the exact
merged commit detached. Set `RELEASE_COMMIT` to that full SHA, `RELEASE_TARGET`
to the included baseline, and `RELEASE_TAG` to a new version such as `v0.1.2`.

Run `python3 scripts/release_preflight.py build` for tests, license validation,
all four native builds, all five npm packages, and a real offline installation.
Committed clean inputs are required. Output lives in
`dist/preflight/COMMIT/TAG`; completed output is verified before reuse. Inspect
incomplete output before removing only that failed build's directory and retrying.

Dispatch the existing workflow with publication disabled:

```sh
gh workflow run publish-packages.yml --repo github.com/BrokkAi/feature-bot --ref master -f tag=v0.1.2 -f publish=false
```

Confirm the dispatched SHA matches the release commit. **Publish packages** calls
**Release**, which calls Linux/macOS **CI**, then builds all deliverables in job
`packages`, environment `packages-publish`. It checks all destination versions,
exchanges the job's GitHub OIDC token for a scoped publishing token for **each**
npm package, and checks expiry. It exercises the same job's GitHub token by
creating, updating and deleting a uniquely named disposable draft without a Git
tag or asset upload. It saves staged builds and non-secret authorization evidence
as Actions artifact `preflight-TAG`. Deletion of the probe does not invalidate the
evidence. No release tag, final asset, public release or registry package is created.

For independent gates, use these commands with the environment above:

```sh
python3 scripts/release_preflight.py build
python3 scripts/release_preflight.py authorization
python3 scripts/release_preflight.py version
```

Authorization requires the latest successful exact-SHA dispatch, all jobs passing,
matching run attempt and evidence, and unexpired npm grants. Re-dispatch with
`publish=false` when evidence expires. Missing, failed or unknown evidence fails.
Required workflows are `ci.yml`, `release.yml`, and `publish-packages.yml`.

## Credentials and approvals

The actual publisher is `publish-packages.yml`, job `packages`, environment
`packages-publish`, with `contents: write` and `id-token: write`. Configure npm
trusted publishing for all five existing packages: GitHub owner `BrokkAi`, repo
`feature-bot`, workflow `publish-packages.yml`, environment `packages-publish`.
The workflow uses OIDC, not a developer login or stored NPM_TOKEN. A rejected
exchange must be fixed by an npm package administrator; never upload a test
version to discover rights. Trust validation uses npm's documented
[package token exchange API](https://api-docs.npmjs.com/).

Keep any environment approval and branch/tag rules. Required human approval or
an inaccessible publishing identity blocks readiness. The preflight draft needs
GitHub contents write; no final assets are uploaded to test it.

## Publication and recovery (separate authorization required)

Only after preflight approval, create the exact proposed tag at the prepared
commit and push it. The tag push explicitly starts `publish-packages.yml`;
it is not created by another workflow's GITHUB_TOKEN. Alternatively dispatch
from an existing exact tag with its matching `tag` input and `publish=true`.
A branch dispatch can never publish.

The job rechecks every version before its first upload, then rechecks credentials,
creates missing draft staging, retains matching partial uploads and rejects
conflicts. Four platform packages precede the launcher. A failed npm upload or
unpropagated version leaves GitHub in draft; resume from the same tag after
resolving the failure. Uploads are compared to exact staged bytes in the job.
Never overwrite a conflicting immutable version or move a completed release tag.
Changes after a completed release require a new version.

After publication run `python3 scripts/release_preflight.py published` for every
destination. This verifies the exact Git tag, complete published GitHub assets,
their manifest/checksums and commit/platform metadata, and all five npm versions,
download integrity, unpacked payloads and executable modes against the expected
build. Independent rebuilds compare contents rather than compressor bytes.
A complete existing release takes a read-only verification path on recovery.

For dependency changes, follow [licenses/README.md](licenses/README.md), keep the
published Go dependencies and committed sums, and run `python3 scripts/licenses.py`.

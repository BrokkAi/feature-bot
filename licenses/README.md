# Licensing and third-party notices

Brokk Feature Bot uses [Apache-2.0](../LICENSE); [NOTICE](../NOTICE) identifies the
project. The standard Apache license text is kept unmodified.

Feature Bot is derived from [Brokk Bug Bot](https://github.com/BrokkAi/bug-bot).
The original project's attribution is retained in `NOTICE`, including in every
native archive and npm package.

[THIRD_PARTY_NOTICES.txt](THIRD_PARTY_NOTICES.txt) contains full dependency
license texts, patent grants, and supplemental Go runtime and Unicode notices.
The report includes acp-go, uniseg, golang.org/x/sys, and golang.org/x/term. The
Unicode notice also covers the data tables generated into uniseg.

## Review and regeneration

Use the Go version pinned in `go.mod` and Python 3:

```sh
python3 scripts/licenses.py --write
python3 scripts/licenses.py
```

`policy.json` is a deny-by-default inventory of the complete selected module
graph, including dependencies selected for other platforms. Every module is
reviewed at an exact version, with its license expression, source location,
and SHA-256 hashes of all license, notice, copying, and patent files. The
checker rejects additions, removals, version changes, replacements, changed
legal files, stale reports, and unreviewed Go toolchain updates. It uses Go's
module checksum verification; it does not automatically classify licenses or
approve dependency changes.

When changing dependencies, review upstream source and generated or vendored
material as well as the top-level license. Record any extra attribution in the
supplemental inputs, update the policy deliberately, regenerate the report,
and review its diff. Review Go runtime and standard-library notices when
upgrading the pinned Go version. CI runs the same check.

Separately installed coding agents, Node.js, and Python interpreters have their
own terms and are not included in these distributions.

## Distribution

Go module/source archives retain the root `LICENSE`, `NOTICE`, and `licenses/`
directory. Keep these files when redistributing the project.

Native release archives and every npm package carry `LICENSE`, `NOTICE`, and
`licenses/THIRD_PARTY_NOTICES.txt`. Release packaging checks their contents
against the checkout. The report is generated before packaging, not during an
install. `BUILD.json` identifies each native artifact's exact tag and commit;
its source is available at the matching GitHub tag.

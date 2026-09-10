# Contributing to Brokk Feature Bot

Contributions from people using AI tools are welcome. Everyone remains
responsible for the accuracy, safety, licensing, and relevance of their work.
Please follow the [Code of Conduct](CODE_OF_CONDUCT.md).

## Issues and pull requests

Search existing issues and pull requests before opening a new one. For bugs,
include the version or commit, operating system, reproduction steps, expected
behavior, and actual behavior. Redact secrets and private source code from logs
and transcripts. Report vulnerabilities privately as described in
[SECURITY.md](SECURITY.md).

Keep changes focused. Discuss substantial behavior or interface changes with
maintainers before implementing them. A pull request should explain the
problem, resulting behavior, validation performed, and any remaining limits.
Link related issues and update documentation when behavior changes.

## Development and validation

Install the Go version in `go.mod` and Python 3. Run from the repository root:

```sh
go test -race ./...
go vet ./...
python3 scripts/licenses.py
python3 -m unittest discover -s scripts -p '*_test.py'
```

Format Go changes with `gofmt`. Add focused tests for behavior changes; ordinary
documentation changes need a diff and link review. Tests should use temporary
repositories and simulated agents, without publishing releases or requiring
live credentials.

For installer and release changes, also use Node.js 24 and run the local
packaging smoke checks described in [RELEASING.md](RELEASING.md).

## Licensing and dependencies

This project uses [Apache-2.0](LICENSE). By intentionally submitting a
contribution for inclusion, you submit it under the project's license unless
you explicitly state otherwise, as described in section 5. Submit only work
you have the right to share and preserve upstream attribution and notices.

Dependency versions, legal texts, generated tables, and bundled assets require
license review. Follow [licenses/README.md](licenses/README.md), update the
reviewed policy and notices together, and commit `go.mod` and `go.sum` when
dependencies change. Do not add local replacement directives to a release.

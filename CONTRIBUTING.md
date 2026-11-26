# Contributing

Thanks for helping. Issues and pull requests are welcome.

- Open an issue first for anything larger than a small fix.
- Keep pull requests small and focused, with tests.
- CI is local. Run `git config core.hooksPath .githooks` once after cloning;
  the `pre-push` hook then runs `scripts/ci.sh` (gofmt, `go vet`,
  `go test -race`, plus `govulncheck` and `gitleaks` if installed). Run it by
  hand with `bash scripts/ci.sh`.
- Maintainers release with `scripts/release.sh vX.Y.Z` on a clean, tagged
  tree: it runs CI, then `goreleaser release --clean`, which signs
  `checksums.txt` with cosign. It installs nothing and reports missing tools.
- Tests must not use the network beyond loopback, and must never contain real
  API keys or tokens. Use `internal/mockapi` and obviously fake values.
- Keep dependencies minimal. New dependencies need a good reason.
- The agent stays a thin relay. It must not gain request content, expected
  answers or any grading logic, and must never become a proxy.

Security issues: see [SECURITY.md](SECURITY.md), not the public tracker.

By contributing you agree that your contribution is licensed under the
Apache License 2.0.

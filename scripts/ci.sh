#!/usr/bin/env bash
# Local CI: run before every push (installed as a pre-push hook via
# `git config core.hooksPath .githooks`). Exits non-zero on any failure.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

declare -a summary=()
failed=0

pass() { summary+=("✓ $1"); }
fail() { summary+=("✗ $1"); failed=1; }
skip() { summary+=("- $1 (skipped: $2)"); }

step() {
  local name="$1"; shift
  echo "==> ${name}"
  if "$@"; then pass "${name}"; else fail "${name}"; fi
}

gofmt_check() {
  local out
  out="$(gofmt -l .)"
  if [[ -n "${out}" ]]; then
    echo "gofmt needed on:"
    echo "${out}"
    return 1
  fi
}

step "gofmt" gofmt_check
step "go vet" go vet ./...
step "go test -race" go test -race -count=1 ./...

if command -v govulncheck >/dev/null 2>&1; then
  step "govulncheck" govulncheck ./...
else
  echo "==> govulncheck not installed; skipping (go install golang.org/x/vuln/cmd/govulncheck@latest)"
  skip "govulncheck" "not installed"
fi

if command -v gitleaks >/dev/null 2>&1; then
  if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    step "gitleaks (history)" gitleaks git --no-banner --redact -c .gitleaks.toml .
  else
    step "gitleaks (files)" gitleaks dir --no-banner --redact -c .gitleaks.toml .
  fi
else
  echo "==> gitleaks not installed; skipping"
  skip "gitleaks" "not installed"
fi

echo
echo "Summary:"
printf '  %s\n' "${summary[@]}"
if [[ ${failed} -ne 0 ]]; then
  echo "CI FAILED"
  exit 1
fi
echo "CI PASSED"

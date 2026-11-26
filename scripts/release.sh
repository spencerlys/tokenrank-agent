#!/usr/bin/env bash
# Local release: scripts/release.sh vX.Y.Z
#
# Requires a clean tree with HEAD tagged as the given tag. Runs local CI,
# builds reproducible archives with goreleaser, and signs checksums.txt with
# cosign. Installs nothing: missing tools are reported and the script stops.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

tag="${1:-}"
if [[ ! "${tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.-]+)?$ ]]; then
  echo "usage: scripts/release.sh vX.Y.Z" >&2
  exit 2
fi

missing=()
for tool in git go goreleaser cosign; do
  command -v "${tool}" >/dev/null 2>&1 || missing+=("${tool}")
done
if [[ ${#missing[@]} -ne 0 ]]; then
  echo "missing tools: ${missing[*]}" >&2
  echo "install them yourself (see README), then re-run." >&2
  exit 1
fi

if [[ -n "$(git status --porcelain)" ]]; then
  echo "working tree is not clean; commit or stash first" >&2
  exit 1
fi
if ! git rev-parse -q --verify "refs/tags/${tag}" >/dev/null; then
  echo "tag ${tag} does not exist; create it first: git tag -a ${tag} -m ${tag}" >&2
  exit 1
fi
if [[ "$(git rev-parse HEAD)" != "$(git rev-list -n1 "${tag}")" ]]; then
  echo "HEAD is not at ${tag}; check out the tag first" >&2
  exit 1
fi

bash scripts/ci.sh

# GITHUB_TOKEN must be set for goreleaser to publish the GitHub release.
# The `signs` step in .goreleaser.yaml runs `cosign sign-blob` on
# checksums.txt (keyless: a browser opens for OIDC login, and the signer
# identity is the account you log in with) and uploads
# checksums.txt.sigstore.json with the release.
goreleaser release --clean

cosign verify-blob \
  --bundle dist/checksums.txt.sigstore.json \
  --certificate-identity-regexp '.+' \
  --certificate-oidc-issuer-regexp '.+' \
  dist/checksums.txt

echo
echo "Released ${tag}. Note the signer identity and OIDC issuer from the cosign"
echo "output above, and the Go version ($(go version)), in the release notes."

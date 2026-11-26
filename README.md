# tokenrank-agent

`tokenrank-agent` is a small open-source program that runs TokenRank checks
against **your own** AI API endpoint, with **your own** key, from **your own**
machine. It runs on macOS, Windows and Linux as a single binary.

You might use it if you buy API access from a reseller and want TokenRank to
see what your endpoint really serves, without handing your key to anyone.

> **Status: pre-release.** The TokenRank cloud API is not live yet, so there
> is nothing to register with today. The default API URL,
> `https://api.tokenrank.example`, is a placeholder that resolves nowhere; the
> agent will be pointed at the real address (or you will pass it with `--api`)
> when the service launches. Until then the code can be built, read and tested
> against the bundled mock server (`go test ./...`). There are no releases yet.

## What it does

1. `register` tells the TokenRank cloud your endpoint's base URL and model
   name. It does **not** send your key. The cloud returns an install id, an
   install token and the public keys that sign work batches.
2. `run` polls the cloud on a schedule (default every 15 minutes). When a
   check is due, it fetches a one-time **signed** batch of requests.
3. It verifies the batch before doing anything: Ed25519 signature, that the
   batch is for this install, this endpoint and this protocol, that it has not
   expired, and that its validity window is at most 30 minutes. Every request
   path must be relative to your base URL.
4. It sends the requests straight to your endpoint with the same headers the
   official SDKs send, one at a time, pausing a random interval between them.
   If the endpoint answers 429 or 5xx, or can't be reached, it stops the batch
   at once, waits at least as long as the endpoint's `Retry-After`, and after 3
   such cycles in a row leaves the endpoint alone for 24 hours. A check must
   never add load to a busy endpoint or use up its upstream account's limits.
5. It uploads the raw results: status, output text, token counts, latency,
   time to first token, a few allowlisted response headers, and a SHA-256 of
   the endpoint's request id.

## What it does not do

- **It is not a proxy.** It never sits in front of your traffic. Your own apps
  keep talking to your endpoint directly.
- **Your key never leaves your device except to your own endpoint.** It is
  stored in your OS keychain (macOS Keychain, Windows Credential Manager, or
  the Secret Service on Linux), never in the config file, never sent to
  TokenRank, never logged. If an endpoint echoes your key back, the agent
  replaces it with `[REDACTED]` before uploading.
- **It never sends to any host other than your configured base URL**, and it
  does not follow redirects.
- **It has no grading logic.** The agent contains no request content, no
  expected answers and no scoring. It only relays raw facts.
- **Agent results are signals, never grades.** Only checks that TokenRank runs
  itself can move a public grade.
- It never uploads your own prompts. It only sends the requests in a signed
  batch, and the local audit sidecar (not implemented yet) will only ever
  listen on loopback.

The full contract is in [docs/protocol.md](docs/protocol.md).

## Install

Download the archive for your platform from the
[releases page](https://github.com/spencerlys/tokenrank-agent/releases),
verify it (see below), unpack it and put `tokenrank-agent` on your `PATH`.

Or build from source (Go 1.27 or newer):

```sh
git clone https://github.com/spencerlys/tokenrank-agent
cd tokenrank-agent
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -buildid=" -o tokenrank-agent ./cmd/tokenrank-agent
```

## Usage

Register once. The key is read from a hidden prompt, from `--key-stdin`, or
from `TOKENRANK_ENDPOINT_KEY`. It is never accepted as a command-line flag, so
it does not end up in your shell history.

```sh
tokenrank-agent register \
  --api https://<tokenrank api url> \
  --base-url https://llm.example.com/v1 \
  --model your-model-name
```

Then run the agent. It keeps running until you stop it with Ctrl+C:

```sh
tokenrank-agent run
```

Or run one cycle and exit (handy for cron or a scheduled task):

```sh
tokenrank-agent run --once
```

Other commands and flags:

| | |
|---|---|
| `tokenrank-agent version` | print the version |
| `--api URL` / `TOKENRANK_API_URL` | TokenRank API URL (the built-in default is a placeholder) |
| `--config PATH` | config file (default: `<user config dir>/tokenrank-agent/config.json`) |
| `--jitter-min`, `--jitter-max` | random pause between requests (default 2s–8s) |
| `--timeout` | timeout for each request to your endpoint (default 120s) |
| `tokenrank-agent audit --listen 127.0.0.1:8787` | local audit sidecar (stub; loopback only) |

### Headless Linux

If no Secret Service is available, set `TOKENRANK_ENDPOINT_KEY` in the
agent's environment. The agent then reads the key from the environment on
each run. The install token still needs a working keychain.

## Where things are stored

| what | where |
|---|---|
| endpoint key | OS keychain, service `tokenrank-agent`, account `endpoint-key:<install_id>` (or `TOKENRANK_ENDPOINT_KEY`) |
| install token | OS keychain, service `tokenrank-agent`, account `install-token:<install_id>` |
| config (`api_url`, `install_id`, `base_url`, `protocol`, `model`, `schedule_minutes`, `batch_keys`) | `<user config dir>/tokenrank-agent/config.json`, mode 0600 |

## Verify a release

Each release has a `checksums.txt` signed with [cosign](https://github.com/sigstore/cosign)
keyless signing by a maintainer. The release notes state the signer identity
and OIDC issuer; use them below.

```sh
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity '<signer identity from the release notes>' \
  --certificate-oidc-issuer '<OIDC issuer from the release notes>' \
  checksums.txt

sha256sum --ignore-missing -c checksums.txt   # macOS: shasum -a 256 --ignore-missing -c checksums.txt
```

Builds are reproducible: `CGO_ENABLED=0`, `-trimpath`, `-buildid=` and file
times set to the commit time. To check a binary, run `go version -m
tokenrank-agent` to see the Go version and build settings it was made with,
then build the same tag with that Go version:

```sh
git checkout vX.Y.Z
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -buildid= -X main.version=X.Y.Z" -o tokenrank-agent ./cmd/tokenrank-agent
sha256sum tokenrank-agent
```

## Development

CI runs locally; there is no hosted CI. One-time setup after cloning:

```sh
git config core.hooksPath .githooks
```

The `pre-push` hook then runs `scripts/ci.sh` before every push: gofmt check,
`go vet`, `go test -race`, and `govulncheck` and `gitleaks` when they are
installed. You can run it by hand with `bash scripts/ci.sh`.

Releases are cut locally with `scripts/release.sh vX.Y.Z` on a clean, tagged
tree. It runs `scripts/ci.sh`, then `goreleaser release --clean` (using
`.goreleaser.yaml`), which signs `checksums.txt` with `cosign sign-blob`. The
script installs nothing; it stops and lists any missing tools (`goreleaser`,
`cosign`).

Tests never touch the network beyond loopback: they use a mock TokenRank API
and a fake endpoint (`internal/mockapi`) with obviously fake keys.

See [CONTRIBUTING.md](CONTRIBUTING.md) and [SECURITY.md](SECURITY.md).

## License

Apache License 2.0. See [LICENSE](LICENSE).

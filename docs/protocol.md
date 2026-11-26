# Agent ↔ cloud protocol

This is the public contract between `tokenrank-agent` and the TokenRank cloud.
The agent talks to the cloud only through the three calls below, and to the
user's own endpoint only with requests taken from a verified batch.

All bodies are JSON (`Content-Type: application/json`). The API URL must use
`https` (plain `http` is accepted only for loopback hosts, for local testing).
The agent never follows redirects.

## 1. Register

`POST /v1/agent/register`

```json
{
  "base_url": "https://llm.example.com/v1",
  "model": "some-model",
  "agent_version": "1.0.0",
  "platform": "darwin/arm64"
}
```

The endpoint key is **never** sent. `base_url` is normalised (lower-case
scheme and host, no trailing slash, no query or fragment).

Response `201 Created`:

```json
{
  "install_id": "inst_abc123",
  "install_token": "<opaque bearer token>",
  "protocol": "openai",
  "schedule_minutes": 15,
  "batch_keys": { "key-2026-01": "<base64 Ed25519 public key, 32 bytes>" }
}
```

- `install_id`: 1–128 characters from `[A-Za-z0-9._-]`.
- `protocol`: one of `openai`, `anthropic`, `gemini`, `ark`.
- `schedule_minutes`: how often the agent polls. Missing or `0` means 15.
  The agent applies a local floor of 5 minutes and a cap of 1440.
- `batch_keys`: the keys that may sign batches for this install. Several keys
  may be listed to allow rotation.

The agent stores `install_token` and the endpoint key in the OS keychain and
writes the rest to its config file.

## 2. Fetch the next batch

`GET /v1/agent/batches/next` with `Authorization: Bearer <install_token>`

- `204 No Content`: nothing is due. The agent sends nothing.
- `200 OK`:

```json
{ "key_id": "key-2026-01", "payload": "<base64>", "signature": "<base64>" }
```

`payload` is standard base64 (with padding) of the batch JSON. `signature` is
standard base64 of an Ed25519 signature over the **decoded payload bytes**.
The agent verifies the signature before parsing the payload.

### Batch (decoded payload)

```json
{
  "batch_id": "b_123",
  "install_id": "inst_abc123",
  "endpoint": "https://llm.example.com/v1",
  "protocol": "openai",
  "model": "some-model",
  "issued_at": "2026-01-02T03:04:05Z",
  "expires_at": "2026-01-02T03:14:05Z",
  "probes": [
    {
      "probe_id": "p_1",
      "method": "POST",
      "path": "/chat/completions",
      "body": { "model": "some-model", "messages": [ { "role": "user", "content": "..." } ] }
    }
  ]
}
```

### The agent rejects a batch, and sends nothing, if

- `key_id` is not one of the `batch_keys` from registration, or the
  signature does not verify;
- `install_id` is not this install's id;
- `endpoint` (after normalising a trailing slash) is not the configured base URL;
- `protocol` is not the configured protocol;
- `model` is present and differs from the configured model;
- the batch has expired: local time is after `expires_at` plus 2 minutes of
  clock-skew tolerance, or `issued_at` is more than 2 minutes in the future;
- `expires_at − issued_at` is more than 30 minutes, or not positive;
- `batch_id` or any `probe_id` is not 1–128 characters from `[A-Za-z0-9._-]`,
  or a `probe_id` repeats;
- there are no probes, or more than 100;
- any `method` is not `POST`, or any `body` is not a JSON object;
- any `path` is not relative: it must start with a single `/`, and must not
  contain a scheme, host, `.` or `..` segments, backslashes, a fragment,
  percent-encoding (in the path part), spaces or control characters. A query
  string such as `?alt=sse` is allowed.

Each path is appended to the configured base URL. The agent never sends a
request to any other host.

## 3. Send to the endpoint

For each probe, in order, the agent sends `POST <base_url><path>` with the
`body` bytes unchanged, and pauses a random interval between requests
(default 2–8 s). Headers depend on the protocol:

| protocol | auth header | other headers |
|---|---|---|
| `openai`, `ark` | `Authorization: Bearer <key>` | OpenAI Python SDK style `User-Agent` and `X-Stainless-*` |
| `anthropic` | `x-api-key: <key>` | `anthropic-version: 2023-06-01`, Anthropic Python SDK style `User-Agent` and `X-Stainless-*` |
| `gemini` | `x-goog-api-key: <key>` | google-genai SDK style `User-Agent` and `x-goog-api-client` |

The key is never put in a URL. Each request has a timeout (default 120 s,
including reading the body), at most 4 MiB of each response is read, and
redirects are not followed.

If the response is `text/event-stream`, the agent reads it as a stream and
records the time to the first output token (`ttft_ms`).

## 4. Upload results

`POST /v1/agent/batches/{batch_id}/results` with
`Authorization: Bearer <install_token>`

```json
{
  "batch_id": "b_123",
  "install_id": "inst_abc123",
  "results": [
    {
      "probe_id": "p_1",
      "http_status": 200,
      "output_text": "...",
      "input_tokens": 12,
      "output_tokens": 3,
      "cache_read_tokens": 0,
      "latency_ms": 840,
      "ttft_ms": 0,
      "headers": { "content-type": "application/json", "server": "nginx" },
      "request_id_sha256": "<hex SHA-256 of the endpoint's request id>",
      "error": ""
    }
  ]
}
```

Expected response: `202 Accepted`.

Field notes:

- `http_status` is `0` if no response was received (see `error`).
- `output_text` is the visible output text (thinking text is excluded).
- Token counts are as reported by the endpoint. For Gemini,
  `output_tokens` is `candidatesTokenCount + thoughtsTokenCount`.
- `latency_ms` runs from sending the request to reading the full body.
  `ttft_ms` is `0` for non-streaming responses. Measured values are at least 1.
- `headers` contains only allowlisted response headers, with lower-case
  names; see `AllowedHeaders` in `internal/upload/upload.go`.
- `request_id_sha256` is the SHA-256 of the first request-id header present
  (see `RequestIDHeaders` in `internal/upload/upload.go`), never the id itself.
- Every exact occurrence of the endpoint key (or install token) in
  `output_text`, header values and `error` is replaced with `[REDACTED]`, and
  the encoded body is checked again before it is sent.

The agent uploads raw facts only. It does not grade, score or judge anything.

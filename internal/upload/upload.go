// Package upload turns raw sender results into the upload payload and posts
// it to the TokenRank cloud.
//
// What leaves the device: status, output text, token counts, timings, an
// allowlist of response headers, a SHA-256 of the endpoint's request id, and
// error strings. What never leaves: the endpoint key, the install token in
// any body, request ids in clear, and any header not on the allowlist. Every
// exact occurrence of a secret in output text, header values and errors is
// replaced with "[REDACTED]" before upload, and the encoded payload is checked
// again before it is sent.
package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spencerlys/tokenrank-agent/internal/sender"
)

// Redacted replaces secrets in uploaded strings.
const Redacted = "[REDACTED]"

const maxHeaderValue = 512

// RequestIDHeaders are hashed, never uploaded in clear. The first one present
// (in this order) is used.
var RequestIDHeaders = []string{
	"x-request-id",
	"request-id",
	"x-oneapi-request-id",
	"x-new-api-request-id",
	"openai-request-id",
	"x-amzn-requestid",
	"x-amz-request-id",
	"x-goog-request-id",
	"x-client-request-id",
	"x-tt-logid",
	"x-trace-id",
	"x-ds-trace-id",
	"cf-ray",
}

// AllowedHeaders are the only response headers uploaded (lower-case names).
var AllowedHeaders = map[string]bool{
	"content-type":                       true,
	"server":                             true,
	"via":                                true,
	"x-powered-by":                       true,
	"openai-processing-ms":               true,
	"openai-version":                     true,
	"x-litellm-version":                  true,
	"x-envoy-upstream-service-time":      true,
	"x-ratelimit-limit-requests":         true,
	"x-ratelimit-limit-tokens":           true,
	"anthropic-ratelimit-requests-limit": true,
	"anthropic-ratelimit-tokens-limit":   true,
	"cf-cache-status":                    true,
}

// Result is one uploaded result.
type Result struct {
	ProbeID         string            `json:"probe_id"`
	HTTPStatus      int               `json:"http_status"`
	OutputText      string            `json:"output_text"`
	InputTokens     int64             `json:"input_tokens"`
	OutputTokens    int64             `json:"output_tokens"`
	CacheReadTokens int64             `json:"cache_read_tokens"`
	LatencyMS       int64             `json:"latency_ms"`
	TTFTMS          int64             `json:"ttft_ms"`
	Headers         map[string]string `json:"headers"`
	RequestIDSHA256 string            `json:"request_id_sha256"`
	Error           string            `json:"error"`
}

// Payload is the body of POST /v1/agent/batches/{id}/results.
type Payload struct {
	BatchID   string   `json:"batch_id"`
	InstallID string   `json:"install_id"`
	Results   []Result `json:"results"`
}

// Build converts sender results into a payload, redacting every non-empty
// secret (the endpoint key, the install token).
func Build(batchID, installID string, rs []sender.Result, secrets ...string) Payload {
	secrets = nonEmpty(secrets)
	p := Payload{BatchID: batchID, InstallID: installID, Results: make([]Result, 0, len(rs))}
	for _, r := range rs {
		p.Results = append(p.Results, Result{
			ProbeID:         r.ProbeID,
			HTTPStatus:      r.HTTPStatus,
			OutputText:      redact(r.OutputText, secrets),
			InputTokens:     r.InputTokens,
			OutputTokens:    r.OutputTokens,
			CacheReadTokens: r.CacheReadTokens,
			LatencyMS:       r.LatencyMS,
			TTFTMS:          r.TTFTMS,
			Headers:         allowedHeaders(r.Header, secrets),
			RequestIDSHA256: requestIDHash(r.Header),
			Error:           redact(r.Error, secrets),
		})
	}
	return p
}

func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func redact(s string, secrets []string) string {
	for _, sec := range secrets {
		if strings.Contains(s, sec) {
			s = strings.ReplaceAll(s, sec, Redacted)
		}
	}
	return s
}

func allowedHeaders(h http.Header, secrets []string) map[string]string {
	out := map[string]string{}
	for name, vals := range h {
		lname := strings.ToLower(name)
		if !AllowedHeaders[lname] || len(vals) == 0 {
			continue
		}
		v := redact(strings.Join(vals, ", "), secrets)
		if len(v) > maxHeaderValue {
			v = strings.ToValidUTF8(v[:maxHeaderValue], "")
		}
		out[lname] = v
	}
	return out
}

func requestIDHash(h http.Header) string {
	for _, name := range RequestIDHeaders {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			sum := sha256.Sum256([]byte(v))
			return hex.EncodeToString(sum[:])
		}
	}
	return ""
}

// Encode marshals p and refuses to return bytes that still contain a secret,
// in raw or JSON-escaped form.
func Encode(p Payload, secrets ...string) ([]byte, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	for _, sec := range nonEmpty(secrets) {
		esc, _ := json.Marshal(sec)
		if bytes.Contains(b, []byte(sec)) || bytes.Contains(b, esc[1:len(esc)-1]) {
			return nil, errors.New("refusing to upload: payload still contains a secret")
		}
	}
	return b, nil
}

// Client posts payloads to the cloud.
type Client struct {
	APIURL    string
	Token     string
	HTTP      *http.Client
	UserAgent string
}

// Upload posts the payload. secrets are re-checked in the encoded body.
func (c *Client) Upload(ctx context.Context, p Payload, secrets ...string) error {
	body, err := Encode(p, secrets...)
	if err != nil {
		return err
	}
	u := c.APIURL + "/v1/agent/batches/" + url.PathEscape(p.BatchID) + "/results"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("upload results: %w", err)
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("upload results: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}

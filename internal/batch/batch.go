// Package batch fetches signed work batches from the TokenRank cloud and
// verifies them before anything is sent to the user's endpoint.
//
// Verification order matters: the Ed25519 signature is checked over the raw
// payload bytes before the payload is parsed. Only then are the binding
// fields (install id, endpoint, protocol, model) and the validity window
// checked, and every request path is confirmed to be relative.
package batch

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spencerlys/tokenrank-agent/internal/config"
)

const (
	// MaxWindow is the longest allowed gap between issued_at and expires_at.
	MaxWindow = 30 * time.Minute
	// ClockSkew is the tolerance applied to the local clock.
	ClockSkew = 2 * time.Minute
	// MaxPayloadBytes caps the decoded payload size.
	MaxPayloadBytes = 1 << 20
	// MaxProbes caps the number of requests in one batch.
	MaxProbes = 100
)

// ErrRejected wraps every reason a batch is refused.
var ErrRejected = errors.New("batch rejected")

// Envelope is the signed wrapper returned by GET /v1/agent/batches/next.
type Envelope struct {
	KeyID     string `json:"key_id"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// Probe is one request the cloud asks the agent to send.
type Probe struct {
	ProbeID string          `json:"probe_id"`
	Method  string          `json:"method"`
	Path    string          `json:"path"`
	Body    json.RawMessage `json:"body"`
}

// Batch is the decoded, verified payload.
type Batch struct {
	BatchID   string    `json:"batch_id"`
	InstallID string    `json:"install_id"`
	Endpoint  string    `json:"endpoint"`
	Protocol  string    `json:"protocol"`
	Model     string    `json:"model"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Probes    []Probe   `json:"probes"`
}

// Expect holds what a batch must be bound to.
type Expect struct {
	InstallID string
	BaseURL   string // normalised, see config.NormalizeBaseURL
	Protocol  string
	Model     string
	Keys      map[string]ed25519.PublicKey
	Now       time.Time
}

func reject(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrRejected}, args...)...)
}

// ParseKeys decodes base64 Ed25519 public keys keyed by key id.
func ParseKeys(in map[string]string) (map[string]ed25519.PublicKey, error) {
	if len(in) == 0 {
		return nil, errors.New("no batch signing keys")
	}
	out := make(map[string]ed25519.PublicKey, len(in))
	for id, b64 := range in {
		if id == "" {
			return nil, errors.New("empty batch key id")
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("batch key %q: %w", id, err)
		}
		if len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("batch key %q: want %d bytes, got %d", id, ed25519.PublicKeySize, len(raw))
		}
		out[id] = ed25519.PublicKey(raw)
	}
	return out, nil
}

// Verify checks the envelope signature, then decodes and validates the batch.
func Verify(env *Envelope, exp Expect) (*Batch, error) {
	if env == nil {
		return nil, reject("empty envelope")
	}
	pub, ok := exp.Keys[env.KeyID]
	if !ok || len(pub) != ed25519.PublicKeySize {
		return nil, reject("unknown key_id %q", env.KeyID)
	}
	if base64.StdEncoding.DecodedLen(len(env.Payload)) > MaxPayloadBytes+3 {
		return nil, reject("payload too large")
	}
	payload, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return nil, reject("payload is not valid base64")
	}
	sig, err := base64.StdEncoding.DecodeString(env.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, reject("signature is not a valid base64 Ed25519 signature")
	}
	if !ed25519.Verify(pub, payload, sig) {
		return nil, reject("invalid signature")
	}

	// Only now is the payload trusted enough to parse.
	var b Batch
	dec := json.NewDecoder(bytes.NewReader(payload))
	if err := dec.Decode(&b); err != nil {
		return nil, reject("payload is not valid batch JSON: %v", err)
	}
	if err := check(&b, exp); err != nil {
		return nil, err
	}
	return &b, nil
}

func check(b *Batch, exp Expect) error {
	if !config.ValidID(b.BatchID) {
		return reject("malformed batch_id")
	}
	if b.InstallID != exp.InstallID {
		return reject("batch is for a different install")
	}
	ep, err := config.NormalizeBaseURL(b.Endpoint)
	if err != nil || ep != exp.BaseURL {
		return reject("batch endpoint does not match the configured base URL")
	}
	if b.Protocol != exp.Protocol {
		return reject("batch protocol %q does not match configured %q", b.Protocol, exp.Protocol)
	}
	if b.Model != "" && exp.Model != "" && b.Model != exp.Model {
		return reject("batch model does not match the configured model")
	}
	if b.IssuedAt.IsZero() || b.ExpiresAt.IsZero() {
		return reject("issued_at and expires_at are required")
	}
	if !b.ExpiresAt.After(b.IssuedAt) {
		return reject("expires_at is not after issued_at")
	}
	if b.ExpiresAt.Sub(b.IssuedAt) > MaxWindow {
		return reject("validity window exceeds %v", MaxWindow)
	}
	now := exp.Now
	if now.IsZero() {
		now = time.Now()
	}
	if now.After(b.ExpiresAt.Add(ClockSkew)) {
		return reject("batch expired at %s", b.ExpiresAt.Format(time.RFC3339))
	}
	if b.IssuedAt.After(now.Add(ClockSkew)) {
		return reject("batch issued in the future")
	}
	if len(b.Probes) == 0 {
		return reject("batch has no requests")
	}
	if len(b.Probes) > MaxProbes {
		return reject("batch has more than %d requests", MaxProbes)
	}
	seen := make(map[string]bool, len(b.Probes))
	for i, p := range b.Probes {
		if !config.ValidID(p.ProbeID) {
			return reject("request %d: malformed probe_id", i)
		}
		if seen[p.ProbeID] {
			return reject("request %d: duplicate probe_id", i)
		}
		seen[p.ProbeID] = true
		if p.Method != "POST" {
			return reject("request %s: method must be POST", p.ProbeID)
		}
		if err := ValidatePath(p.Path); err != nil {
			return reject("request %s: %v", p.ProbeID, err)
		}
		if !isJSONObject(p.Body) {
			return reject("request %s: body must be a JSON object", p.ProbeID)
		}
	}
	return nil
}

func isJSONObject(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || t[0] != '{' {
		return false
	}
	return json.Valid(t)
}

// ValidatePath checks that p is a relative request path that can only be
// appended to the configured base URL: it must start with a single "/", have
// no scheme, host, user info, fragment, backslash, percent-encoding in the
// path part, control characters or "." / ".." segments. A query string is
// allowed (for example "?alt=sse").
func ValidatePath(p string) error {
	if p == "" || len(p) > 2048 {
		return errors.New("path is empty or too long")
	}
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return errors.New("path must be relative and start with a single /")
	}
	for _, r := range p {
		if r < 0x21 || r == 0x7f || r > 0x7e {
			return errors.New("path contains spaces, control or non-ASCII characters")
		}
	}
	if strings.ContainsAny(p, `\#`) {
		return errors.New("path must not contain a backslash or fragment")
	}
	pathPart, _, _ := strings.Cut(p, "?")
	if strings.Contains(pathPart, "%") {
		return errors.New("path must not contain percent-encoding")
	}
	if strings.Contains(pathPart, "://") {
		return errors.New("path must not contain a scheme")
	}
	for _, seg := range strings.Split(pathPart, "/") {
		if seg == "." || seg == ".." {
			return errors.New("path must not contain . or .. segments")
		}
	}
	u, err := url.Parse(p)
	if err != nil {
		return fmt.Errorf("unparseable path: %v", err)
	}
	if u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return errors.New("path must not contain a scheme or host")
	}
	return nil
}

// JoinURL appends a validated relative path to the configured base URL. The
// result always has the base URL's scheme and host.
func JoinURL(baseURL, probePath string) (*url.URL, error) {
	if err := ValidatePath(probePath); err != nil {
		return nil, err
	}
	b, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	p, err := url.Parse(probePath)
	if err != nil {
		return nil, err
	}
	u := &url.URL{
		Scheme:   b.Scheme,
		Host:     b.Host,
		Path:     strings.TrimRight(b.Path, "/") + p.Path,
		RawQuery: p.RawQuery,
	}
	if u.Host != b.Host || u.Scheme != b.Scheme {
		return nil, errors.New("joined URL escaped the base URL")
	}
	return u, nil
}

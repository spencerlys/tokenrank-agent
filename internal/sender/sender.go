// Package sender sends verified requests to the user's own endpoint, using
// the same headers the official SDKs send, and records the raw response
// facts (status, output text, token usage, latency, time to first token).
//
// The sender only ever talks to the configured base URL. It does not follow
// redirects, caps how much of a response it reads, and applies a timeout to
// each request. It has no knowledge of what any response "should" be.
package sender

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/spencerlys/tokenrank-agent/internal/batch"
	"github.com/spencerlys/tokenrank-agent/internal/config"
)

const (
	// DefaultMaxResponseBytes caps how much of one response body is read.
	DefaultMaxResponseBytes = 4 << 20
	// DefaultTimeout bounds one request, including reading the full body.
	DefaultTimeout = 120 * time.Second
	// DefaultJitterMin and DefaultJitterMax bound the random pause between requests.
	DefaultJitterMin = 2 * time.Second
	DefaultJitterMax = 8 * time.Second

	maxErrorSnippet = 2048
)

// SDK versions mirrored in request headers. They only shape headers.
const (
	openaiSDKVersion    = "1.109.1"
	anthropicSDKVersion = "0.69.0"
	genaiSDKVersion     = "1.38.0"
	pythonVersion       = "3.12.8"
	anthropicAPIVersion = "2023-06-01"
)

// Options configures a Sender.
type Options struct {
	BaseURL  string // normalised endpoint base URL
	Protocol string // openai | anthropic | gemini | ark
	Key      string // the user's endpoint key; used only in request headers

	Client           *http.Client  // nil: NewHTTPClient()
	Timeout          time.Duration // per request; 0: DefaultTimeout
	JitterMin        time.Duration // pause between requests is uniform in [JitterMin, JitterMax]
	JitterMax        time.Duration
	MaxResponseBytes int64 // 0: DefaultMaxResponseBytes
}

// Result is the raw outcome of one request. Header holds the full response
// headers; package upload decides which of them may leave the device.
type Result struct {
	ProbeID         string
	HTTPStatus      int
	OutputText      string
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
	LatencyMS       int64
	TTFTMS          int64
	Header          http.Header
	Error           string
}

// Sender sends requests for one batch.
type Sender struct {
	opts   Options
	client *http.Client
	sleep  func(context.Context, time.Duration) error
}

// NewHTTPClient returns a client that never follows redirects and honours the
// standard proxy environment variables.
func NewHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// New validates opts and returns a Sender.
func New(opts Options) (*Sender, error) {
	if !config.ValidProtocol(opts.Protocol) {
		return nil, fmt.Errorf("unsupported protocol %q", opts.Protocol)
	}
	if opts.Key == "" {
		return nil, errors.New("endpoint key is empty")
	}
	norm, err := config.NormalizeBaseURL(opts.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("base URL: %w", err)
	}
	opts.BaseURL = norm
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if opts.JitterMin < 0 {
		opts.JitterMin = 0
	}
	if opts.JitterMax < opts.JitterMin {
		opts.JitterMax = opts.JitterMin
	}
	c := opts.Client
	if c == nil {
		c = NewHTTPClient()
	} else {
		// Never follow redirects, even with a caller-supplied client.
		cc := *c
		cc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		c = &cc
	}
	return &Sender{opts: opts, client: c, sleep: sleepCtx}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (s *Sender) jitter() time.Duration {
	lo, hi := s.opts.JitterMin, s.opts.JitterMax
	if hi <= lo {
		return lo
	}
	return lo + time.Duration(rand.Int64N(int64(hi-lo)+1))
}

// Send sends each request in order, pausing a random jitter between them.
// It always returns one Result per request.
func (s *Sender) Send(ctx context.Context, probes []batch.Probe) []Result {
	results := make([]Result, 0, len(probes))
	for i, p := range probes {
		if i > 0 {
			if err := s.sleep(ctx, s.jitter()); err != nil {
				for _, rest := range probes[i:] {
					results = append(results, Result{ProbeID: rest.ProbeID, Error: "not sent: " + err.Error()})
				}
				break
			}
		}
		results = append(results, s.sendOne(ctx, p))
	}
	return results
}

func (s *Sender) sendOne(parent context.Context, p batch.Probe) Result {
	res := Result{ProbeID: p.ProbeID}
	u, err := batch.JoinURL(s.opts.BaseURL, p.Path)
	if err != nil {
		res.Error = "not sent: " + err.Error()
		return res
	}
	ctx, cancel := context.WithTimeout(parent, s.opts.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(p.Body))
	if err != nil {
		res.Error = "not sent: " + err.Error()
		return res
	}
	s.setHeaders(req.Header)

	start := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		res.LatencyMS = sinceMS(start)
		res.Error = "request failed: " + err.Error()
		return res
	}
	defer resp.Body.Close()
	res.HTTPStatus = resp.StatusCode
	res.Header = resp.Header.Clone()

	limited := &io.LimitedReader{R: resp.Body, N: s.opts.MaxResponseBytes + 1}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300

	if ok && mediaType == "text/event-stream" {
		readErr := parseStream(s.opts.Protocol, limited, start, &res)
		res.LatencyMS = sinceMS(start)
		if limited.N <= 0 {
			appendErr(&res, fmt.Sprintf("response truncated at %d bytes", s.opts.MaxResponseBytes))
		} else if readErr != nil {
			appendErr(&res, "read stream: "+readErr.Error())
		}
		return res
	}

	body, readErr := io.ReadAll(limited)
	res.LatencyMS = sinceMS(start)
	truncated := int64(len(body)) > s.opts.MaxResponseBytes
	if truncated {
		body = body[:s.opts.MaxResponseBytes]
	}
	if !ok {
		res.Error = fmt.Sprintf("http %d: %s", resp.StatusCode, snippet(body))
		if msg := errorMessage(body); msg != "" {
			res.Error = fmt.Sprintf("http %d: %s", resp.StatusCode, msg)
		}
		return res
	}
	if readErr != nil {
		appendErr(&res, "read body: "+readErr.Error())
	}
	if truncated {
		appendErr(&res, fmt.Sprintf("response truncated at %d bytes", s.opts.MaxResponseBytes))
		return res
	}
	if err := parseBody(s.opts.Protocol, body, &res); err != nil {
		appendErr(&res, err.Error())
		if res.OutputText == "" {
			res.OutputText = snippet(body)
		}
	}
	return res
}

func appendErr(r *Result, msg string) {
	if r.Error == "" {
		r.Error = msg
		return
	}
	r.Error += "; " + msg
}

// sinceMS returns elapsed milliseconds, at least 1 so a measured value is
// never confused with "not measured".
func sinceMS(start time.Time) int64 {
	ms := time.Since(start).Milliseconds()
	if ms < 1 {
		ms = 1
	}
	return ms
}

func snippet(b []byte) string {
	if len(b) > maxErrorSnippet {
		b = b[:maxErrorSnippet]
	}
	return strings.ToValidUTF8(strings.TrimSpace(string(b)), "")
}

// setHeaders sets the auth header and SDK-style headers for the protocol.
// The key is only ever placed in a header, never in the URL.
func (s *Sender) setHeaders(h http.Header) {
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	key := s.opts.Key
	switch s.opts.Protocol {
	case "openai", "ark":
		h.Set("Authorization", "Bearer "+key)
		h.Set("User-Agent", "OpenAI/Python "+openaiSDKVersion)
		setStainless(h, openaiSDKVersion)
	case "anthropic":
		h.Set("X-Api-Key", key)
		h.Set("Anthropic-Version", anthropicAPIVersion)
		h.Set("User-Agent", "Anthropic/Python "+anthropicSDKVersion)
		setStainless(h, anthropicSDKVersion)
	case "gemini":
		h.Set("X-Goog-Api-Key", key)
		ua := "google-genai-sdk/" + genaiSDKVersion + " gl-python/" + pythonVersion
		h.Set("User-Agent", ua)
		h.Set("X-Goog-Api-Client", ua)
	}
}

func setStainless(h http.Header, pkgVersion string) {
	h.Set("X-Stainless-Lang", "python")
	h.Set("X-Stainless-Package-Version", pkgVersion)
	h.Set("X-Stainless-OS", stainlessOS())
	h.Set("X-Stainless-Arch", stainlessArch())
	h.Set("X-Stainless-Runtime", "CPython")
	h.Set("X-Stainless-Runtime-Version", pythonVersion)
	h.Set("X-Stainless-Async", "false")
	h.Set("X-Stainless-Retry-Count", "0")
}

func stainlessOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "MacOS"
	case "linux":
		return "Linux"
	case "windows":
		return "Windows"
	case "freebsd":
		return "FreeBSD"
	}
	return "Other:" + runtime.GOOS
}

func stainlessArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	case "386":
		return "x32"
	}
	return "other:" + runtime.GOARCH
}

// errorMessage extracts {"error": {"message": ...}} style messages.
func errorMessage(body []byte) string {
	var e struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || len(e.Error) == 0 {
		return ""
	}
	return apiErrorText(e.Error)
}

func apiErrorText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var obj struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Status  string `json:"status"`
	}
	if json.Unmarshal(raw, &obj) == nil && (obj.Message != "" || obj.Type != "" || obj.Status != "") {
		parts := []string{}
		for _, p := range []string{obj.Type, obj.Status} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		if obj.Message != "" {
			parts = append(parts, obj.Message)
		}
		return snippet([]byte(strings.Join(parts, ": ")))
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return snippet([]byte(str))
	}
	return snippet(raw)
}

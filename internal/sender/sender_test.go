package sender

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spencerlys/tokenrank-agent/internal/batch"
	"github.com/spencerlys/tokenrank-agent/internal/mockapi"
)

func toBatchProbes(in []mockapi.Probe) []batch.Probe {
	out := make([]batch.Probe, len(in))
	for i, p := range in {
		out[i] = batch.Probe{ProbeID: p.ProbeID, Method: p.Method, Path: p.Path, Body: p.Body}
	}
	return out
}

func newSender(t *testing.T, base, protocol string) *Sender {
	t.Helper()
	s, err := New(Options{BaseURL: base, Protocol: protocol, Key: mockapi.FakeEndpointKey, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSendAllProtocols(t *testing.T) {
	for _, proto := range []string{"openai", "anthropic", "gemini", "ark"} {
		t.Run(proto, func(t *testing.T) {
			ep := mockapi.NewEndpoint(t, proto, mockapi.FakeEndpointKey)
			s := newSender(t, ep.BaseURL, proto)
			res := s.Send(context.Background(), toBatchProbes(mockapi.DummyProbes(proto)))
			if len(res) != 2 {
				t.Fatalf("got %d results", len(res))
			}
			for i, r := range res {
				if r.Error != "" || r.HTTPStatus != 200 {
					t.Fatalf("result %d: status %d error %q", i, r.HTTPStatus, r.Error)
				}
				if r.OutputText != mockapi.FakeText {
					t.Errorf("result %d: output %q", i, r.OutputText)
				}
				if r.InputTokens != mockapi.FakeInputTokens || r.OutputTokens != mockapi.FakeOutputTokens || r.CacheReadTokens != mockapi.FakeCacheTokens {
					t.Errorf("result %d: tokens in=%d out=%d cache=%d", i, r.InputTokens, r.OutputTokens, r.CacheReadTokens)
				}
				if r.LatencyMS <= 0 {
					t.Errorf("result %d: latency not measured", i)
				}
			}
			if res[0].TTFTMS != 0 {
				t.Errorf("non-stream TTFT = %d, want 0", res[0].TTFTMS)
			}
			if res[1].TTFTMS <= 0 || res[1].TTFTMS > res[1].LatencyMS {
				t.Errorf("stream TTFT = %d (latency %d)", res[1].TTFTMS, res[1].LatencyMS)
			}
			reqs := ep.Requests()
			if len(reqs) != 2 {
				t.Fatalf("endpoint saw %d requests", len(reqs))
			}
			for _, rq := range reqs {
				if strings.Contains(rq.RawQuery, mockapi.FakeEndpointKey) || strings.Contains(rq.Path, mockapi.FakeEndpointKey) {
					t.Fatal("key leaked into the URL")
				}
			}
		})
	}
}

func TestHeadersPerProtocol(t *testing.T) {
	cases := map[string]map[string]string{
		"openai":    {"Authorization": "Bearer " + mockapi.FakeEndpointKey, "X-Stainless-Lang": "python", "User-Agent": "OpenAI/Python " + openaiSDKVersion},
		"ark":       {"Authorization": "Bearer " + mockapi.FakeEndpointKey, "X-Stainless-Runtime": "CPython"},
		"anthropic": {"X-Api-Key": mockapi.FakeEndpointKey, "Anthropic-Version": "2023-06-01", "User-Agent": "Anthropic/Python " + anthropicSDKVersion},
		"gemini":    {"X-Goog-Api-Key": mockapi.FakeEndpointKey, "X-Goog-Api-Client": "google-genai-sdk/" + genaiSDKVersion + " gl-python/" + pythonVersion},
	}
	for proto, want := range cases {
		ep := mockapi.NewEndpoint(t, proto, mockapi.FakeEndpointKey)
		s := newSender(t, ep.BaseURL, proto)
		s.Send(context.Background(), toBatchProbes(mockapi.DummyProbes(proto))[:1])
		reqs := ep.Requests()
		if len(reqs) != 1 {
			t.Fatalf("%s: %d requests", proto, len(reqs))
		}
		for k, v := range want {
			if got := reqs[0].Header.Get(k); got != v {
				t.Errorf("%s: header %s = %q, want %q", proto, k, got, v)
			}
		}
		if proto != "openai" && proto != "ark" && reqs[0].Header.Get("Authorization") != "" {
			t.Errorf("%s: unexpected Authorization header", proto)
		}
	}
}

func TestNoRedirectFollow(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer target.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/chat/completions", http.StatusTemporaryRedirect)
	}))
	defer src.Close()

	s := newSender(t, src.URL+"/v1", "openai")
	res := s.Send(context.Background(), toBatchProbes(mockapi.DummyProbes("openai"))[:1])
	if res[0].HTTPStatus != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307", res[0].HTTPStatus)
	}
	if hits.Load() != 0 {
		t.Fatal("redirect was followed")
	}
}

func TestResponseCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + strings.Repeat("A", 5000) + `"}}]}`))
	}))
	defer srv.Close()
	s, err := New(Options{BaseURL: srv.URL, Protocol: "openai", Key: mockapi.FakeEndpointKey, MaxResponseBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	res := s.Send(context.Background(), toBatchProbes(mockapi.DummyProbes("openai"))[:1])
	if !strings.Contains(res[0].Error, "truncated") {
		t.Fatalf("error = %q, want truncation", res[0].Error)
	}
}

func TestTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	s, err := New(Options{BaseURL: srv.URL, Protocol: "openai", Key: mockapi.FakeEndpointKey, Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	res := s.Send(context.Background(), toBatchProbes(mockapi.DummyProbes("openai"))[:1])
	if !strings.Contains(res[0].Error, "request failed") {
		t.Fatalf("error = %q, want timeout", res[0].Error)
	}
}

func TestErrorStatus(t *testing.T) {
	ep := mockapi.NewEndpoint(t, "anthropic", "sk-fake-some-other-key")
	s := newSender(t, ep.BaseURL, "anthropic")
	res := s.Send(context.Background(), toBatchProbes(mockapi.DummyProbes("anthropic"))[:1])
	if res[0].HTTPStatus != 401 || !strings.Contains(res[0].Error, "invalid api key") {
		t.Fatalf("got status %d error %q", res[0].HTTPStatus, res[0].Error)
	}
}

func TestJitterBetweenRequests(t *testing.T) {
	ep := mockapi.NewEndpoint(t, "openai", mockapi.FakeEndpointKey)
	s, err := New(Options{BaseURL: ep.BaseURL, Protocol: "openai", Key: mockapi.FakeEndpointKey, JitterMin: time.Second, JitterMax: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var slept []time.Duration
	s.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	s.Send(context.Background(), toBatchProbes(mockapi.DummyProbes("openai")))
	if len(slept) != 1 || slept[0] < time.Second || slept[0] > 3*time.Second {
		t.Fatalf("slept %v, want one pause in [1s,3s]", slept)
	}
}

func TestCancelledContextMarksRemaining(t *testing.T) {
	ep := mockapi.NewEndpoint(t, "openai", mockapi.FakeEndpointKey)
	s := newSender(t, ep.BaseURL, "openai")
	ctx, cancel := context.WithCancel(context.Background())
	s.sleep = func(context.Context, time.Duration) error { cancel(); return context.Canceled }
	res := s.Send(ctx, toBatchProbes(mockapi.DummyProbes("openai")))
	if len(res) != 2 || !strings.HasPrefix(res[1].Error, "not sent") {
		t.Fatalf("results: %+v", res)
	}
	if n := len(ep.Requests()); n != 1 {
		t.Fatalf("endpoint saw %d requests, want 1", n)
	}
}

func TestParseOpenAIResponsesAPI(t *testing.T) {
	var r Result
	body := `{"output":[{"type":"message","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":7,"output_tokens":2,"input_tokens_details":{"cached_tokens":1}}}`
	if err := parseBody("openai", []byte(body), &r); err != nil {
		t.Fatal(err)
	}
	if r.OutputText != "OK" || r.InputTokens != 7 || r.OutputTokens != 2 || r.CacheReadTokens != 1 {
		t.Fatalf("got %+v", r)
	}
}

func TestParseGeminiArrayAndThoughts(t *testing.T) {
	var r Result
	body := `[{"candidates":[{"content":{"parts":[{"text":"hmm","thought":true},{"text":"O"}]}}]},
	{"candidates":[{"content":{"parts":[{"text":"K"}]}}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"thoughtsTokenCount":3}}]`
	if err := parseBody("gemini", []byte(body), &r); err != nil {
		t.Fatal(err)
	}
	if r.OutputText != "OK" || r.InputTokens != 5 || r.OutputTokens != 5 {
		t.Fatalf("got %+v", r)
	}
}

func TestParseStreamAnthropicThinkingCountsForTTFT(t *testing.T) {
	var r Result
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9,\"output_tokens\":1}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"...\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":6}}\n\n"
	if err := parseStream("anthropic", strings.NewReader(stream), time.Now(), &r); err != nil {
		t.Fatal(err)
	}
	if r.OutputText != "OK" || r.InputTokens != 9 || r.OutputTokens != 6 || r.TTFTMS <= 0 {
		t.Fatalf("got %+v", r)
	}
}

func TestParseStreamGarbage(t *testing.T) {
	var r Result
	if err := parseStream("openai", strings.NewReader("data: not json\n\ndata: also not\n\n"), time.Now(), &r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Error, "no parseable stream events") {
		t.Fatalf("error = %q", r.Error)
	}
}

func TestUnparseableBodyKeepsSnippet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>gateway</html>"))
	}))
	defer srv.Close()
	s := newSender(t, srv.URL, "openai")
	res := s.Send(context.Background(), toBatchProbes(mockapi.DummyProbes("openai"))[:1])
	if res[0].OutputText != "<html>gateway</html>" || res[0].Error == "" {
		t.Fatalf("got %+v", res[0])
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Options{BaseURL: "https://x.example", Protocol: "smtp", Key: "k"}); err == nil {
		t.Fatal("bad protocol accepted")
	}
	if _, err := New(Options{BaseURL: "https://x.example", Protocol: "openai"}); err == nil {
		t.Fatal("empty key accepted")
	}
	if _, err := New(Options{BaseURL: "x.example", Protocol: "openai", Key: "k"}); err == nil {
		t.Fatal("bad base URL accepted")
	}
}

// Ensure the probe body is forwarded byte for byte.
func TestBodyForwardedVerbatim(t *testing.T) {
	ep := mockapi.NewEndpoint(t, "openai", mockapi.FakeEndpointKey)
	s := newSender(t, ep.BaseURL, "openai")
	probes := toBatchProbes(mockapi.DummyProbes("openai"))[:1]
	s.Send(context.Background(), probes)
	got := ep.Requests()[0].Body
	if string(got) != string(probes[0].Body) {
		t.Fatalf("body changed: %s vs %s", got, probes[0].Body)
	}
	var v map[string]any
	if json.Unmarshal(got, &v) != nil {
		t.Fatal("body not JSON")
	}
}

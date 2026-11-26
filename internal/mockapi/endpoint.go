package mockapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Values the fake endpoint reports for every successful request.
const (
	FakeText         = "OK"
	FakeInputTokens  = 12
	FakeOutputTokens = 3
	FakeCacheTokens  = 4
	FakeRequestID    = "req-fake-0001"
	FakeServer       = "fake-endpoint"
	// FakePrivateHeader is a response header that must not be uploaded.
	FakePrivateHeader = "X-Internal-Debug"
)

// Request is one request the fake endpoint received.
type Request struct {
	Method   string
	Path     string // path relative to the base URL
	RawQuery string
	Header   http.Header
	Body     []byte
}

// Endpoint is a fake model API that answers in the shape of one protocol.
type Endpoint struct {
	BaseURL  string
	Protocol string
	Key      string

	srv *httptest.Server

	mu      sync.Mutex
	reqs    []Request
	echoKey bool
}

// NewEndpoint starts a fake endpoint that accepts key.
func NewEndpoint(t testing.TB, protocol, key string) *Endpoint {
	t.Helper()
	e := &Endpoint{Protocol: protocol, Key: key}
	e.srv = httptest.NewServer(http.HandlerFunc(e.serve))
	e.BaseURL = e.srv.URL + PathPrefix(protocol)
	t.Cleanup(e.srv.Close)
	return e
}

// SetEchoKey makes the endpoint echo the key back in the output text and in
// response headers, to test redaction.
func (e *Endpoint) SetEchoKey(v bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.echoKey = v
}

// Requests returns the recorded requests.
func (e *Endpoint) Requests() []Request {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Request(nil), e.reqs...)
}

func (e *Endpoint) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	rel, ok := trimPrefix(r.URL.Path, PathPrefix(e.Protocol))
	e.mu.Lock()
	e.reqs = append(e.reqs, Request{Method: r.Method, Path: rel, RawQuery: r.URL.RawQuery, Header: r.Header.Clone(), Body: body})
	echo := e.echoKey
	e.mu.Unlock()

	if !ok || r.Method != http.MethodPost {
		http.Error(w, `{"error":{"message":"not found"}}`, http.StatusNotFound)
		return
	}
	if !e.authOK(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"invalid api key"}}`)
		return
	}

	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)
	stream := req.Stream
	if e.Protocol == "gemini" {
		stream = strings.HasSuffix(rel, ":streamGenerateContent")
	}

	text := FakeText
	h := w.Header()
	h.Set("Server", FakeServer)
	h.Set("X-Request-Id", FakeRequestID)
	h.Set(FakePrivateHeader, "should-not-be-uploaded")
	if echo {
		text = FakeText + " " + e.Key
		h.Set("Via", "1.1 "+e.Key) // allowlisted header carrying the key
		h.Set("X-Echo-Key", e.Key)
	}

	if !stream {
		h.Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(e.plainBody(text))
		return
	}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	for _, ev := range e.streamEvents(text) {
		if ev.name != "" {
			fmt.Fprintf(w, "event: %s\n", ev.name)
		}
		fmt.Fprintf(w, "data: %s\n\n", ev.data)
		if fl != nil {
			fl.Flush()
		}
	}
}

func (e *Endpoint) authOK(r *http.Request) bool {
	switch e.Protocol {
	case "anthropic":
		return r.Header.Get("X-Api-Key") == e.Key && r.Header.Get("Anthropic-Version") == "2023-06-01"
	case "gemini":
		return r.Header.Get("X-Goog-Api-Key") == e.Key && r.URL.Query().Get("key") == ""
	default:
		return r.Header.Get("Authorization") == "Bearer "+e.Key
	}
}

func (e *Endpoint) plainBody(text string) any {
	switch e.Protocol {
	case "anthropic":
		return map[string]any{
			"id": "msg_fake", "type": "message", "role": "assistant", "model": DummyModel,
			"content":     []any{map[string]any{"type": "text", "text": text}},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": FakeInputTokens, "output_tokens": FakeOutputTokens, "cache_read_input_tokens": FakeCacheTokens},
		}
	case "gemini":
		return map[string]any{
			"candidates":    []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": text}}}, "finishReason": "STOP"}},
			"usageMetadata": map[string]any{"promptTokenCount": FakeInputTokens, "candidatesTokenCount": FakeOutputTokens, "cachedContentTokenCount": FakeCacheTokens, "totalTokenCount": FakeInputTokens + FakeOutputTokens},
		}
	default:
		return map[string]any{
			"id": "chatcmpl-fake", "object": "chat.completion", "model": DummyModel,
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": FakeInputTokens, "completion_tokens": FakeOutputTokens, "total_tokens": FakeInputTokens + FakeOutputTokens,
				"prompt_tokens_details": map[string]any{"cached_tokens": FakeCacheTokens}},
		}
	}
}

type sseEvent struct {
	name string
	data string
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (e *Endpoint) streamEvents(text string) []sseEvent {
	first, rest := text[:1], text[1:]
	switch e.Protocol {
	case "anthropic":
		delta := func(s string) sseEvent {
			return sseEvent{"content_block_delta", mustJSON(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": s}})}
		}
		return []sseEvent{
			{"message_start", mustJSON(map[string]any{"type": "message_start", "message": map[string]any{
				"id": "msg_fake", "type": "message", "role": "assistant", "model": DummyModel, "content": []any{},
				"usage": map[string]any{"input_tokens": FakeInputTokens, "cache_read_input_tokens": FakeCacheTokens, "output_tokens": 1}}})},
			{"content_block_start", mustJSON(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})},
			{"ping", `{"type":"ping"}`},
			delta(first),
			delta(rest),
			{"content_block_stop", `{"type":"content_block_stop","index":0}`},
			{"message_delta", mustJSON(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": FakeOutputTokens}})},
			{"message_stop", `{"type":"message_stop"}`},
		}
	case "gemini":
		chunk := func(s string, out int) sseEvent {
			return sseEvent{"", mustJSON(map[string]any{
				"candidates":    []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": s}}}}},
				"usageMetadata": map[string]any{"promptTokenCount": FakeInputTokens, "candidatesTokenCount": out, "cachedContentTokenCount": FakeCacheTokens},
			})}
		}
		return []sseEvent{chunk(first, 1), chunk(rest, FakeOutputTokens)}
	default:
		chunk := func(delta map[string]any, finish any) sseEvent {
			return sseEvent{"", mustJSON(map[string]any{"id": "chatcmpl-fake", "object": "chat.completion.chunk", "model": DummyModel,
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})}
		}
		return []sseEvent{
			chunk(map[string]any{"role": "assistant", "content": ""}, nil),
			chunk(map[string]any{"content": first}, nil),
			chunk(map[string]any{"content": rest}, nil),
			chunk(map[string]any{}, "stop"),
			{"", mustJSON(map[string]any{"id": "chatcmpl-fake", "object": "chat.completion.chunk", "choices": []any{},
				"usage": map[string]any{"prompt_tokens": FakeInputTokens, "completion_tokens": FakeOutputTokens, "total_tokens": FakeInputTokens + FakeOutputTokens,
					"prompt_tokens_details": map[string]any{"cached_tokens": FakeCacheTokens}}})},
			{"", "[DONE]"},
		}
	}
}

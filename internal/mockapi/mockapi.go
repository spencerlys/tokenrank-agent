// Package mockapi provides test doubles: a mock TokenRank cloud API (register,
// next batch, results) that signs dummy batches with a throwaway Ed25519 key,
// and a fake model endpoint that answers in each protocol's response shape.
//
// Everything here is for tests. All keys and tokens are obviously fake, and
// the only request content is a dummy "Reply with OK" message.
package mockapi

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Obviously fake credentials used across tests.
const (
	FakeEndpointKey  = "sk-fake-0000-tokenrank-agent-test-key-not-real"
	FakeInstallToken = "fake-install-token-0000-not-real"
	FakeInstallID    = "inst_test_0001"
	KeyID            = "test-key-1"
	DummyModel       = "dummy-model"
)

// Probe mirrors one request in a batch.
type Probe struct {
	ProbeID string          `json:"probe_id"`
	Method  string          `json:"method"`
	Path    string          `json:"path"`
	Body    json.RawMessage `json:"body"`
}

// Batch mirrors the batch payload. Tests can set any field, including invalid
// values, to exercise rejection paths.
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

// Envelope mirrors the signed wrapper.
type Envelope struct {
	KeyID     string `json:"key_id"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// Upload is one recorded POST to the results endpoint.
type Upload struct {
	Path string
	Body []byte
}

// API is a mock TokenRank cloud.
type API struct {
	URL             string
	Protocol        string
	ScheduleMinutes int

	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	srv  *httptest.Server

	mu             sync.Mutex
	queue          []Envelope
	registerBodies [][]byte
	uploads        []Upload
	nextCalls      int
	badAuth        int
}

// NewAPI starts a mock API that registers installs with the given protocol.
func NewAPI(t testing.TB, protocol string) *API {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a := &API{Protocol: protocol, ScheduleMinutes: 15, pub: pub, priv: priv}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/agent/register", a.register)
	mux.HandleFunc("GET /v1/agent/batches/next", a.next)
	mux.HandleFunc("POST /v1/agent/batches/{id}/results", a.results)
	a.srv = httptest.NewServer(mux)
	a.URL = a.srv.URL
	t.Cleanup(a.srv.Close)
	return a
}

// BatchKeys returns the key set the API hands out at register time.
func (a *API) BatchKeys() map[string]string {
	return map[string]string{KeyID: base64.StdEncoding.EncodeToString(a.pub)}
}

// Sign signs payload with the API's key.
func (a *API) Sign(payload []byte) Envelope {
	return Envelope{
		KeyID:     KeyID,
		Payload:   base64.StdEncoding.EncodeToString(payload),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(a.priv, payload)),
	}
}

// QueueBatch signs b and queues it for the next GET /batches/next.
func (a *API) QueueBatch(t testing.TB, b Batch) {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	a.QueueEnvelope(a.Sign(raw))
}

// QueueEnvelope queues a raw envelope (possibly tampered).
func (a *API) QueueEnvelope(e Envelope) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.queue = append(a.queue, e)
}

// NewBatch returns a valid batch for the registered install and endpoint.
func (a *API) NewBatch(endpoint string) Batch {
	now := time.Now().UTC().Truncate(time.Second)
	return Batch{
		BatchID:   "batch_test_0001",
		InstallID: FakeInstallID,
		Endpoint:  endpoint,
		Protocol:  a.Protocol,
		Model:     DummyModel,
		IssuedAt:  now.Add(-30 * time.Second),
		ExpiresAt: now.Add(10 * time.Minute),
		Probes:    DummyProbes(a.Protocol),
	}
}

// RegisterBodies returns the raw bodies of register calls.
func (a *API) RegisterBodies() [][]byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([][]byte(nil), a.registerBodies...)
}

// Uploads returns recorded result uploads.
func (a *API) Uploads() []Upload {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Upload(nil), a.uploads...)
}

// NextCalls returns how many times /batches/next was called with valid auth.
func (a *API) NextCalls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.nextCalls
}

// BadAuth returns how many calls carried a wrong or missing install token.
func (a *API) BadAuth() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.badAuth
}

func (a *API) authorized(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") == "Bearer "+FakeInstallToken {
		return true
	}
	a.mu.Lock()
	a.badAuth++
	a.mu.Unlock()
	http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	return false
}

func (a *API) register(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req struct {
		BaseURL      string `json:"base_url"`
		Model        string `json:"model"`
		AgentVersion string `json:"agent_version"`
		Platform     string `json:"platform"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.BaseURL == "" || req.Model == "" {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	a.registerBodies = append(a.registerBodies, body)
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"install_id":       FakeInstallID,
		"install_token":    FakeInstallToken,
		"protocol":         a.Protocol,
		"schedule_minutes": a.ScheduleMinutes,
		"batch_keys":       a.BatchKeys(),
	})
}

func (a *API) next(w http.ResponseWriter, r *http.Request) {
	if !a.authorized(w, r) {
		return
	}
	a.mu.Lock()
	a.nextCalls++
	if len(a.queue) == 0 {
		a.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	e := a.queue[0]
	a.queue = a.queue[1:]
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(e)
}

func (a *API) results(w http.ResponseWriter, r *http.Request) {
	if !a.authorized(w, r) {
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	a.mu.Lock()
	a.uploads = append(a.uploads, Upload{Path: r.URL.Path, Body: body})
	a.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
}

// DummyProbes returns two obviously dummy requests for the protocol: one
// plain and one streaming.
func DummyProbes(protocol string) []Probe {
	msg := []any{map[string]any{"role": "user", "content": "Reply with OK"}}
	mk := func(id, path string, body map[string]any) Probe {
		raw, _ := json.Marshal(body)
		return Probe{ProbeID: id, Method: "POST", Path: path, Body: raw}
	}
	switch protocol {
	case "anthropic":
		return []Probe{
			mk("probe_plain", "/messages", map[string]any{"model": DummyModel, "max_tokens": 5, "messages": msg}),
			mk("probe_stream", "/messages", map[string]any{"model": DummyModel, "max_tokens": 5, "messages": msg, "stream": true}),
		}
	case "gemini":
		contents := []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "Reply with OK"}}}}
		return []Probe{
			mk("probe_plain", "/models/"+DummyModel+":generateContent", map[string]any{"contents": contents}),
			mk("probe_stream", "/models/"+DummyModel+":streamGenerateContent?alt=sse", map[string]any{"contents": contents}),
		}
	default: // openai, ark
		return []Probe{
			mk("probe_plain", "/chat/completions", map[string]any{"model": DummyModel, "max_tokens": 5, "messages": msg}),
			mk("probe_stream", "/chat/completions", map[string]any{"model": DummyModel, "max_tokens": 5, "messages": msg,
				"stream": true, "stream_options": map[string]any{"include_usage": true}}),
		}
	}
}

// PathPrefix is the base URL path the fake endpoint serves for a protocol.
func PathPrefix(protocol string) string {
	switch protocol {
	case "gemini":
		return "/v1beta"
	case "ark":
		return "/api/v3"
	}
	return "/v1"
}

// trimPrefix is strings.TrimPrefix that also reports whether it matched.
func trimPrefix(s, prefix string) (string, bool) {
	if !strings.HasPrefix(s, prefix) {
		return s, false
	}
	return strings.TrimPrefix(s, prefix), true
}

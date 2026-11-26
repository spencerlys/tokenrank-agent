package agent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/spencerlys/tokenrank-agent/internal/batch"
	"github.com/spencerlys/tokenrank-agent/internal/config"
	"github.com/spencerlys/tokenrank-agent/internal/keychain"
	"github.com/spencerlys/tokenrank-agent/internal/mockapi"
	"github.com/spencerlys/tokenrank-agent/internal/upload"
)

type harness struct {
	api  *mockapi.API
	ep   *mockapi.Endpoint
	env  Env
	cfg  string
	opts RunOptions
}

func noEnv(string) string { return "" }

func newHarness(t *testing.T, protocol string) *harness {
	t.Helper()
	keyring.MockInit()
	h := &harness{
		api: mockapi.NewAPI(t, protocol),
		ep:  mockapi.NewEndpoint(t, protocol, mockapi.FakeEndpointKey),
		env: Env{Store: keychain.NewOS(), Getenv: noEnv, Version: "test"},
		cfg: filepath.Join(t.TempDir(), "tokenrank-agent", "config.json"),
	}
	h.opts = RunOptions{ConfigPath: h.cfg, Timeout: 5 * time.Second}
	_, err := Register(context.Background(), h.env, RegisterOptions{
		ConfigPath:  h.cfg,
		APIURL:      h.api.URL,
		BaseURL:     h.ep.BaseURL + "/",
		Model:       mockapi.DummyModel,
		EndpointKey: mockapi.FakeEndpointKey,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	return h
}

func (h *harness) runOnce(t *testing.T) (Summary, error) {
	t.Helper()
	return RunOnce(context.Background(), h.env, h.opts)
}

func decodeUpload(t *testing.T, raw []byte) upload.Payload {
	t.Helper()
	var p upload.Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode upload: %v", err)
	}
	return p
}

func TestRegisterStoresSecretsOnlyInKeychain(t *testing.T) {
	h := newHarness(t, "openai")

	key, err := keyring.Get(keychain.Service, keychain.EndpointKeyAccount(mockapi.FakeInstallID))
	if err != nil || key != mockapi.FakeEndpointKey {
		t.Fatalf("endpoint key in keychain = %q, %v", key, err)
	}
	tok, err := keyring.Get(keychain.Service, keychain.InstallTokenAccount(mockapi.FakeInstallID))
	if err != nil || tok != mockapi.FakeInstallToken {
		t.Fatalf("install token in keychain = %q, %v", tok, err)
	}

	raw, err := os.ReadFile(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{mockapi.FakeEndpointKey, mockapi.FakeInstallToken} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("config file contains a secret: %s", raw)
		}
	}
	cfg, err := config.Load(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != h.ep.BaseURL || cfg.Protocol != "openai" || cfg.InstallID != mockapi.FakeInstallID || cfg.ScheduleMinutes != 15 {
		t.Fatalf("unexpected config %+v", cfg)
	}

	bodies := h.api.RegisterBodies()
	if len(bodies) != 1 {
		t.Fatalf("register calls = %d", len(bodies))
	}
	if strings.Contains(string(bodies[0]), mockapi.FakeEndpointKey) {
		t.Fatal("endpoint key was sent to the cloud at register")
	}
	var reg map[string]any
	_ = json.Unmarshal(bodies[0], &reg)
	for _, f := range []string{"base_url", "model", "agent_version", "platform"} {
		if _, ok := reg[f]; !ok {
			t.Errorf("register body missing %s", f)
		}
	}
	if len(reg) != 4 {
		t.Errorf("register body has unexpected fields: %v", reg)
	}
}

func TestRegisterFailsWithoutKeychainOrEnv(t *testing.T) {
	keyring.MockInitWithError(errors.New("no secret service"))
	api := mockapi.NewAPI(t, "openai")
	cfg := filepath.Join(t.TempDir(), "config.json")
	_, err := Register(context.Background(), Env{Store: keychain.NewOS(), Getenv: noEnv}, RegisterOptions{
		ConfigPath: cfg, APIURL: api.URL, BaseURL: "https://llm.example.com/v1", Model: mockapi.DummyModel, EndpointKey: mockapi.FakeEndpointKey,
	})
	if err == nil {
		t.Fatal("register succeeded with no keychain")
	}
	if _, statErr := os.Stat(cfg); statErr == nil {
		t.Fatal("config written despite failure")
	}
}

func TestRunOnceEndToEnd(t *testing.T) {
	for _, proto := range []string{"openai", "anthropic", "gemini", "ark"} {
		t.Run(proto, func(t *testing.T) {
			h := newHarness(t, proto)
			h.api.QueueBatch(t, h.api.NewBatch(h.ep.BaseURL))

			sum, err := h.runOnce(t)
			if err != nil {
				t.Fatalf("run once: %v", err)
			}
			if sum.NothingDue || sum.Sent != 2 || sum.Failed != 0 {
				t.Fatalf("summary %+v", sum)
			}

			reqs := h.ep.Requests()
			probes := mockapi.DummyProbes(proto)
			if len(reqs) != len(probes) {
				t.Fatalf("endpoint saw %d requests, want %d", len(reqs), len(probes))
			}
			for i, rq := range reqs {
				wantPath, wantQuery, _ := strings.Cut(probes[i].Path, "?")
				if rq.Path != wantPath || rq.RawQuery != wantQuery {
					t.Errorf("request %d path %q?%q, want %q", i, rq.Path, rq.RawQuery, probes[i].Path)
				}
				switch proto {
				case "openai", "ark":
					if rq.Header.Get("Authorization") != "Bearer "+mockapi.FakeEndpointKey {
						t.Errorf("request %d: missing bearer auth", i)
					}
				case "anthropic":
					if rq.Header.Get("X-Api-Key") != mockapi.FakeEndpointKey || rq.Header.Get("Anthropic-Version") != "2023-06-01" {
						t.Errorf("request %d: missing anthropic auth", i)
					}
				case "gemini":
					if rq.Header.Get("X-Goog-Api-Key") != mockapi.FakeEndpointKey {
						t.Errorf("request %d: missing gemini auth", i)
					}
					if strings.Contains(rq.RawQuery, "key=") {
						t.Errorf("request %d: key in URL", i)
					}
				}
			}

			ups := h.api.Uploads()
			if len(ups) != 1 {
				t.Fatalf("uploads = %d", len(ups))
			}
			if ups[0].Path != "/v1/agent/batches/batch_test_0001/results" {
				t.Errorf("upload path %q", ups[0].Path)
			}
			p := decodeUpload(t, ups[0].Body)
			if p.BatchID != "batch_test_0001" || p.InstallID != mockapi.FakeInstallID || len(p.Results) != 2 {
				t.Fatalf("payload %+v", p)
			}
			for i, r := range p.Results {
				if r.ProbeID != probes[i].ProbeID || r.HTTPStatus != 200 || r.Error != "" {
					t.Errorf("result %d: %+v", i, r)
				}
				if r.OutputText != mockapi.FakeText || r.InputTokens != mockapi.FakeInputTokens ||
					r.OutputTokens != mockapi.FakeOutputTokens || r.CacheReadTokens != mockapi.FakeCacheTokens {
					t.Errorf("result %d: output/tokens %+v", i, r)
				}
				if r.LatencyMS <= 0 {
					t.Errorf("result %d: no latency", i)
				}
				if r.Headers["server"] != mockapi.FakeServer {
					t.Errorf("result %d: headers %v", i, r.Headers)
				}
				if _, ok := r.Headers[strings.ToLower(mockapi.FakePrivateHeader)]; ok {
					t.Errorf("result %d: private header uploaded", i)
				}
			}
			if p.Results[1].TTFTMS <= 0 {
				t.Errorf("streaming result has no TTFT")
			}
			if strings.Contains(string(ups[0].Body), mockapi.FakeEndpointKey) {
				t.Fatal("upload contains the endpoint key")
			}
		})
	}
}

func TestUploadNeverContainsKeyEvenWhenEchoed(t *testing.T) {
	for _, proto := range []string{"openai", "anthropic", "gemini"} {
		t.Run(proto, func(t *testing.T) {
			h := newHarness(t, proto)
			h.ep.SetEchoKey(true)
			h.api.QueueBatch(t, h.api.NewBatch(h.ep.BaseURL))
			if _, err := h.runOnce(t); err != nil {
				t.Fatal(err)
			}
			ups := h.api.Uploads()
			if len(ups) != 1 {
				t.Fatalf("uploads = %d", len(ups))
			}
			raw := string(ups[0].Body)
			if strings.Contains(raw, mockapi.FakeEndpointKey) {
				t.Fatalf("upload contains the endpoint key: %s", raw)
			}
			if strings.Contains(raw, mockapi.FakeInstallToken) {
				t.Fatal("upload body contains the install token")
			}
			if !strings.Contains(raw, upload.Redacted) {
				t.Fatal("expected redaction marker in upload")
			}
			if strings.Contains(raw, mockapi.FakeRequestID) {
				t.Fatal("request id uploaded in clear")
			}
			sum := sha256.Sum256([]byte(mockapi.FakeRequestID))
			want := hex.EncodeToString(sum[:])
			p := decodeUpload(t, ups[0].Body)
			for i, r := range p.Results {
				if r.RequestIDSHA256 != want {
					t.Errorf("result %d request_id_sha256 = %q, want %q", i, r.RequestIDSHA256, want)
				}
				if r.OutputText != mockapi.FakeText+" "+upload.Redacted {
					t.Errorf("result %d output %q", i, r.OutputText)
				}
				if r.Headers["via"] != "1.1 "+upload.Redacted {
					t.Errorf("result %d via %q", i, r.Headers["via"])
				}
				if _, ok := r.Headers["x-echo-key"]; ok {
					t.Errorf("result %d uploaded non-allowlisted echo header", i)
				}
			}
		})
	}
}

func TestNothingDue(t *testing.T) {
	h := newHarness(t, "openai")
	sum, err := h.runOnce(t)
	if err != nil {
		t.Fatal(err)
	}
	if !sum.NothingDue {
		t.Fatalf("summary %+v", sum)
	}
	if h.api.NextCalls() != 1 {
		t.Fatalf("next calls = %d", h.api.NextCalls())
	}
	if n := len(h.ep.Requests()); n != 0 {
		t.Fatalf("endpoint saw %d requests on 204", n)
	}
	if n := len(h.api.Uploads()); n != 0 {
		t.Fatalf("uploads = %d on 204", n)
	}
}

func TestRejectedBatchesSendNothing(t *testing.T) {
	cases := map[string]func(h *harness, b *mockapi.Batch) mockapi.Envelope{
		"tampered payload": func(h *harness, b *mockapi.Batch) mockapi.Envelope {
			raw, _ := json.Marshal(b)
			env := h.api.Sign(raw)
			tampered := strings.Replace(string(raw), "Reply with OK", "Reply with XX", 1)
			env.Payload = base64.StdEncoding.EncodeToString([]byte(tampered))
			return env
		},
		"wrong key id": func(h *harness, b *mockapi.Batch) mockapi.Envelope {
			raw, _ := json.Marshal(b)
			env := h.api.Sign(raw)
			env.KeyID = "unknown-key"
			return env
		},
		"wrong install id": func(h *harness, b *mockapi.Batch) mockapi.Envelope {
			b.InstallID = "inst_someone_else"
			raw, _ := json.Marshal(b)
			return h.api.Sign(raw)
		},
		"wrong endpoint": func(h *harness, b *mockapi.Batch) mockapi.Envelope {
			b.Endpoint = "https://other-endpoint.example/v1"
			raw, _ := json.Marshal(b)
			return h.api.Sign(raw)
		},
		"wrong protocol": func(h *harness, b *mockapi.Batch) mockapi.Envelope {
			b.Protocol = "gemini"
			raw, _ := json.Marshal(b)
			return h.api.Sign(raw)
		},
		"expired": func(h *harness, b *mockapi.Batch) mockapi.Envelope {
			now := time.Now().UTC()
			b.IssuedAt = now.Add(-20 * time.Minute)
			b.ExpiresAt = now.Add(-5 * time.Minute)
			raw, _ := json.Marshal(b)
			return h.api.Sign(raw)
		},
		"window over 30 minutes": func(h *harness, b *mockapi.Batch) mockapi.Envelope {
			b.ExpiresAt = b.IssuedAt.Add(31 * time.Minute)
			raw, _ := json.Marshal(b)
			return h.api.Sign(raw)
		},
		"absolute probe path": func(h *harness, b *mockapi.Batch) mockapi.Envelope {
			b.Probes[1].Path = "http://" + strings.TrimPrefix(h.ep.BaseURL, "http://") + "/chat/completions"
			raw, _ := json.Marshal(b)
			return h.api.Sign(raw)
		},
		"dot-dot probe path": func(h *harness, b *mockapi.Batch) mockapi.Envelope {
			b.Probes[0].Path = "/../../chat/completions"
			raw, _ := json.Marshal(b)
			return h.api.Sign(raw)
		},
		"protocol-relative probe path": func(h *harness, b *mockapi.Batch) mockapi.Envelope {
			b.Probes[0].Path = "//evil.example/chat/completions"
			raw, _ := json.Marshal(b)
			return h.api.Sign(raw)
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "openai")
			b := h.api.NewBatch(h.ep.BaseURL)
			h.api.QueueEnvelope(mk(h, &b))
			_, err := h.runOnce(t)
			if !errors.Is(err, batch.ErrRejected) {
				t.Fatalf("err = %v, want ErrRejected", err)
			}
			if n := len(h.ep.Requests()); n != 0 {
				t.Fatalf("endpoint received %d requests from a rejected batch", n)
			}
			if n := len(h.api.Uploads()); n != 0 {
				t.Fatalf("uploads = %d for a rejected batch", n)
			}
		})
	}
}

func TestRunOnceUsesEnvFallbackKey(t *testing.T) {
	h := newHarness(t, "openai")
	// Remove the key from the keychain; supply it via the environment instead.
	if err := h.env.Store.Delete(keychain.EndpointKeyAccount(mockapi.FakeInstallID)); err != nil {
		t.Fatal(err)
	}
	h.env.Getenv = func(k string) string {
		if k == keychain.EnvEndpointKey {
			return mockapi.FakeEndpointKey
		}
		return ""
	}
	h.api.QueueBatch(t, h.api.NewBatch(h.ep.BaseURL))
	sum, err := h.runOnce(t)
	if err != nil || sum.Sent != 2 {
		t.Fatalf("sum %+v err %v", sum, err)
	}
}

func TestRunOnceNotRegistered(t *testing.T) {
	keyring.MockInit()
	_, err := RunOnce(context.Background(), Env{Store: keychain.NewOS()}, RunOptions{ConfigPath: filepath.Join(t.TempDir(), "none.json")})
	if !errors.Is(err, config.ErrNotRegistered) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	h := newHarness(t, "openai")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, h.env, h.opts) }()
	deadline := time.Now().Add(5 * time.Second)
	for h.api.NextCalls() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}

func TestNextWait(t *testing.T) {
	if w := nextWait(15, 0); w < 15*time.Minute || w > 17*time.Minute {
		t.Fatalf("nextWait(15,0) = %v", w)
	}
	if w := nextWait(15, 10); w < 120*time.Minute || w > 133*time.Minute {
		t.Fatalf("nextWait(15,10) = %v", w)
	}
	if w := nextWait(1, 0); w < 5*time.Minute {
		t.Fatalf("schedule floor not applied: %v", w)
	}
}

func TestValidateKey(t *testing.T) {
	if ValidateKey("sk-fake-ok") != nil {
		t.Fatal("valid key rejected")
	}
	for _, k := range []string{"", "a b", "a\nb", strings.Repeat("a", 5000)} {
		if ValidateKey(k) == nil {
			t.Errorf("ValidateKey(%q) accepted", k)
		}
	}
}

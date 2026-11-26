package batch

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	testBase    = "https://llm.example.com/v1"
	testInstall = "inst_test_0001"
)

type fixture struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	now  time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{pub: pub, priv: priv, now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func (f *fixture) batch() map[string]any {
	return map[string]any{
		"batch_id":   "b_1",
		"install_id": testInstall,
		"endpoint":   testBase + "/",
		"protocol":   "openai",
		"model":      "dummy-model",
		"issued_at":  f.now.Add(-time.Minute).Format(time.RFC3339),
		"expires_at": f.now.Add(9 * time.Minute).Format(time.RFC3339),
		"probes": []any{map[string]any{
			"probe_id": "p_1",
			"method":   "POST",
			"path":     "/chat/completions",
			"body":     map[string]any{"model": "dummy-model", "messages": []any{map[string]any{"role": "user", "content": "Reply with OK"}}},
		}},
	}
}

func (f *fixture) sign(t *testing.T, b map[string]any) *Envelope {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return &Envelope{
		KeyID:     "k1",
		Payload:   base64.StdEncoding.EncodeToString(raw),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(f.priv, raw)),
	}
}

func (f *fixture) expect() Expect {
	return Expect{
		InstallID: testInstall,
		BaseURL:   testBase,
		Protocol:  "openai",
		Model:     "dummy-model",
		Keys:      map[string]ed25519.PublicKey{"k1": f.pub},
		Now:       f.now,
	}
}

func TestVerifyAccepts(t *testing.T) {
	f := newFixture(t)
	b, err := Verify(f.sign(t, f.batch()), f.expect())
	if err != nil {
		t.Fatal(err)
	}
	if b.BatchID != "b_1" || len(b.Probes) != 1 || b.Probes[0].Path != "/chat/completions" {
		t.Fatalf("unexpected batch: %+v", b)
	}
}

func TestVerifyAcceptsWithinClockSkew(t *testing.T) {
	f := newFixture(t)
	b := f.batch()
	b["expires_at"] = f.now.Add(-time.Minute).Format(time.RFC3339)
	b["issued_at"] = f.now.Add(-10 * time.Minute).Format(time.RFC3339)
	if _, err := Verify(f.sign(t, b), f.expect()); err != nil {
		t.Fatalf("batch expired 1 minute ago should pass with 2 minute skew: %v", err)
	}
}

func TestVerifyRejects(t *testing.T) {
	f := newFixture(t)
	other, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	_ = other

	cases := map[string]func() (*Envelope, Expect){
		"tampered payload": func() (*Envelope, Expect) {
			env := f.sign(t, f.batch())
			raw, _ := base64.StdEncoding.DecodeString(env.Payload)
			raw = []byte(strings.Replace(string(raw), "Reply with OK", "Reply with NO", 1))
			env.Payload = base64.StdEncoding.EncodeToString(raw)
			return env, f.expect()
		},
		"signed by another key": func() (*Envelope, Expect) {
			raw, _ := json.Marshal(f.batch())
			env := &Envelope{KeyID: "k1", Payload: base64.StdEncoding.EncodeToString(raw),
				Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(otherPriv, raw))}
			return env, f.expect()
		},
		"unknown key id": func() (*Envelope, Expect) {
			env := f.sign(t, f.batch())
			env.KeyID = "k2"
			return env, f.expect()
		},
		"bad signature encoding": func() (*Envelope, Expect) {
			env := f.sign(t, f.batch())
			env.Signature = "not base64!"
			return env, f.expect()
		},
		"wrong install id": func() (*Envelope, Expect) {
			b := f.batch()
			b["install_id"] = "inst_other"
			return f.sign(t, b), f.expect()
		},
		"wrong endpoint": func() (*Envelope, Expect) {
			b := f.batch()
			b["endpoint"] = "https://evil.example/v1"
			return f.sign(t, b), f.expect()
		},
		"wrong protocol": func() (*Envelope, Expect) {
			b := f.batch()
			b["protocol"] = "anthropic"
			return f.sign(t, b), f.expect()
		},
		"wrong model": func() (*Envelope, Expect) {
			b := f.batch()
			b["model"] = "other-model"
			return f.sign(t, b), f.expect()
		},
		"expired": func() (*Envelope, Expect) {
			b := f.batch()
			b["issued_at"] = f.now.Add(-20 * time.Minute).Format(time.RFC3339)
			b["expires_at"] = f.now.Add(-3 * time.Minute).Format(time.RFC3339)
			return f.sign(t, b), f.expect()
		},
		"window over 30 minutes": func() (*Envelope, Expect) {
			b := f.batch()
			b["issued_at"] = f.now.Add(-time.Minute).Format(time.RFC3339)
			b["expires_at"] = f.now.Add(30 * time.Minute).Format(time.RFC3339)
			return f.sign(t, b), f.expect()
		},
		"issued in the future": func() (*Envelope, Expect) {
			b := f.batch()
			b["issued_at"] = f.now.Add(5 * time.Minute).Format(time.RFC3339)
			b["expires_at"] = f.now.Add(10 * time.Minute).Format(time.RFC3339)
			return f.sign(t, b), f.expect()
		},
		"absolute probe path": func() (*Envelope, Expect) {
			b := f.batch()
			b["probes"].([]any)[0].(map[string]any)["path"] = "https://evil.example/steal"
			return f.sign(t, b), f.expect()
		},
		"GET method": func() (*Envelope, Expect) {
			b := f.batch()
			b["probes"].([]any)[0].(map[string]any)["method"] = "GET"
			return f.sign(t, b), f.expect()
		},
		"non-object body": func() (*Envelope, Expect) {
			b := f.batch()
			b["probes"].([]any)[0].(map[string]any)["body"] = "hello"
			return f.sign(t, b), f.expect()
		},
		"no probes": func() (*Envelope, Expect) {
			b := f.batch()
			b["probes"] = []any{}
			return f.sign(t, b), f.expect()
		},
		"bad batch id": func() (*Envelope, Expect) {
			b := f.batch()
			b["batch_id"] = "../../admin"
			return f.sign(t, b), f.expect()
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			env, exp := mk()
			_, err := Verify(env, exp)
			if !errors.Is(err, ErrRejected) {
				t.Fatalf("Verify err = %v, want ErrRejected", err)
			}
		})
	}
}

func TestValidatePath(t *testing.T) {
	good := []string{"/chat/completions", "/messages", "/models/gemini-dummy:generateContent", "/models/x:streamGenerateContent?alt=sse", "/responses"}
	for _, p := range good {
		if err := ValidatePath(p); err != nil {
			t.Errorf("ValidatePath(%q) = %v", p, err)
		}
	}
	bad := []string{
		"", "chat/completions", "//evil.example/x", "https://evil.example/x", "/../admin", "/a/../../b",
		"/a/./b", "/%2e%2e/x", "/a%2fb", `/a\b`, "/a#frag", "/a b", "/a\nb", "/x/http://evil.example",
		"/é",
	}
	for _, p := range bad {
		if err := ValidatePath(p); err == nil {
			t.Errorf("ValidatePath(%q) accepted", p)
		}
	}
}

func TestJoinURL(t *testing.T) {
	cases := map[[2]string]string{
		{"https://llm.example.com/v1", "/chat/completions"}:                      "https://llm.example.com/v1/chat/completions",
		{"https://llm.example.com", "/v1/messages"}:                              "https://llm.example.com/v1/messages",
		{"http://127.0.0.1:9/v1beta", "/models/m:streamGenerateContent?alt=sse"}: "http://127.0.0.1:9/v1beta/models/m:streamGenerateContent?alt=sse",
	}
	for in, want := range cases {
		u, err := JoinURL(in[0], in[1])
		if err != nil {
			t.Fatalf("JoinURL(%q, %q): %v", in[0], in[1], err)
		}
		if u.String() != want {
			t.Errorf("JoinURL(%q, %q) = %q, want %q", in[0], in[1], u.String(), want)
		}
	}
	if _, err := JoinURL(testBase, "//evil.example/x"); err == nil {
		t.Fatal("JoinURL accepted a protocol-relative path")
	}
}

func TestParseKeys(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keys, err := ParseKeys(map[string]string{"k1": base64.StdEncoding.EncodeToString(pub)})
	if err != nil || len(keys["k1"]) != ed25519.PublicKeySize {
		t.Fatalf("ParseKeys: %v", err)
	}
	if _, err := ParseKeys(map[string]string{"k1": base64.StdEncoding.EncodeToString([]byte("short"))}); err == nil {
		t.Fatal("short key accepted")
	}
	if _, err := ParseKeys(nil); err == nil {
		t.Fatal("empty key set accepted")
	}
}

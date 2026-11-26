package upload

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"github.com/spencerlys/tokenrank-agent/internal/sender"
)

// Obviously fake values.
const (
	fakeKey   = "sk-fake-upload-test-key-0000"
	fakeToken = "fake-install-token-upload-0000"
)

func TestBuildRedactsAndFilters(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Server", "nginx")
	h.Set("Via", "1.1 proxy "+fakeKey)
	h.Set("X-Request-Id", "req-abc")
	h.Set("Set-Cookie", "session=secret")
	h.Set("X-Echo", fakeKey)
	rs := []sender.Result{{
		ProbeID:    "p1",
		HTTPStatus: 200,
		OutputText: "you sent " + fakeKey + " twice: " + fakeKey,
		Header:     h,
		Error:      "upstream said Bearer " + fakeKey,
	}}
	p := Build("b1", "i1", rs, fakeKey, fakeToken, "")
	r := p.Results[0]
	if strings.Contains(r.OutputText, fakeKey) || strings.Count(r.OutputText, Redacted) != 2 {
		t.Fatalf("output not redacted: %q", r.OutputText)
	}
	if strings.Contains(r.Error, fakeKey) {
		t.Fatalf("error not redacted: %q", r.Error)
	}
	if r.Headers["via"] != "1.1 proxy "+Redacted {
		t.Fatalf("via = %q", r.Headers["via"])
	}
	for _, banned := range []string{"set-cookie", "x-echo", "x-request-id"} {
		if _, ok := r.Headers[banned]; ok {
			t.Errorf("header %s uploaded", banned)
		}
	}
	if r.Headers["server"] != "nginx" || r.Headers["content-type"] != "application/json" {
		t.Errorf("allowlisted headers missing: %v", r.Headers)
	}
	sum := sha256.Sum256([]byte("req-abc"))
	if r.RequestIDSHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("request id hash = %q", r.RequestIDSHA256)
	}
	b, err := Encode(p, fakeKey, fakeToken)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), fakeKey) || strings.Contains(string(b), "req-abc") {
		t.Fatalf("encoded payload leaks: %s", b)
	}
}

func TestRequestIDPriority(t *testing.T) {
	h := http.Header{}
	h.Set("cf-ray", "ray-1")
	h.Set("request-id", "anthropic-1")
	sum := sha256.Sum256([]byte("anthropic-1"))
	if got := requestIDHash(h); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash = %q", got)
	}
	if requestIDHash(http.Header{}) != "" {
		t.Fatal("empty headers should give empty hash")
	}
}

func TestEncodeRefusesSecret(t *testing.T) {
	p := Payload{BatchID: "b", InstallID: "i", Results: []Result{{ProbeID: "p", OutputText: fakeKey}}}
	if _, err := Encode(p, fakeKey); err == nil {
		t.Fatal("Encode accepted a payload containing the key")
	}
	// A secret that JSON escapes differently is still caught.
	weird := "sk-fake<&>key"
	p.Results[0].OutputText = weird
	if _, err := Encode(p, weird); err == nil {
		t.Fatal("Encode accepted a payload containing an escaped secret")
	}
}

func TestHeaderValueCapped(t *testing.T) {
	h := http.Header{}
	h.Set("Server", strings.Repeat("x", 2000))
	p := Build("b", "i", []sender.Result{{ProbeID: "p", Header: h}})
	if len(p.Results[0].Headers["server"]) != maxHeaderValue {
		t.Fatalf("len = %d", len(p.Results[0].Headers["server"]))
	}
}

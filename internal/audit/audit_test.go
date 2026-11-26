package audit

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestValidateListen(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:8787", "localhost:8787", "[::1]:8787", "127.0.0.2:1"} {
		if err := ValidateListen(ok); err != nil {
			t.Errorf("ValidateListen(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"0.0.0.0:8787", ":8787", "[::]:8787", "192.168.1.10:8787", "example.com:8787", "127.0.0.1", "127.0.0.1:"} {
		if err := ValidateListen(bad); err == nil {
			t.Errorf("ValidateListen(%q) accepted", bad)
		}
	}
}

func TestServeRefusesAllInterfaces(t *testing.T) {
	var out bytes.Buffer
	if err := Serve("0.0.0.0:8787", &out); !errors.Is(err, ErrNotLoopback) {
		t.Fatalf("Serve(0.0.0.0) = %v, want ErrNotLoopback", err)
	}
	if out.Len() != 0 {
		t.Fatalf("unexpected output %q", out.String())
	}
}

func TestServeStub(t *testing.T) {
	var out bytes.Buffer
	if err := Serve("127.0.0.1:8787", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "not implemented yet") {
		t.Fatalf("output %q", out.String())
	}
}

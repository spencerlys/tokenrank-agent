package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/spencerlys/tokenrank-agent/internal/keychain"
	"github.com/spencerlys/tokenrank-agent/internal/mockapi"
)

func runCLI(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := cli(context.Background(), args, strings.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestRegisterAndRunOnceViaCLI(t *testing.T) {
	keyring.MockInit()
	t.Setenv(keychain.EnvEndpointKey, "")
	t.Setenv(EnvAPIURL, "")
	api := mockapi.NewAPI(t, "openai")
	ep := mockapi.NewEndpoint(t, "openai", mockapi.FakeEndpointKey)
	cfg := filepath.Join(t.TempDir(), "tokenrank-agent", "config.json")

	code, out, errOut := runCLI(t, mockapi.FakeEndpointKey+"\n",
		"register", "--api", api.URL, "--base-url", ep.BaseURL, "--model", mockapi.DummyModel, "--key-stdin", "--config", cfg)
	if code != 0 {
		t.Fatalf("register exit %d: %s %s", code, out, errOut)
	}
	if strings.Contains(out+errOut, mockapi.FakeEndpointKey) || strings.Contains(out+errOut, mockapi.FakeInstallToken) {
		t.Fatal("register printed a secret")
	}

	// Secrets are in the keychain ...
	if k, err := keyring.Get(keychain.Service, keychain.EndpointKeyAccount(mockapi.FakeInstallID)); err != nil || k != mockapi.FakeEndpointKey {
		t.Fatalf("endpoint key not in keychain: %q %v", k, err)
	}
	if tok, err := keyring.Get(keychain.Service, keychain.InstallTokenAccount(mockapi.FakeInstallID)); err != nil || tok != mockapi.FakeInstallToken {
		t.Fatalf("install token not in keychain: %q %v", tok, err)
	}
	// ... and not in the config file.
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(mockapi.FakeEndpointKey)) || bytes.Contains(raw, []byte(mockapi.FakeInstallToken)) {
		t.Fatalf("config contains a secret: %s", raw)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"api_url", "install_id", "base_url", "protocol", "model", "schedule_minutes", "batch_keys"} {
		if _, ok := m[f]; !ok {
			t.Errorf("config missing %s", f)
		}
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(cfg)
		if st.Mode().Perm() != 0o600 {
			t.Errorf("config mode %v", st.Mode().Perm())
		}
	}

	// Nothing due.
	code, out, errOut = runCLI(t, "", "run", "--once", "--config", cfg, "--jitter-min", "0", "--jitter-max", "0")
	if code != 0 || !strings.Contains(out, "Nothing due") {
		t.Fatalf("run --once (204) exit %d: %s %s", code, out, errOut)
	}
	if len(ep.Requests()) != 0 {
		t.Fatal("requests sent on 204")
	}

	// One batch.
	api.QueueBatch(t, api.NewBatch(ep.BaseURL))
	code, out, errOut = runCLI(t, "", "run", "--once", "--config", cfg, "--jitter-min", "0", "--jitter-max", "0")
	if code != 0 {
		t.Fatalf("run --once exit %d: %s %s", code, out, errOut)
	}
	if len(ep.Requests()) != 2 || len(api.Uploads()) != 1 {
		t.Fatalf("requests=%d uploads=%d", len(ep.Requests()), len(api.Uploads()))
	}
	if strings.Contains(out+errOut, mockapi.FakeEndpointKey) {
		t.Fatal("run printed the key")
	}
}

func TestRegisterRequiresFlags(t *testing.T) {
	code, _, _ := runCLI(t, "", "register", "--model", "m")
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

func TestRegisterNoKeyAvailable(t *testing.T) {
	t.Setenv(keychain.EnvEndpointKey, "")
	code, _, errOut := runCLI(t, "", "register", "--base-url", "https://llm.example.com/v1", "--model", "m", "--config", filepath.Join(t.TempDir(), "c.json"))
	if code != 1 || !strings.Contains(errOut, "no endpoint key") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}

func TestRunNotRegistered(t *testing.T) {
	keyring.MockInit()
	code, _, errOut := runCLI(t, "", "run", "--once", "--config", filepath.Join(t.TempDir(), "none.json"))
	if code != 1 || !strings.Contains(errOut, "not registered") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}

func TestAuditRefusesNonLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8787", ":8787", "[::]:8787", "10.0.0.5:8787"} {
		code, out, errOut := runCLI(t, "", "audit", "--listen", addr)
		if code == 0 || strings.Contains(out, "not implemented") || !strings.Contains(errOut, "loopback") {
			t.Errorf("audit --listen %s: exit %d out %q err %q", addr, code, out, errOut)
		}
	}
	code, out, _ := runCLI(t, "", "audit", "--listen", "127.0.0.1:8787")
	if code != 0 || !strings.Contains(out, "not implemented yet") {
		t.Fatalf("audit stub: exit %d out %q", code, out)
	}
}

func TestVersionAndUnknown(t *testing.T) {
	code, out, _ := runCLI(t, "", "version")
	if code != 0 || !strings.HasPrefix(out, "tokenrank-agent dev") {
		t.Fatalf("version: %d %q", code, out)
	}
	if code, _, _ := runCLI(t, "", "bogus"); code != 2 {
		t.Fatalf("unknown command exit %d", code)
	}
	if code, _, _ := runCLI(t, ""); code != 2 {
		t.Fatalf("no args exit %d", code)
	}
}

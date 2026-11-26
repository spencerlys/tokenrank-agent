package keychain

import (
	"errors"
	"testing"

	"github.com/zalando/go-keyring"
)

// Obviously fake values; never real credentials.
const (
	fakeKey   = "sk-fake-keychain-test-0000"
	fakeEnv   = "sk-fake-env-fallback-0000"
	installID = "inst_test_0001"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestStoreRoundTrip(t *testing.T) {
	keyring.MockInit()
	s := NewOS()
	if _, err := s.Get(EndpointKeyAccount(installID)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get on empty store: %v, want ErrNotFound", err)
	}
	if err := s.Set(EndpointKeyAccount(installID), fakeKey); err != nil {
		t.Fatal(err)
	}
	got, err := EndpointKey(s, installID, envMap(nil))
	if err != nil || got != fakeKey {
		t.Fatalf("EndpointKey = %q, %v", got, err)
	}
	if err := s.Delete(EndpointKeyAccount(installID)); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(EndpointKeyAccount(installID)); err != nil {
		t.Fatalf("second delete should be a no-op: %v", err)
	}
}

func TestEndpointKeyPrefersKeychain(t *testing.T) {
	keyring.MockInit()
	s := NewOS()
	if err := s.Set(EndpointKeyAccount(installID), fakeKey); err != nil {
		t.Fatal(err)
	}
	got, err := EndpointKey(s, installID, envMap(map[string]string{EnvEndpointKey: fakeEnv}))
	if err != nil || got != fakeKey {
		t.Fatalf("EndpointKey = %q, %v; want keychain value", got, err)
	}
}

func TestEndpointKeyEnvFallback(t *testing.T) {
	keyring.MockInit()
	got, err := EndpointKey(NewOS(), installID, envMap(map[string]string{EnvEndpointKey: "  " + fakeEnv + "\n"}))
	if err != nil || got != fakeEnv {
		t.Fatalf("EndpointKey = %q, %v; want env value", got, err)
	}
}

func TestEndpointKeyEnvFallbackWhenKeychainBroken(t *testing.T) {
	keyring.MockInitWithError(errors.New("no secret service"))
	got, err := EndpointKey(NewOS(), installID, envMap(map[string]string{EnvEndpointKey: fakeEnv}))
	if err != nil || got != fakeEnv {
		t.Fatalf("EndpointKey = %q, %v; want env value", got, err)
	}
	_, err = EndpointKey(NewOS(), installID, envMap(nil))
	if err == nil {
		t.Fatal("expected an error with a broken keychain and no env var")
	}
}

func TestEndpointKeyMissing(t *testing.T) {
	keyring.MockInit()
	if _, err := EndpointKey(NewOS(), installID, envMap(nil)); err == nil {
		t.Fatal("expected an error when no key is available")
	}
}

func TestEndpointKeyRealEnv(t *testing.T) {
	keyring.MockInit()
	t.Setenv(EnvEndpointKey, fakeEnv)
	got, err := EndpointKey(NewOS(), installID, nil)
	if err != nil || got != fakeEnv {
		t.Fatalf("EndpointKey = %q, %v", got, err)
	}
}

func TestInstallToken(t *testing.T) {
	keyring.MockInit()
	s := NewOS()
	if _, err := InstallToken(s, installID); err == nil {
		t.Fatal("expected error for missing token")
	}
	if err := s.Set(InstallTokenAccount(installID), "fake-token"); err != nil {
		t.Fatal(err)
	}
	if got, err := InstallToken(s, installID); err != nil || got != "fake-token" {
		t.Fatalf("InstallToken = %q, %v", got, err)
	}
}

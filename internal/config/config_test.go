package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func validConfig() *Config {
	return &Config{
		APIURL:          "https://api.tokenrank.example",
		InstallID:       "inst_test_0001",
		BaseURL:         "https://llm.example.com/v1",
		Protocol:        "openai",
		Model:           "dummy-model",
		ScheduleMinutes: 15,
		BatchKeys:       map[string]string{"k1": "AAAA"},
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", FileName)
	c := validConfig()
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.InstallID != c.InstallID || got.BaseURL != c.BaseURL || got.BatchKeys["k1"] != "AAAA" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("config mode = %v, want 0600", st.Mode().Perm())
		}
	}
}

func TestLoadMissing(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("err = %v, want ErrNotRegistered", err)
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	ok := map[string]string{
		"https://LLM.example.com/v1/":  "https://llm.example.com/v1",
		"https://llm.example.com":      "https://llm.example.com",
		" https://llm.example.com/v1 ": "https://llm.example.com/v1",
		"http://127.0.0.1:8080/api/v3": "http://127.0.0.1:8080/api/v3",
		"HTTPS://llm.example.com/v1//": "https://llm.example.com/v1",
	}
	for in, want := range ok {
		got, err := NormalizeBaseURL(in)
		if err != nil || got != want {
			t.Errorf("NormalizeBaseURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{"", "llm.example.com", "ftp://x", "https://u:p@x.example", "https://x.example/v1?a=b", "https://x.example/#f", "https://x.example/a%2Fb"}
	for _, in := range bad {
		if _, err := NormalizeBaseURL(in); err == nil {
			t.Errorf("NormalizeBaseURL(%q) accepted", in)
		}
	}
}

func TestNormalizeAPIURLRequiresHTTPS(t *testing.T) {
	if _, err := NormalizeAPIURL("http://api.tokenrank.example"); err == nil {
		t.Fatal("plain http to a remote host accepted")
	}
	for _, ok := range []string{"http://127.0.0.1:9999", "http://localhost:1", "https://api.tokenrank.example/"} {
		if _, err := NormalizeAPIURL(ok); err != nil {
			t.Errorf("NormalizeAPIURL(%q): %v", ok, err)
		}
	}
}

func TestClampSchedule(t *testing.T) {
	cases := map[int]int{0: 15, -1: 15, 1: MinScheduleMinutes, 15: 15, 100000: MaxScheduleMinutes}
	for in, want := range cases {
		if got := ClampSchedule(in); got != want {
			t.Errorf("ClampSchedule(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestValidID(t *testing.T) {
	for _, s := range []string{"a", "inst_1", "b-2.x"} {
		if !ValidID(s) {
			t.Errorf("ValidID(%q) = false", s)
		}
	}
	for _, s := range []string{"", "a/b", "../x", "a b", "a?b"} {
		if ValidID(s) {
			t.Errorf("ValidID(%q) = true", s)
		}
	}
}

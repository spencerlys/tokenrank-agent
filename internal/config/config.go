// Package config reads and writes the agent's local configuration file.
//
// The configuration file never contains the endpoint API key or the install
// token. Both live in the OS keychain (see package keychain).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const (
	// DirName is the directory under os.UserConfigDir that holds the config.
	DirName = "tokenrank-agent"
	// FileName is the config file name.
	FileName = "config.json"
	// DefaultAPIURL is a placeholder. Override it with --api or TOKENRANK_API_URL.
	DefaultAPIURL = "https://api.tokenrank.example"
	// DefaultScheduleMinutes is used when the cloud does not send a schedule.
	DefaultScheduleMinutes = 15
	// MinScheduleMinutes is a local floor so the agent never polls (and never
	// sends to the user's endpoint) more often than this, whatever the cloud says.
	MinScheduleMinutes = 5
	// MaxScheduleMinutes caps the polling interval at one day.
	MaxScheduleMinutes = 24 * 60
)

// ErrNotRegistered is returned by Load when no config file exists yet.
var ErrNotRegistered = errors.New("agent is not registered yet: run `tokenrank-agent register` first")

// Config is the on-disk configuration. It must never hold secrets.
type Config struct {
	APIURL          string            `json:"api_url"`
	InstallID       string            `json:"install_id"`
	BaseURL         string            `json:"base_url"`
	Protocol        string            `json:"protocol"`
	Model           string            `json:"model"`
	ScheduleMinutes int               `json:"schedule_minutes"`
	BatchKeys       map[string]string `json:"batch_keys"`
}

// Protocols the agent knows how to speak.
var protocols = map[string]bool{"openai": true, "anthropic": true, "gemini": true, "ark": true}

// ValidProtocol reports whether p is a supported protocol name.
func ValidProtocol(p string) bool { return protocols[p] }

// DefaultPath returns os.UserConfigDir()/tokenrank-agent/config.json.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return filepath.Join(dir, DirName, FileName), nil
}

// Load reads and validates the config at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotRegistered
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &c, nil
}

// Save writes c to path atomically with mode 0600 (directory 0700).
func Save(path string, c *Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("chmod config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

// Validate checks that every field is present and well formed.
func (c *Config) Validate() error {
	if _, err := NormalizeAPIURL(c.APIURL); err != nil {
		return fmt.Errorf("api_url: %w", err)
	}
	if !ValidID(c.InstallID) {
		return errors.New("install_id is missing or malformed")
	}
	norm, err := NormalizeBaseURL(c.BaseURL)
	if err != nil {
		return fmt.Errorf("base_url: %w", err)
	}
	if norm != c.BaseURL {
		return errors.New("base_url is not normalised")
	}
	if !ValidProtocol(c.Protocol) {
		return fmt.Errorf("unsupported protocol %q", c.Protocol)
	}
	if err := ValidateModel(c.Model); err != nil {
		return err
	}
	if c.ScheduleMinutes < MinScheduleMinutes || c.ScheduleMinutes > MaxScheduleMinutes {
		return fmt.Errorf("schedule_minutes must be between %d and %d", MinScheduleMinutes, MaxScheduleMinutes)
	}
	if len(c.BatchKeys) == 0 {
		return errors.New("batch_keys is empty")
	}
	return nil
}

// ClampSchedule applies the default, floor and cap to a schedule from the cloud.
func ClampSchedule(m int) int {
	switch {
	case m <= 0:
		return DefaultScheduleMinutes
	case m < MinScheduleMinutes:
		return MinScheduleMinutes
	case m > MaxScheduleMinutes:
		return MaxScheduleMinutes
	}
	return m
}

// ValidID reports whether s is a safe identifier (install or batch id):
// 1-128 characters from [A-Za-z0-9._-].
func ValidID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// ValidateModel checks a model name is present, short and printable.
func ValidateModel(m string) error {
	if m == "" {
		return errors.New("model is required")
	}
	if len(m) > 200 {
		return errors.New("model name is too long")
	}
	for _, r := range m {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return errors.New("model name must not contain spaces or control characters")
		}
	}
	return nil
}

// NormalizeBaseURL validates an endpoint base URL and returns it in canonical
// form: lower-case scheme and host, no trailing slash, no query or fragment.
func NormalizeBaseURL(raw string) (string, error) {
	u, err := parseHTTPURL(raw)
	if err != nil {
		return "", err
	}
	if strings.Contains(u.EscapedPath(), "%") {
		return "", errors.New("URL path must not contain percent-encoded characters")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

// NormalizeAPIURL validates the TokenRank API URL. It must be https, except
// for loopback hosts (used by local development and tests).
func NormalizeAPIURL(raw string) (string, error) {
	u, err := parseHTTPURL(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" && !IsLoopbackHost(u.Hostname()) {
		return "", errors.New("API URL must use https")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

func parseHTTPURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, errors.New("URL must start with https:// or http://")
	}
	if u.Host == "" {
		return nil, errors.New("URL has no host")
	}
	if u.User != nil {
		return nil, errors.New("URL must not contain credentials")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("URL must not contain a query or fragment")
	}
	u.Host = strings.ToLower(u.Host)
	return u, nil
}

// IsLoopbackHost reports whether host is "localhost" or a loopback IP.
func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

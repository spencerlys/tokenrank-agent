// Package agent ties the pieces together: register with the cloud, then on
// each cycle fetch one signed batch, verify it, send it to the user's own
// endpoint and upload the raw results.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/spencerlys/tokenrank-agent/internal/batch"
	"github.com/spencerlys/tokenrank-agent/internal/config"
	"github.com/spencerlys/tokenrank-agent/internal/keychain"
	"github.com/spencerlys/tokenrank-agent/internal/sender"
	"github.com/spencerlys/tokenrank-agent/internal/upload"
)

// Env carries dependencies so tests can substitute them.
type Env struct {
	Store          keychain.Store
	Getenv         func(string) string // nil: os.Getenv
	APIClient      *http.Client        // nil: no-redirect client with 30s timeout
	EndpointClient *http.Client        // nil: sender.NewHTTPClient()
	Now            func() time.Time    // nil: time.Now
	Log            io.Writer           // nil: io.Discard
	Version        string
}

func (e Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e Env) apiClient() *http.Client {
	if e.APIClient != nil {
		return e.APIClient
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (e Env) logger() *log.Logger {
	w := e.Log
	if w == nil {
		w = io.Discard
	}
	return log.New(w, "tokenrank-agent: ", log.LstdFlags)
}

func (e Env) userAgent() string {
	v := e.Version
	if v == "" {
		v = "dev"
	}
	return "tokenrank-agent/" + v + " (" + runtime.GOOS + "/" + runtime.GOARCH + ")"
}

// ---- register ----------------------------------------------------------------

// RegisterOptions are the inputs to Register.
type RegisterOptions struct {
	ConfigPath  string
	APIURL      string
	BaseURL     string
	Model       string
	EndpointKey string // stored in the keychain only; never sent to the cloud
}

type registerRequest struct {
	BaseURL      string `json:"base_url"`
	Model        string `json:"model"`
	AgentVersion string `json:"agent_version"`
	Platform     string `json:"platform"`
}

type registerResponse struct {
	InstallID       string            `json:"install_id"`
	InstallToken    string            `json:"install_token"`
	Protocol        string            `json:"protocol"`
	ScheduleMinutes int               `json:"schedule_minutes"`
	BatchKeys       map[string]string `json:"batch_keys"`
}

// ValidateKey rejects empty or obviously malformed endpoint keys.
func ValidateKey(k string) error {
	if k == "" {
		return errors.New("endpoint key is empty")
	}
	if len(k) > 4096 {
		return errors.New("endpoint key is too long")
	}
	for _, r := range k {
		if r <= 0x20 || r == 0x7f {
			return errors.New("endpoint key contains spaces or control characters")
		}
	}
	return nil
}

// Register registers this install with the cloud, stores the endpoint key and
// install token in the keychain and writes the config file (without secrets).
// If the endpoint key cannot be stored but TOKENRANK_ENDPOINT_KEY is set,
// registration continues and the agent will read the key from the
// environment on every run.
func Register(ctx context.Context, env Env, o RegisterOptions) (*config.Config, error) {
	lg := env.logger()
	apiURL, err := config.NormalizeAPIURL(o.APIURL)
	if err != nil {
		return nil, fmt.Errorf("api url: %w", err)
	}
	baseURL, err := config.NormalizeBaseURL(o.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("base url: %w", err)
	}
	if err := config.ValidateModel(o.Model); err != nil {
		return nil, err
	}
	key := strings.TrimSpace(o.EndpointKey)
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	if env.Store == nil {
		return nil, errors.New("no keychain store")
	}

	reqBody, err := json.Marshal(registerRequest{
		BaseURL:      baseURL,
		Model:        o.Model,
		AgentVersion: env.Version,
		Platform:     runtime.GOOS + "/" + runtime.GOARCH,
	})
	if err != nil {
		return nil, err
	}
	if bytes.Contains(reqBody, []byte(key)) {
		return nil, errors.New("refusing to register: the endpoint key appears in the base URL or model")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/v1/agent/register", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", env.userAgent())
	resp, err := env.apiClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("register: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	var rr registerResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rr); err != nil {
		return nil, fmt.Errorf("register: decode response: %w", err)
	}
	if !config.ValidID(rr.InstallID) {
		return nil, errors.New("register: malformed install_id in response")
	}
	if rr.InstallToken == "" {
		return nil, errors.New("register: empty install_token in response")
	}
	if !config.ValidProtocol(rr.Protocol) {
		return nil, fmt.Errorf("register: unsupported protocol %q", rr.Protocol)
	}
	if _, err := batch.ParseKeys(rr.BatchKeys); err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}

	cfg := &config.Config{
		APIURL:          apiURL,
		InstallID:       rr.InstallID,
		BaseURL:         baseURL,
		Protocol:        rr.Protocol,
		Model:           o.Model,
		ScheduleMinutes: config.ClampSchedule(rr.ScheduleMinutes),
		BatchKeys:       rr.BatchKeys,
	}

	// Drop secrets of a previous install, if any.
	if old, err := config.Load(o.ConfigPath); err == nil && old.InstallID != cfg.InstallID {
		_ = env.Store.Delete(keychain.EndpointKeyAccount(old.InstallID))
		_ = env.Store.Delete(keychain.InstallTokenAccount(old.InstallID))
	}

	if err := env.Store.Set(keychain.InstallTokenAccount(cfg.InstallID), rr.InstallToken); err != nil {
		return nil, fmt.Errorf("store install token in keychain: %w", err)
	}
	if err := env.Store.Set(keychain.EndpointKeyAccount(cfg.InstallID), key); err != nil {
		getenv := env.Getenv
		if getenv == nil {
			getenv = os.Getenv
		}
		if strings.TrimSpace(getenv(keychain.EnvEndpointKey)) == "" {
			_ = env.Store.Delete(keychain.InstallTokenAccount(cfg.InstallID))
			return nil, fmt.Errorf("store endpoint key in keychain: %w (set %s to run without a keychain)", err, keychain.EnvEndpointKey)
		}
		lg.Printf("warning: could not store the endpoint key in the keychain (%v); it will be read from %s on every run", err, keychain.EnvEndpointKey)
	}
	if err := config.Save(o.ConfigPath, cfg); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(baseURL, "https://") && !config.IsLoopbackHost(hostOf(baseURL)) {
		lg.Printf("warning: base URL is plain http; your endpoint key will cross the network unencrypted")
	}
	return cfg, nil
}

func hostOf(u string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	s, _, _ = strings.Cut(s, "/")
	if i := strings.LastIndex(s, ":"); i >= 0 && !strings.HasSuffix(s, "]") {
		s = s[:i]
	}
	return strings.Trim(s, "[]")
}

// ---- run ---------------------------------------------------------------------

// RunOptions control one or more cycles.
type RunOptions struct {
	ConfigPath string
	APIURL     string // optional override of the configured API URL
	JitterMin  time.Duration
	JitterMax  time.Duration
	Timeout    time.Duration // per request to the endpoint
}

// Summary describes one cycle.
type Summary struct {
	NothingDue      bool
	BatchID         string
	Sent            int
	Failed          int
	ScheduleMinutes int
	// Throttled is set when the endpoint answered 429/5xx or couldn't be reached and
	// the rest of the batch was not sent. RetryAfter is what the endpoint asked for.
	Throttled  bool
	RetryAfter time.Duration
}

// ThrottledCyclesBeforePause and ThrottlePause mirror the cloud's back-off rule:
// after 3 throttled cycles in a row, leave the endpoint alone for 24 hours.
const (
	ThrottledCyclesBeforePause = 3
	ThrottlePause              = 24 * time.Hour
)

// RunOnce performs one cycle: fetch, verify, send, upload.
func RunOnce(ctx context.Context, env Env, o RunOptions) (Summary, error) {
	var sum Summary
	cfg, err := config.Load(o.ConfigPath)
	if err != nil {
		return sum, err
	}
	sum.ScheduleMinutes = cfg.ScheduleMinutes
	apiURL := cfg.APIURL
	if o.APIURL != "" {
		if apiURL, err = config.NormalizeAPIURL(o.APIURL); err != nil {
			return sum, fmt.Errorf("api url: %w", err)
		}
	}
	keys, err := batch.ParseKeys(cfg.BatchKeys)
	if err != nil {
		return sum, err
	}
	if env.Store == nil {
		return sum, errors.New("no keychain store")
	}
	token, err := keychain.InstallToken(env.Store, cfg.InstallID)
	if err != nil {
		return sum, err
	}
	endpointKey, err := keychain.EndpointKey(env.Store, cfg.InstallID, env.Getenv)
	if err != nil {
		return sum, err
	}

	api := env.apiClient()
	bc := &batch.Client{APIURL: apiURL, Token: token, HTTP: api, UserAgent: env.userAgent()}
	envlp, err := bc.Next(ctx)
	if err != nil {
		return sum, err
	}
	if envlp == nil {
		sum.NothingDue = true
		return sum, nil
	}
	b, err := batch.Verify(envlp, batch.Expect{
		InstallID: cfg.InstallID,
		BaseURL:   cfg.BaseURL,
		Protocol:  cfg.Protocol,
		Model:     cfg.Model,
		Keys:      keys,
		Now:       env.now(),
	})
	if err != nil {
		return sum, err
	}
	sum.BatchID = b.BatchID

	s, err := sender.New(sender.Options{
		BaseURL:   cfg.BaseURL,
		Protocol:  cfg.Protocol,
		Key:       endpointKey,
		Client:    env.EndpointClient,
		Timeout:   o.Timeout,
		JitterMin: o.JitterMin,
		JitterMax: o.JitterMax,
	})
	if err != nil {
		return sum, err
	}
	results := s.Send(ctx, b.Probes)
	sum.Throttled, sum.RetryAfter = s.Stopped()
	for _, r := range results {
		if r.HTTPStatus != 0 {
			sum.Sent++
		}
		if r.Error != "" {
			sum.Failed++
		}
	}

	payload := upload.Build(b.BatchID, cfg.InstallID, results, endpointKey, token)
	uc := &upload.Client{APIURL: apiURL, Token: token, HTTP: api, UserAgent: env.userAgent()}
	if err := uc.Upload(ctx, payload, endpointKey, token); err != nil {
		return sum, err
	}
	return sum, nil
}

// Run loops until ctx is cancelled, running one cycle every schedule_minutes
// (plus up to 10% random spread). After consecutive failures it backs off,
// up to 8x the schedule.
func Run(ctx context.Context, env Env, o RunOptions) error {
	lg := env.logger()
	failures, throttled := 0, 0
	for {
		sum, err := RunOnce(ctx, env, o)
		if ctx.Err() != nil {
			return nil
		}
		switch {
		case errors.Is(err, config.ErrNotRegistered):
			return err
		case err != nil:
			failures++
			lg.Printf("cycle failed: %v", err)
		case sum.NothingDue:
			failures = 0
			lg.Printf("nothing due")
		default:
			failures = 0
			lg.Printf("batch %s: %d requests answered, %d with errors; results uploaded", sum.BatchID, sum.Sent, sum.Failed)
		}
		wait := nextWait(sum.ScheduleMinutes, failures)
		if sum.Throttled {
			throttled++
			if sum.RetryAfter > wait {
				wait = sum.RetryAfter
			}
			if throttled >= ThrottledCyclesBeforePause {
				lg.Printf("endpoint pushed back %d cycles in a row; pausing for %s", throttled, ThrottlePause)
				wait, throttled = ThrottlePause, 0
			}
		} else if err == nil {
			throttled = 0
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

func nextWait(scheduleMinutes, failures int) time.Duration {
	base := time.Duration(config.ClampSchedule(scheduleMinutes)) * time.Minute
	mult := 1
	for i := 0; i < failures && mult < 8; i++ {
		mult *= 2
	}
	base *= time.Duration(mult)
	spread := rand.Int64N(int64(base/10) + 1)
	return base + time.Duration(spread)
}

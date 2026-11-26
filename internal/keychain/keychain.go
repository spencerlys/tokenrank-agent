// Package keychain stores the agent's secrets in the operating system's
// credential store (macOS Keychain, Windows Credential Manager, or the Secret
// Service / libsecret on Linux).
//
// Two secrets are kept per install:
//
//	endpoint-key:<install_id>   the user's API key for their own endpoint
//	install-token:<install_id>  the bearer token the cloud issued at register
//
// Neither is ever written to the config file, to disk, or to a log.
package keychain

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/zalando/go-keyring"
)

// Service is the keychain service name for every entry the agent writes.
const Service = "tokenrank-agent"

// EnvEndpointKey is consulted when the endpoint key is not in the keychain
// (for example on a headless Linux host without a Secret Service).
const EnvEndpointKey = "TOKENRANK_ENDPOINT_KEY"

// ErrNotFound is returned when no secret exists for an account.
var ErrNotFound = errors.New("secret not found in keychain")

// Store is a minimal secret store.
type Store interface {
	Get(account string) (string, error)
	Set(account, secret string) error
	Delete(account string) error
}

// EndpointKeyAccount is the account name holding the endpoint key.
func EndpointKeyAccount(installID string) string { return "endpoint-key:" + installID }

// InstallTokenAccount is the account name holding the install token.
func InstallTokenAccount(installID string) string { return "install-token:" + installID }

type osStore struct{}

// NewOS returns a Store backed by the OS credential store via go-keyring.
// Tests replace the backend with keyring.MockInit().
func NewOS() Store { return osStore{} }

func (osStore) Get(account string) (string, error) {
	s, err := keyring.Get(Service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNotFound
	}
	return s, err
}

func (osStore) Set(account, secret string) error {
	return keyring.Set(Service, account, secret)
}

func (osStore) Delete(account string) error {
	err := keyring.Delete(Service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

// EndpointKey returns the endpoint key for installID from the store, falling
// back to the TOKENRANK_ENDPOINT_KEY environment variable. getenv may be nil.
func EndpointKey(s Store, installID string, getenv func(string) string) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	k, err := s.Get(EndpointKeyAccount(installID))
	if err == nil && k != "" {
		return k, nil
	}
	if v := strings.TrimSpace(getenv(EnvEndpointKey)); v != "" {
		return v, nil
	}
	if err == nil || errors.Is(err, ErrNotFound) {
		return "", fmt.Errorf("no endpoint key in the keychain and %s is not set", EnvEndpointKey)
	}
	return "", fmt.Errorf("read endpoint key from keychain: %v (and %s is not set)", err, EnvEndpointKey)
}

// InstallToken returns the install token for installID.
func InstallToken(s Store, installID string) (string, error) {
	t, err := s.Get(InstallTokenAccount(installID))
	if err != nil {
		return "", fmt.Errorf("read install token from keychain: %w", err)
	}
	if t == "" {
		return "", fmt.Errorf("read install token from keychain: %w", ErrNotFound)
	}
	return t, nil
}

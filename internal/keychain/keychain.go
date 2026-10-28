// Package keychain keeps the Zoom API key in the macOS Keychain.
package keychain

import (
	"errors"
	"os"
	"strings"

	"github.com/zalando/go-keyring"
)

const (
	service = "com.ai.capy"
	account = "zoom-api-key"
	// EnvKey overrides the stored key, for development and E2E runs.
	EnvKey = "ZOOM_API_KEY"
	// maxKeyLen bounds what is stored; API keys are far shorter, and
	// go-keyring cannot store much more than 3 KB.
	maxKeyLen = 1024
)

// ErrNotFound means no key is stored and none is in the environment.
var ErrNotFound = errors.New("keychain: no Zoom API key")

// FromEnv reports whether $ZOOM_API_KEY sets the key, overriding the Keychain.
func FromEnv() bool { return strings.TrimSpace(os.Getenv(EnvKey)) != "" }

// APIKey returns the key from $ZOOM_API_KEY, else the Keychain.
func APIKey() (string, error) {
	if FromEnv() {
		return strings.TrimSpace(os.Getenv(EnvKey)), nil
	}
	k, err := keyring.Get(service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNotFound
	}
	return k, err
}

// SetAPIKey stores key in the Keychain, replacing any earlier one.
func SetAPIKey(key string) error {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > maxKeyLen || strings.ContainsAny(key, "\r\n") {
		return errors.New("keychain: the API key is empty or malformed")
	}
	return keyring.Set(service, account, key)
}

// DeleteAPIKey removes the stored key; removing none is not an error.
func DeleteAPIKey() error {
	if err := keyring.Delete(service, account); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	return nil
}

package keychain

import (
	"errors"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestBlankEnvironmentKeyDoesNotOverride(t *testing.T) {
	keyring.MockInit()
	t.Setenv(EnvKey, "  ")
	if FromEnv() {
		t.Fatal("a blank $ZOOM_API_KEY overrides the Keychain, where Settings could then add no key")
	}
	if _, err := APIKey(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("APIKey: %v, want ErrNotFound", err)
	}
}

func TestDeleteAPIKeyRemovesTheKeyAndToleratesNone(t *testing.T) {
	keyring.MockInit() // an in-memory Keychain
	t.Setenv(EnvKey, "")
	if err := SetAPIKey("a-key"); err != nil {
		t.Fatal(err)
	}
	if err := DeleteAPIKey(); err != nil {
		t.Fatal(err)
	}
	if _, err := APIKey(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v, want ErrNotFound", err)
	}
	// Deleting again, as from a second Settings window, is not a failure.
	if err := DeleteAPIKey(); err != nil {
		t.Fatalf("deleting no key: %v", err)
	}
}

// Package placeholder defines the convention for opaque credential
// handles: a random "abph_" token the agent receives in place of the real
// Authorization value, plus the namespaced key used to store the real
// token in a shared store. The generic store itself (authlib/shared) holds
// no credential semantics.
package placeholder

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
)

// Prefix marks a value as a credential placeholder. token-exchange uses it
// as a cheap fast-path before attempting a store lookup.
const Prefix = "abph_"

// keyNamespace prefixes shared-store keys to avoid collisions with other
// shared-store consumers.
const keyNamespace = "placeholder/"

// New returns a fresh, unguessable handle (Prefix + 256 bits base64url).
func New() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// IsPlaceholder reports whether token is a placeholder handle.
func IsPlaceholder(token string) bool {
	return strings.HasPrefix(token, Prefix)
}

// Key returns the shared-store key for a handle.
func Key(handle string) string {
	return keyNamespace + handle
}

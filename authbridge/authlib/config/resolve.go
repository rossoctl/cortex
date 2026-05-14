// Package config's resolve.go previously constructed a shared
// auth.Config + auth.Auth for all plugins to share. With per-plugin
// configuration, that responsibility moved into each plugin's Configure
// (see authbridge/docs/plugin-reference.md), and this file is now
// just the shared credential-file waiters that multiple plugins need
// when they share a file path (e.g. /shared/client-id.txt used by both
// jwt-validation's audience_file and token-exchange's client_id_file).
package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// ReadCredentialFile performs a one-shot read of a credential file,
// returning its whitespace-trimmed contents. Used by plugins from
// Configure to opportunistically pick up values the operator has
// already mounted from a Secret; when it returns an error, the
// plugin should fall back to WaitForCredentialFile from Init to wait
// for the file.
func ReadCredentialFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Size() == 0 {
		return "", fmt.Errorf("file %s is empty", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// heartbeatInterval is how often WaitForCredentialFile emits a WARN
// while still waiting. Chosen so that a misconfigured volume mount
// (e.g., ConfigMap name typo) is loud enough to notice during an
// operational incident, but not so spammy that it drowns out real
// signal. Overridable at package scope for tests.
var heartbeatInterval = 60 * time.Second

// WaitForCredentialFile blocks until the file is readable with non-zero
// length, or until ctx is cancelled. Plugins call this from Init (via a
// goroutine) to wait out the race with the operator's Secret-mount
// propagation.
//
// Polls at 2s intervals — fast enough for human-observable boot times,
// slow enough that a pod full of plugins isn't hammering the kubelet.
// Emits a WARN every heartbeatInterval while the file is still absent
// so operators can spot wrong paths / missing volume mounts in
// `kubectl logs` during an incident, rather than discovering them
// after chasing silent 503s from traffic.
func WaitForCredentialFile(ctx context.Context, path string) (string, error) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()
	start := time.Now()
	for {
		if v, err := ReadCredentialFile(path); err == nil {
			return v, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-heartbeat.C:
			slog.Warn("credential file still not available",
				"path", path,
				"waited", time.Since(start).Round(time.Second))
		case <-ticker.C:
		}
	}
}

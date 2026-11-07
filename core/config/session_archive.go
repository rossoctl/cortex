package config

import (
	"fmt"
	"path/filepath"
)

// SessionArchiveConfig is `session.archive`: the session archive, which persists every session
// to disk so it survives a restart and the store's own eviction (core/session/archive).
//
// A LAPTOP FEATURE. The cortex binary runs it from a config inside ~/.cortex, where it writes to
// ~/.cortex/sessions, or from any config that sets Dir. A cluster cannot start it by mounting a
// volume over ~/.cortex: someone must write Dir. And off loopback, where DELETE /v1/sessions cannot
// clear it, someone must also write enabled: true. See ArchiveRunsOnLocalInstall.
//
// Nested under `session` rather than top-level like cost_ledger, because it records through
// the session store: the dependency is structural. And because reloader.validateReloadable
// refuses any live change under `session`, changing this needs a restart with no code of its
// own.
type SessionArchiveConfig struct {
	// Enabled is a pointer so unset can follow the default; see ArchiveRunsOnLocalInstall.
	// false always wins.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// RetentionDays is how long a segment is kept after its last write. 0 means the default,
	// 30.
	RetentionDays int `yaml:"retention_days,omitempty" json:"retention_days,omitempty"`
	// MaxBytes bounds the archive's size; the oldest segments go first past it. 0 means the
	// default, 2 GiB.
	MaxBytes int64 `yaml:"max_bytes,omitempty" json:"max_bytes,omitempty"`
	// Dir is where the archive writes. Empty means ~/.cortex/sessions, which only a local install
	// uses. Must be absolute, for the reason cost_ledger.dir must be.
	Dir string `yaml:"dir,omitempty" json:"dir,omitempty"`
}

// DirSet reports whether the config names a directory for the archive. Safe on a nil receiver.
func (c *SessionArchiveConfig) DirSet() bool {
	return c != nil && c.Dir != ""
}

// ArchiveEnabled reports whether the archive should run, given the caller's default for an
// unset block. Safe on a nil receiver.
func (c *SessionArchiveConfig) ArchiveEnabled(defaultOn bool) bool {
	if c == nil || c.Enabled == nil {
		return defaultOn
	}
	return *c.Enabled
}

// ArchiveRunsOnLocalInstall reports whether the session archive runs for c when it is started
// from a local install — a config inside ~/.cortex — or with session.archive.dir set, and why.
// Otherwise it never runs. That half is the caller's to decide, since only the caller knows where
// the config lives.
//
// ON BY DEFAULT ONLY WHERE A CLEAR REACHES IT: bound to loopback only, the one shape whose
// DELETE /v1/sessions answers. Elsewhere the history it wrote could only be removed by hand, so
// an unset block leaves it off there, and an explicit true opts in knowing that.
func (c *Config) ArchiveRunsOnLocalInstall() (bool, string) {
	a := c.Session.Archive
	switch {
	case a != nil && a.Enabled != nil && !*a.Enabled:
		return false, "session.archive.enabled is false"
	case a != nil && a.Enabled != nil && c.Listener.BindLoopbackOnly:
		return true, "session.archive.enabled is true"
	case a != nil && a.Enabled != nil:
		return true, "session.archive.enabled is true; DELETE /v1/sessions refuses to clear it, " +
			"because listener.bind_loopback_only is false"
	case c.Listener.BindLoopbackOnly:
		return true, "on by default when bound to loopback only; set session.archive.enabled: false to turn it off"
	}
	return false, "off by default: listener.bind_loopback_only is false, so DELETE /v1/sessions could not clear " +
		"what it wrote; set session.archive.enabled: true to opt in anyway"
}

// Validate is called from the loader when session.archive is present.
func (c *SessionArchiveConfig) Validate() error {
	if c.RetentionDays < 0 {
		return fmt.Errorf("session.archive.retention_days must not be negative, got %d", c.RetentionDays)
	}
	// The cost ledger's ceiling, for its reason: retention counted back with time arithmetic
	// that normalises can wrap a cutoff into the future and delete everything.
	if c.RetentionDays > maxCostLedgerRetentionDays {
		return fmt.Errorf("session.archive.retention_days must be at most %d (ten years), got %d",
			maxCostLedgerRetentionDays, c.RetentionDays)
	}
	if c.MaxBytes < 0 {
		return fmt.Errorf("session.archive.max_bytes must not be negative, got %d", c.MaxBytes)
	}
	if c.Dir != "" && !filepath.IsAbs(c.Dir) {
		return fmt.Errorf("session.archive.dir must be an absolute path, got %q: a relative path "+
			"resolves against the proxy's working directory, which is not a property of the config", c.Dir)
	}
	return nil
}

package main

import (
	"fmt"

	"github.com/rossoctl/cortex/core/config"
)

// praxisRefusal returns an error when praxis mode is on in the config file at path
// or in the running Cortex at statsURL. In praxis mode Cortex runs no outbound plugins, so a
// plugin edit has no effect. The file is what the next start runs, and /config is
// what runs now, so either one refuses. statsURL "" skips the /config read, and a
// failed read counts as "not on".
func praxisRefusal(cfg *config.Config, path, statsURL string) error {
	inFile := cfg != nil && cfg.PraxisEnabled()
	running := false
	if statsURL != "" {
		if rc, err := runningConfig(statsURL); err == nil {
			running = rc.PraxisEnabled()
		}
	}
	var where string
	switch {
	case inFile && running:
		where = "in " + path + " and in the running Cortex"
	case inFile:
		where = "in " + path
	case running:
		where = "in the running Cortex"
	default:
		return nil
	}
	return fmt.Errorf("praxis mode is on (%s), so Cortex runs no outbound plugins and this command has no effect. To turn it off, set praxis.enabled: false in %s and run: agentop service install --restart", where, path)
}

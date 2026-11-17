package accounting

import (
	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/pipeline"
)

// ThreadOf gives the accountant.Thread for pctx's request (accountant/docs/cortex-wiring.md
// W5.2): "main" for the primary agent, and the Claude Code subagent id for a subagent, with
// Parent set to the agent that started it. A subagent with no recorded parent forked from the
// primary agent, so its parent is "main".
//
// This is the default rule with no context-guru plugin in the pipeline. Task 2 adds the plugin's
// override (pctx.SetAccountingThread), which lets context-guru's own thread split — built from
// prefix continuity inside the engine, not from the subagent header — take precedence where it
// ran (design §5.4).
func ThreadOf(pctx *pipeline.Context) accountant.Thread {
	sub := pctx.Subagent()
	if sub == "" {
		return accountant.Thread{ID: "main"}
	}
	parent := pctx.ParentSubagent()
	if parent == "" {
		parent = "main"
	}
	return accountant.Thread{ID: sub, Parent: parent}
}

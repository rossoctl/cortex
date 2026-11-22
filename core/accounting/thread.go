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
// That is the default rule, with no context-guru plugin in the pipeline. A plugin that named a
// thread with pctx.SetAccountingThread overrides it: context-guru's own thread split, built from
// prefix continuity inside the engine rather than from the subagent header, takes precedence
// where it ran (design §5.4).
func ThreadOf(pctx *pipeline.Context) accountant.Thread {
	if id, parent, ok := pctx.AccountingThread(); ok {
		return accountant.Thread{ID: id, Parent: parent}
	}
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

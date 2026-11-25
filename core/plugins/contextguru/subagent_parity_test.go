package contextguru

import (
	"net/http"
	"testing"

	"github.com/rossoctl/context-guru/agent"
	"github.com/rossoctl/cortex/core/pipeline"
)

// TestSubagentMatchesContextGuru pins core/pipeline's copy of the subagent header rule
// (pipeline.Context.Subagent) to context-guru's own (agent.Client.SubagentID). core/pipeline may
// not import context-guru, so the rule is copied there, and this test fails when they drift. The
// engine's thread, the keeper's key and the accountant's thread are all read from Subagent, so a
// drift would split one subagent differently in Cortex and in context-guru-proxy.
//
// Bob is left out: context-guru names no Bob subagent, and Cortex reads Bob's task headers.
func TestSubagentMatchesContextGuru(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.Header
	}{
		{"no headers", http.Header{}},
		{"claude code main", http.Header{"User-Agent": {"claude-cli/2.1.302 (external, cli)"}}},
		{"claude code subagent", http.Header{"User-Agent": {"claude-cli/2.1.302"}, "X-Claude-Code-Agent-Id": {"a1b2c3"}}},
		{"codex main", http.Header{"User-Agent": {"codex_cli_rs/0.172.0"}, "Thread-Id": {"t-main"}, "Session-Id": {"s1"}}},
		{"codex subagent", http.Header{"User-Agent": {"codex_cli_rs/0.172.0"}, "Thread-Id": {"t-sub"}, "X-Codex-Parent-Thread-Id": {"t-main"}}},
		{"codex subagent by originator", http.Header{"User-Agent": {"my-app/1.0"}, "Originator": {"codex_vscode"}, "Thread-Id": {"t-sub"}, "X-Codex-Parent-Thread-Id": {"t-main"}}},
		{"codex ids from another client", http.Header{"User-Agent": {"curl/8"}, "Thread-Id": {"t-sub"}, "X-Codex-Parent-Thread-Id": {"t-main"}}},
		{"codex subagent with no thread id", http.Header{"User-Agent": {"codex_exec/0.172.0"}, "X-Codex-Parent-Thread-Id": {"t-main"}}},
		{"opencode main", http.Header{"User-Agent": {"opencode/1.2.0"}, "X-Opencode-Session-Id": {"ses-main"}}},
		{"opencode subagent", http.Header{"User-Agent": {"opencode/1.2.0"}, "X-Opencode-Session-Id": {"ses-sub"}, "X-Opencode-Parent-Session-Id": {"ses-main"}}},
		{"opencode headers, no user agent", http.Header{"X-Opencode-Session-Id": {"ses-sub"}, "X-Opencode-Parent-Session-Id": {"ses-main"}}},
		{"opencode headers on claude code", http.Header{"User-Agent": {"claude-cli/2.1.302"}, "X-Opencode-Session-Id": {"ses-sub"}, "X-Opencode-Parent-Session-Id": {"ses-main"}}},
		{"opencode headers on codex", http.Header{"User-Agent": {"codex_cli_rs/0.172.0"}, "X-Opencode-Session-Id": {"ses-sub"}, "X-Opencode-Parent-Session-Id": {"ses-main"}}},
		{"opencode headers on pi", http.Header{"X-Context-Guru-Client": {"pi"}, "X-Opencode-Session-Id": {"ses-sub"}, "X-Opencode-Parent-Session-Id": {"ses-main"}}},
		{"opencode headers on bob", http.Header{"User-Agent": {"bobshell/1.0"}, "X-Opencode-Session-Id": {"ses-sub"}, "X-Opencode-Parent-Session-Id": {"ses-main"}}},
		{"claude header wins over codex", http.Header{"User-Agent": {"codex_cli_rs/0.172.0"}, "X-Claude-Code-Agent-Id": {"a1b2c3"}, "Thread-Id": {"t-sub"}, "X-Codex-Parent-Thread-Id": {"t-main"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := agent.Identify(tc.h).SubagentID()
			pctx := &pipeline.Context{Headers: tc.h}
			if got := pctx.Subagent(); got != want {
				t.Fatalf("pipeline Subagent() = %q, context-guru SubagentID() = %q", got, want)
			}
		})
	}
}

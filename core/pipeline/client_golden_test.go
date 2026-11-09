package pipeline

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// userAgentGolden is the table that the Rust port (praxis/filters/src/record/client.rs) reads.
const userAgentGolden = "testdata/user-agent.golden.json"

type userAgentCase struct {
	UA     string       `json:"ua"`
	Client *EventClient `json:"client"`
}

type subagentCase struct {
	Value string `json:"value"`
	ID    string `json:"id"`
}

// goldenUserAgents are the user-agent literals of client_test.go, plus edge cases.
var goldenUserAgents = []string{
	"",
	"claude-cli/2.1.14 (external, cli)",
	"claude-cli/2.1.14",
	"SomeNewAgent/9.9",
	"curl/8.4.0",
	"   ",
	"claude-cli/2.1.14\t(external, cli)",
	"claude-cli/2.1.14\tx",
	"ai-sdk/openai-compatible/3.0.36 ai-sdk/provider-utils/5.0.29 runtime/node.js/24 bob-shell/2.0.5",
	"ai/7.0.16 ai-sdk/provider-utils/5.0.29 runtime/node.js/24 bob-shell/2.0.5",
	"bob-shell/2.0.5",
	"claude-cli/2.1.270 bob-shell/2.0.5",
	"bob-shell/2.0.5 claude-cli/2.1.270",
	"ai-sdk/openai-compatible/3.0.36 claude-cli/2.1.270 bob-shell/2.0.5",
	"ai-sdk/openai-compatible/3.0.36 ai-sdk/provider-utils/5.0.29 runtime/node.js/v24.15.0 IBM Bob/2.2.1",
	"ai/7.0.16 ai-sdk/provider-utils/5.0.29 runtime/node.js/v24.15.0 IBM Bob/2.2.1",
	"IBM Bob/2.2.1",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) IBMBob/1.126.0+bob2.2.1 Chrome/148.0.7778",
	"Code/1.126.0+bob2.2.1 Darwin/24.6.0",
	"codex_exec/0.160.1 (Mac OS 26.3.0; arm64) unknown (codex_exec; 0.160.1)",
	"codex_exec/0.160.1 (Mac OS 26.3.0; arm64) unknown",
	"codex-mcp-client/0.160.1",
	"codex-tui/0.160.1 (Mac OS 26.3.0; arm64) xterm-256color (codex-tui; 0.160.1)",
	"codex_cli_rs/0.160.1 (Mac OS 26.3.0; arm64) xterm-256color",
	"codex-tui/0.162.1 (Mac OS 26.3.0; arm64) xterm-256color (codex-tui; 0.162.1)",
	"codex_cli_rs/0.162.1 (Mac OS 26.3.0; arm64) xterm-256color",
	"codex_vscode/0.160.1",
	"codex_desktop/0.160.1",
	"OTel-OTLP-Exporter-Rust/0.31.0",
	"pi/1.4.0 (darwin; node/v24.3.0; arm64)",
	"pi",
	"pi (darwin 25.6.0; arm64)",
	"pip/24.0 (python 3.12)",
	"pino/9.0",
	"pi-hole/6.0",
	"claude-cli/2.1.233 (external, claude-vscode, agent-sdk/0.3.233)",
	"claude-code/2.1.233 (claude-vscode, agent-sdk/0.3.233)",
	"Claude-User (claude-code/2.1.233; +https://support.anthropic.com/)",
	"opencode/latest/2.0.21/cli",
	"opencode/beta/2.1.0-beta.3/tui",
	"opencode/2.0.21",
	"bob-shell/2.0.5/extra",
	"some-tool/1.2 runtime/node.js/24 libcurl/8.4.0",
	strings.Repeat("A", 10_000),
	"claude-cli/" + strings.Repeat("9", 10_000),
	"claude-cli/2.1.14\x1b[2J\x1b[31mPWNED",
	"claude-cli/2.1.14\u009b2J",
	"newagent/1\u009b31m",
	"newagent/1\x7f",
	"newagent/1\nfake-row",
	"newagent/1‮gnitcepsus",
	"newagent/1⁦x⁩",
	"newagent/1​",
	"newagent/1‍2",
	"claude-cli/2.1.14\x1b[31m (external, cli)",
	strings.Repeat("\x1b", maxClientLen+32),
	"x" + strings.Repeat("é", 70),
	strings.Repeat("✓", 60),
	strings.Repeat("é", maxClientLen/2+8),
	"wetter-agent/1.0 (Zürich, ✓)",
	"claude-cli/2.1.284 (external, cli)",
	"Claude-User (claude-code/2.1.284; +https://support.anthropic.com/)",
	"ai-sdk/5.0.1 openai-compatible/3.0.36 bob-shell/2.0.5",
	"Go-http-client/1.1",
	"python-httpx/0.27.0",
	"axios/1.15.2",
	"node",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)",
	// Edge cases for the port.
	strings.Repeat("b", 600),
	"claude-cli/2.1.14 (ext\x00ernal)",
	" claude-cli/2.1.14\u0085",
	"CLAUDE-CLI/2.1.14",
	// Go's strings.ToLower maps U+0130 to a plain "i"; a full Unicode lowercase does not.
	"pİ/1.0",
	"ai/1 İBM-x/2 Bob/3",
}

// goldenSubagentValues are the header values of subagent_test.go.
var goldenSubagentValues = []string{
	"a1a2e36cb5946f31c",
	"",
	"   ",
	" a1a2e36cb5946f31c ",
	"\ta1a2e36cb5946f31c\t",
	" \t a1a2e36cb5946f31c \t ",
	"a1a2\x1b[2Je36cb",
	"a1a2\ne36cb",
	"a1a2\x7fe36cb",
	"a1a2\u009b2Je36cb",
	"a1a2‮e36cb",
	"a1a2‍e36cb",
	strings.Repeat("a", maxClientLen+1),
	"a1a2e36cb5946f31c\n",
	"\na1a2e36cb5946f31c",
	"a1a2e36cb5946f31c\r",
	"a1a2e36cb5946f31c\r\n",
	"\va1a2e36cb5946f31c",
	"a1a2e36cb5946f31c\f",
	"a1a2e36cb5946f31c\u0085",
	"\u0085a1a2e36cb5946f31c",
	strings.Repeat("a", maxClientLen),
	"a21728bb34d27cb12",
	"a0974b0b8591dc76b",
}

// TestParseUserAgent_Golden writes or checks the table that pins the Rust port to Go.
// Inputs that are not valid UTF-8 are left out: JSON cannot carry them, and the Rust
// function takes a str.
func TestParseUserAgent_Golden(t *testing.T) {
	uas := slices.Clone(goldenUserAgents)
	files, err := filepath.Glob("../plugins/inferenceparser/testdata/*/*/request.headers")
	if err != nil || len(files) == 0 {
		t.Fatalf("no request.headers fixtures: %v", err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for line := range strings.SplitSeq(string(raw), "\n") {
			if name, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(name, "User-Agent") {
				if v = strings.TrimSpace(v); !slices.Contains(uas, v) {
					uas = append(uas, v)
				}
			}
		}
	}

	var golden struct {
		UserAgent []userAgentCase `json:"userAgent"`
		Subagent  []subagentCase  `json:"subagent"`
	}
	for _, ua := range uas {
		if utf8.ValidString(ua) {
			golden.UserAgent = append(golden.UserAgent, userAgentCase{ua, ParseUserAgent(ua)})
		}
	}
	for _, v := range goldenSubagentValues {
		golden.Subagent = append(golden.Subagent, subagentCase{v, subagentHeader(v)})
	}
	got, err := json.MarshalIndent(golden, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')

	if *update {
		if err := os.WriteFile(userAgentGolden, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(userAgentGolden)
	if err != nil {
		t.Fatalf("%v (run with -update to write it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s is out of date (run with -update to rewrite it, then run the Rust test)", userAgentGolden)
	}
}

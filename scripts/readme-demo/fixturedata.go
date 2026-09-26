package main

// fixturedata supplies the tool manifest and conversation a Claude Code request
// carries.
//
// These are not decoration. sessionapi.summarizeEvent recomputes
// ToolCount = len(Tools) and MessageCount = len(Messages) and then nils both
// arrays, so a fixture that sets only the counts has them overwritten with zero
// on every timeline fetch — and sessions_context.go skips any response whose tool
// count is zero, leaving CONTEXT(1M) a dash no matter what the counts said. The
// manifest has to be real for the gauge to work, and once it is real the detail
// pane has genuine content to scroll.

import (
	"fmt"

	"github.com/rossoctl/cortex/core/pipeline"
)

// claudeCodeTools is the manifest an agent re-sends on every single turn, which
// is the thing act 4's detail pane is there to show.
var claudeCodeTools = []pipeline.InferenceTool{
	{Name: "Task", Description: "Launch a new agent to handle complex, multi-step tasks.",
		Parameters: `{"type":"object","properties":{"description":{"type":"string"},"prompt":{"type":"string"},"subagent_type":{"type":"string"}},"required":["description","prompt"]}`},
	{Name: "Bash", Description: "Executes a bash command and returns its output.",
		Parameters: `{"type":"object","properties":{"command":{"type":"string"},"timeout":{"type":"number"},"run_in_background":{"type":"boolean"}},"required":["command"]}`},
	{Name: "Glob", Description: "Fast file pattern matching for any codebase size.",
		Parameters: `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"}},"required":["pattern"]}`},
	{Name: "Grep", Description: "A powerful search tool built on ripgrep.",
		Parameters: `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"output_mode":{"type":"string"}},"required":["pattern"]}`},
	{Name: "Read", Description: "Reads a file from the local filesystem.",
		Parameters: `{"type":"object","properties":{"file_path":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}},"required":["file_path"]}`},
	{Name: "Edit", Description: "Performs exact string replacement in a file.",
		Parameters: `{"type":"object","properties":{"file_path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean"}},"required":["file_path","old_string","new_string"]}`},
	{Name: "Write", Description: "Writes a file to the local filesystem.",
		Parameters: `{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}`},
	{Name: "NotebookEdit", Description: "Replace the contents of a cell in a Jupyter notebook.",
		Parameters: `{"type":"object","properties":{"notebook_path":{"type":"string"},"cell_id":{"type":"string"},"new_source":{"type":"string"}},"required":["notebook_path","new_source"]}`},
	{Name: "WebFetch", Description: "Fetches a URL and answers a prompt against it.",
		Parameters: `{"type":"object","properties":{"url":{"type":"string"},"prompt":{"type":"string"}},"required":["url","prompt"]}`},
	{Name: "TodoWrite", Description: "Create and manage a structured task list.",
		Parameters: `{"type":"object","properties":{"todos":{"type":"array","items":{"type":"object"}}},"required":["todos"]}`},
	{Name: "WebSearch", Description: "Search the web and return formatted results.",
		Parameters: `{"type":"object","properties":{"query":{"type":"string"},"allowed_domains":{"type":"array"}},"required":["query"]}`},
	{Name: "BashOutput", Description: "Retrieve output from a running background shell.",
		Parameters: `{"type":"object","properties":{"bash_id":{"type":"string"},"filter":{"type":"string"}},"required":["bash_id"]}`},
	{Name: "KillShell", Description: "Kill a running background bash shell by its ID.",
		Parameters: `{"type":"object","properties":{"shell_id":{"type":"string"}},"required":["shell_id"]}`},
	{Name: "SlashCommand", Description: "Execute a slash command within the main conversation.",
		Parameters: `{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`},
	{Name: "AskUserQuestion", Description: "Ask the user a question when blocked on a decision.",
		Parameters: `{"type":"object","properties":{"questions":{"type":"array","items":{"type":"object"}}},"required":["questions"]}`},
}

// tools returns the first n of the manifest, clamped.
func tools(n int) []pipeline.InferenceTool {
	if n <= 0 {
		return nil
	}
	if n > len(claudeCodeTools) {
		n = len(claudeCodeTools)
	}
	out := make([]pipeline.InferenceTool, n)
	copy(out, claudeCodeTools[:n])
	return out
}

// conversationSeeds are the opening exchanges of each demo session, so the detail
// pane shows a conversation somebody might recognise rather than lorem ipsum.
var conversationSeeds = map[string][]pipeline.InferenceMessage{
	"api-7f3c": {
		{Role: "system", Content: "You are Claude Code, Anthropic's official CLI for Claude."},
		{Role: "user", Content: "The retry handler drops the last attempt's error. Fix it and add a test."},
		{Role: "assistant", Content: "I'll start by reading the retry handler to see how attempts are tracked."},
		{Role: "user", Content: "[tool_result] src/retry/handler.go (142 lines)"},
	},
	"web-2a91": {
		{Role: "system", Content: "You are Claude Code, Anthropic's official CLI for Claude."},
		{Role: "user", Content: "Add a dark mode toggle to the settings page, persisted in localStorage."},
		{Role: "assistant", Content: "Let me look at how the settings page is composed today."},
		{Role: "user", Content: "[tool_result] src/pages/Settings.tsx (88 lines)"},
	},
	"infra-55de": {
		{Role: "system", Content: "You are Claude Code, Anthropic's official CLI for Claude."},
		{Role: "user", Content: "helm upgrade fails with 'template: no such key'. Find which values file is wrong."},
		{Role: "assistant", Content: "I'll diff the values files against the chart's template references."},
		{Role: "user", Content: "[tool_result] charts/app/values.staging.yaml (61 lines)"},
	},
}

// conversation grows a session's message list to n entries, which is what makes a
// later turn's prompt visibly larger than an earlier one: every turn re-sends
// every earlier message.
func conversation(sessionID string, n int) []pipeline.InferenceMessage {
	seed := conversationSeeds[sessionID]
	out := make([]pipeline.InferenceMessage, 0, n)
	out = append(out, seed...)
	filler := []pipeline.InferenceMessage{
		{Role: "assistant", Content: "Now I'll make the change in the file I just read."},
		{Role: "user", Content: "[tool_result] edit applied: 1 replacement"},
		{Role: "assistant", Content: "Running the tests to confirm the change holds."},
		{Role: "user", Content: "[tool_result] ok  0.412s"},
	}
	for i := 0; len(out) < n; i++ {
		m := filler[i%len(filler)]
		if m.Role == "user" {
			m.Content = fmt.Sprintf("%s  (step %d)", m.Content, i/len(filler)+1)
		}
		out = append(out, m)
	}
	if len(out) > n {
		out = out[:n]
	}
	return out
}

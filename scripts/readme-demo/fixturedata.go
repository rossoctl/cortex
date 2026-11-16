package main

// fixturedata supplies the tool manifest and conversation a Claude Code request
// carries.
//
// These are not decoration. sessionapi.summarizeEvent recomputes
// ToolCount = len(Tools) and MessageCount = len(Messages) and then nils both
// arrays, so a fixture that sets only the counts has them overwritten with zero
// on every timeline fetch — and sessions_context.go skips any response whose tool
// count is zero, leaving CTX(1M) a dash no matter what the counts said. The
// manifest has to be real for the gauge to work, and once it is real the detail
// pane has genuine content to scroll.

import (
	"slices"

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

// fixtureCall is one tool call in a demo conversation, with the output it got back.
type fixtureCall struct {
	name, args, result string
}

// call makes the assistant message that calls a tool and the user message that carries
// its result, as Claude Code sends them: the text names the step, and the call and its
// output travel as tool blocks keyed by id, which the detail pane shows under the message.
func call(id, say string, c fixtureCall) []pipeline.InferenceMessage {
	fixtureCalls[id] = c
	return []pipeline.InferenceMessage{
		{Role: "assistant", Content: say, ToolCallNames: c.name, ToolUseIDs: id},
		{Role: "user", ToolUseIDs: id},
	}
}

// fixtureCalls holds every call that call made, by id.
var fixtureCalls = map[string]fixtureCall{}

func seed(system, ask string, steps ...[]pipeline.InferenceMessage) []pipeline.InferenceMessage {
	out := []pipeline.InferenceMessage{{Role: "system", Content: system}, {Role: "user", Content: ask}}
	for _, st := range steps {
		out = append(out, st...)
	}
	return out
}

const claudeSystem = "You are Claude Code, Anthropic's official CLI for Claude."

// conversationSeeds are the exchanges of each demo session, so the detail pane shows a
// conversation somebody might recognise rather than lorem ipsum.
var conversationSeeds = map[string][]pipeline.InferenceMessage{
	"web-2a91": seed(claudeSystem, "Add a dark mode toggle to the settings page, persisted in localStorage.",
		call("toolu_web_01", "Let me look at how the settings page is composed today.",
			fixtureCall{"Read", `{"file_path":"src/pages/Settings.tsx"}`,
				"export function Settings() {\n  return (\n    <Page title=\"Settings\">\n      <ProfileForm />\n    </Page>\n  )\n}"}),
		call("toolu_web_02", "I'll find where the theme is set, so the toggle can drive it.",
			fixtureCall{"Grep", `{"pattern":"data-theme","path":"src"}`,
				"src/main.tsx:9:  document.documentElement.dataset.theme = \"light\""}),
		call("toolu_web_03", "Now I'll add the toggle and store the choice.",
			fixtureCall{"Edit", `{"file_path":"src/pages/Settings.tsx","old_string":"<ProfileForm />","new_string":"<ProfileForm />\n      <ThemeToggle storageKey=\"theme\" />"}`,
				"The file src/pages/Settings.tsx has been updated."}),
		call("toolu_web_04", "Running the tests to confirm the change holds.",
			fixtureCall{"Bash", `{"command":"npm test -- Settings"}`,
				"PASS  src/pages/Settings.test.tsx\n  ✓ toggles dark mode (31 ms)\n  ✓ keeps the choice after reload (12 ms)"}),
	),
	"infra-55de": seed(claudeSystem, "helm upgrade fails with 'template: no such key'. Find which values file is wrong.",
		call("toolu_infra_01", "I'll render the chart with the staging values to see the failing key.",
			fixtureCall{"Bash", `{"command":"helm template charts/app -f charts/app/values.staging.yaml"}`,
				"Error: template: app/templates/deploy.yaml:31: map has no entry for key \"replicas\""}),
	),
}

// conversation is the first n messages of a session's seed, which is what makes a later
// turn's prompt visibly larger than an earlier one: every turn re-sends every earlier
// message.
func conversation(sessionID string, n int) []pipeline.InferenceMessage {
	seed := conversationSeeds[sessionID]
	if n > len(seed) {
		n = len(seed)
	}
	return slices.Clone(seed[:n])
}

// toolHistory is the tool calls and results that msgs carry, in the two fields a request
// sends them in.
func toolHistory(msgs []pipeline.InferenceMessage) (calls []pipeline.InferenceToolCall, results []pipeline.InferenceToolResult) {
	for _, m := range msgs {
		c, ok := fixtureCalls[m.ToolUseIDs]
		switch {
		case !ok:
		case m.Role == "assistant":
			calls = append(calls, pipeline.InferenceToolCall{ID: m.ToolUseIDs, Name: c.name, Arguments: c.args})
		default:
			results = append(results, pipeline.InferenceToolResult{ToolUseID: m.ToolUseIDs, Content: c.result})
		}
	}
	return calls, results
}

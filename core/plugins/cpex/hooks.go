package cpex

// CPEX hook names. AuthBridge dispatches operator-configured subsets
// of these on each pipeline phase via cpexConfig.Hooks. The exact set
// CPEX accepts is governed by whichever sub-plugins are registered on
// the manager; validation of unknown hook names happens at LoadConfig
// time on the CPEX side.
//
// Exported so operators reading the AuthBridge plugin's config schema
// — and out-of-tree code that wants to build hook lists
// programmatically — refer to the same names CPEX uses.
const (
	// HookToolPreInvoke fires before an MCP tool call is forwarded.
	// Canonical APL home for tool-args validation (validator/pii-scan)
	// and PDP checks (pdp/cedar-direct, pdp/cedarling).
	HookToolPreInvoke = "cmf.tool_pre_invoke"

	// HookToolPostInvoke fires after an MCP tool call returns, before
	// the result reaches the agent. Canonical APL home for audit/logger
	// and post-call delegators.
	HookToolPostInvoke = "cmf.tool_post_invoke"

	// HookLLMInput fires on LLM inference request before it leaves the
	// pod. Canonical APL home for prompt-PII redactors.
	HookLLMInput = "cmf.llm_input"

	// HookLLMOutput fires on LLM inference response before it reaches
	// the agent. Canonical APL home for output redactors and audit.
	HookLLMOutput = "cmf.llm_output"
)

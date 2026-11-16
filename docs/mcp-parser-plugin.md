# `mcp-parser`

Parses MCP tool calls/results into `pctx.Extensions.MCP` for downstream
policy plugins.

This plugin makes no decisions of its own — it exists to feed others. The
plugins that consume `pctx.Extensions.MCP` are:

| Consumer | How it uses the MCP extension |
|---|---|
| [`ibac`](ibac-plugin.md) | Reads the tool name and arguments to judge the call against user intent. Declares `mcp-parser` in `RequiresAny`. |
| [`sparc`](sparc-plugin.md) | Extracts the tool name/arguments to reflect on, and returns clarifications as MCP results. Declares `mcp-parser` in `RequiresAny`; required in `enforcement: mcp` mode. |
| [`cpex`](cpex-plugin.md) | Converts the parsed call/result into a CMF message for the `cmf.tool_pre_invoke` / `cmf.tool_post_invoke` hooks. Declares `mcp-parser` in `RequiresAny`. |
| [`opa`](opa-plugin.md) | Exposes the parsed call as `input.mcp` for policy (add `mcp.params` to `include` for arguments). |

Place `mcp-parser` **before** these plugins on the outbound chain;
without it they see no MCP data and pass the traffic through
unclassified.

- `paths` (`[]string`) — URL path globs treated as MCP endpoints (for body-less transport detection: SSE GET, session-terminate DELETE). Default `["/mcp", "/mcp/"]`. Only a canonical path matches, as for `jwt-validation`'s `bypass_paths`.

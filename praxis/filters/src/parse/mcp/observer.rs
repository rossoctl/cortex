//! Port of the mcp-parser plugin's dispatch: `OnRequest` and `OnResponseFrame` in
//! core/plugins/mcpparser/hooks.go. The Observe and Skip calls are not ported.

use http::HeaderMap;

use super::{parse_request, parse_response, top_level_keys_exact};
use crate::parse::model::McpExtension;

/// Parses one MCP request and the response to it, as the plugin does.
pub struct McpObserver {
    ext: Option<McpExtension>,
    exact_keys: bool,
}

impl McpObserver {
    /// Port of `OnRequest`. The extension stays `None` when the parser does not claim the request.
    pub fn on_request(method: &str, path: &str, headers: &HeaderMap, body: &[u8]) -> Self {
        let ext = parse_request(method, path, headers, body);
        let exact_keys = ext.is_some() && top_level_keys_exact(headers, body);
        Self { ext, exact_keys }
    }

    /// The extension for a policy decision: `None` when `top_level_keys_exact` fails.
    pub fn policy_extension(&self) -> Option<&McpExtension> {
        self.ext.as_ref().filter(|_| self.exact_keys)
    }

    /// Port of `OnResponseFrame`. The first frame with a response wins: JSON-RPC
    /// gives a request one response, so a later frame is ignored.
    pub fn on_response_frame(&mut self, frame: &[u8], _last: bool) {
        let Some(ext) = self.ext.as_mut() else {
            return;
        };
        if frame.is_empty() || ext.result.is_some() || ext.err.is_some() {
            return;
        }
        parse_response(frame, ext);
    }

    pub fn extension(&self) -> Option<&McpExtension> {
        self.ext.as_ref()
    }
}

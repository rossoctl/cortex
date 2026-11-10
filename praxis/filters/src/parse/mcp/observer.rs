//! Port of the mcp-parser plugin's dispatch: `OnRequest` and `OnResponseFrame` in
//! core/plugins/mcpparser/hooks.go. The Observe and Skip calls are not ported.

use http::HeaderMap;

use super::{parse_request, parse_response};
use crate::parse::model::McpExtension;

/// Parses one MCP request and the response to it, as the plugin does.
pub struct McpObserver {
    ext: Option<McpExtension>,
}

impl McpObserver {
    /// Port of `OnRequest`. The extension stays `None` when the parser does not claim the request.
    pub fn on_request(method: &str, path: &str, headers: &HeaderMap, body: &[u8]) -> Self {
        Self {
            ext: parse_request(method, path, headers, body),
        }
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

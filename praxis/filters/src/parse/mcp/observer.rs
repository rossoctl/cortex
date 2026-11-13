//! Port of the mcp-parser plugin's dispatch: `OnRequest` and `OnResponseFrame` in
//! core/plugins/mcpparser/hooks.go. The Observe and Skip calls are not ported.

use http::HeaderMap;

use super::{foreign_charset, mcp_shaped, parse_request, parse_response, reads_alike};
use crate::parse::model::McpExtension;

/// Parses one MCP request and the response to it, as the plugin does.
pub struct McpObserver {
    ext: Option<McpExtension>,
    /// True for an empty body. Else false for a `foreign_charset`, else with
    /// `ext`: `reads_alike`, and without it: not `mcp_shaped`.
    alike: bool,
}

/// The parsed MCP request, as a policy decision may use it.
pub enum PolicyView<'a> {
    /// `reads_alike` holds for the body.
    Exact(&'a McpExtension),
    /// Another parser can read the body in another way.
    Ambiguous,
}

impl McpObserver {
    /// Port of `OnRequest`. The extension stays `None` when the parser does not claim the request.
    pub fn on_request(method: &str, path: &str, headers: &HeaderMap, body: &[u8]) -> Self {
        let ext = parse_request(method, path, headers, body);
        let alike = body.is_empty()
            || !foreign_charset(headers)
                && match ext {
                    Some(_) => reads_alike(body),
                    None => !mcp_shaped(headers, body),
                };
        Self { ext, alike }
    }

    /// The request for a policy decision. `None` for an empty body, and when the
    /// parser does not claim the body, `mcp_shaped` does not match it, and it has
    /// no `foreign_charset`.
    /// The row still uses `extension`, so it does not change.
    pub fn policy_view(&self) -> Option<PolicyView<'_>> {
        match (self.ext.as_ref(), self.alike) {
            (Some(ext), true) => Some(PolicyView::Exact(ext)),
            (None, true) => None,
            (_, false) => Some(PolicyView::Ambiguous),
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

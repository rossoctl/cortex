//! Port of the a2a-parser plugin's dispatch: `OnRequest` and `OnResponseFrame`
//! in core/plugins/a2aparser/hooks.go. The Observe and Skip calls are not ported.

use http::HeaderMap;

use super::{parse_request, parse_response};
use crate::parse::model::A2aExtension;

/// Parses one request and the response to it, as the plugin does.
pub struct A2aObserver {
    ext: Option<A2aExtension>,
}

impl A2aObserver {
    /// Port of `OnRequest`. The extension stays `None` when the parser does
    /// not claim the request.
    pub fn on_request(headers: &HeaderMap, body: &[u8]) -> Self {
        Self {
            ext: parse_request(headers, body),
        }
    }

    /// Port of `OnResponseFrame`: each frame that is not empty is folded into the
    /// extension. A frame is one SSE event, or the whole buffered body.
    pub fn on_response_frame(&mut self, frame: &[u8], _last: bool) {
        if let Some(ext) = self.ext.as_mut()
            && !frame.is_empty()
        {
            parse_response(frame, ext);
        }
    }

    pub fn extension(&self) -> Option<&A2aExtension> {
        self.ext.as_ref()
    }
}

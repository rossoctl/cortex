//! Port of the inference-parser plugin's dispatch: `OnRequest` and
//! `OnResponseFrame` in core/plugins/inferenceparser/hooks.go. Pricing
//! (`settleCost`) is not ported; only its `SettleServedModel` call is.

use http::HeaderMap;

use super::text::go_lossy_utf8;
use super::{
    Stream, carries_sse_framing, gojson, parse_request, read_response, settle_served_model,
};
use crate::parse::model::InferenceExtension;

/// Parses one request and the response to it, as the plugin does.
pub struct InferenceObserver {
    path: String,
    ext: Option<InferenceExtension>,
    /// The fold of a streamed response; its existence tells the last frame that a stream ran.
    stream: Option<Stream>,
}

/// The path without its query string.
fn endpoint_path(path: &str) -> &str {
    path.split_once('?').map_or(path, |(p, _)| p)
}

/// Port of settle.isEventStream: the media type of the first Content-Type is text/event-stream.
fn is_event_stream(h: &HeaderMap) -> bool {
    let ct = h
        .get(http::header::CONTENT_TYPE)
        .map(|v| go_lossy_utf8(v.as_bytes()).text.into_owned())
        .unwrap_or_default();
    let ct = ct.split_once(';').map_or(ct.as_str(), |(t, _)| t);
    gojson::go_equal_fold(ct.trim(), "text/event-stream")
}

impl InferenceObserver {
    /// Port of `OnRequest`. The extension stays `None` when the parser does
    /// not claim the request.
    pub fn on_request(path: &str, headers: &HeaderMap, body: &[u8]) -> Self {
        let path = endpoint_path(path).to_owned();
        Self {
            ext: parse_request(&path, headers, body),
            path,
            stream: None,
        }
    }

    /// Port of `OnResponseFrame`. A frame that is not last is one SSE event's
    /// payload. The last frame is empty after a stream, or holds the whole
    /// buffered body.
    pub fn on_response_frame(&mut self, resp_headers: &HeaderMap, frame: &[u8], last: bool) {
        let Self { path, ext, stream } = self;
        let Some(ext) = ext.as_mut() else {
            return;
        };
        if !last {
            stream
                .get_or_insert_with(|| Stream::new(path))
                .fold(frame, ext);
            return;
        }
        // A streamed response whose whole content fitted in one event, given on the last call.
        if stream.is_none()
            && !frame.is_empty()
            && is_event_stream(resp_headers)
            && !carries_sse_framing(frame)
        {
            *stream = Some(Stream::new(path));
        }
        if let Some(s) = stream {
            s.fold(frame, ext);
            s.finish(ext);
        } else if !frame.is_empty() {
            // The listener buffered the response and gave it in one piece.
            let sse = is_event_stream(resp_headers) || carries_sse_framing(frame);
            read_response(path, sse, frame, ext);
        }
        // settleCost runs on every last frame, and it settles the served model.
        settle_served_model(ext, resp_headers);
    }

    pub fn extension(&self) -> Option<&InferenceExtension> {
        self.ext.as_ref()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const PATH: &str = "/v1/messages?beta=true";
    const BODY: &[u8] = br#"{"model":"m","messages":[{"role":"user","content":"hi"}]}"#;

    fn headers(pairs: &[(&'static str, &'static str)]) -> HeaderMap {
        let mut h = HeaderMap::new();
        for (k, v) in pairs {
            h.insert(*k, v.parse().unwrap());
        }
        h
    }

    // #1706: a whole stream in one unframed event, given on the last call.
    #[test]
    fn one_unframed_event_on_the_last_frame_runs_the_stream() {
        let mut o = InferenceObserver::on_request(PATH, &HeaderMap::new(), BODY);
        let h = headers(&[("content-type", "text/event-stream; charset=utf-8")]);
        o.on_response_frame(
            &h,
            br#"{"type":"message_delta","usage":{"output_tokens":7}}"#,
            true,
        );
        let ext = o.extension().unwrap();
        assert_eq!(ext.output_tokens, 7);
        assert!(ext.streamed_response);
    }

    #[test]
    fn non_inference_path_is_ignored() {
        let mut o = InferenceObserver::on_request("/healthz", &HeaderMap::new(), BODY);
        assert!(o.extension().is_none());
        let h = headers(&[("x-litellm-model-name", "x")]);
        o.on_response_frame(&h, b"data: {}", false);
        o.on_response_frame(&h, br#"{"model":"x"}"#, true);
        assert!(o.extension().is_none());
        assert!(o.stream.is_none());
    }

    #[test]
    fn last_frame_settles_served_model_from_the_header() {
        let mut o = InferenceObserver::on_request(PATH, &HeaderMap::new(), BODY);
        let h = headers(&[("x-litellm-model-name", "served-x")]);
        o.on_response_frame(
            &h,
            br#"{"type":"message_start","message":{"model":"body-m"}}"#,
            false,
        );
        // The body's model is kept until the last frame; the header then wins.
        assert_eq!(o.extension().unwrap().served_model, "body-m");
        o.on_response_frame(&h, b"", true);
        assert_eq!(o.extension().unwrap().served_model, "served-x");
    }
}

//! Port of core/parse/inference: parses LLM inference requests and responses
//! (Anthropic Messages, OpenAI Chat Completions, the Responses API) from plain
//! values. The Go parser is the source of truth: each function here has the
//! snake_case name of the Go function it ports.

mod anthropic;
mod dialect;
mod gojson;
mod observer;
mod openai;
mod responses;
mod served_model;
mod subagent;
pub(crate) mod text;
mod usage;

pub use observer::InferenceObserver;
pub use served_model::settle_served_model;

use std::collections::HashMap;

use http::HeaderMap;
use serde_json::value::RawValue;

use crate::parse::decode::decode_request_body;
use crate::parse::model::{InferenceExtension, InferenceThinking, InferenceToolCall};
use crate::parse::sse::normalize_sse;
use dialect::{Dialect, dialect_for};
use text::LossyBody;
use usage::TokenUsage;

/// Parses one inference request. `path` must be query-free. `None` when the
/// parser does not claim the request.
pub fn parse_request(path: &str, headers: &HeaderMap, body: &[u8]) -> Option<InferenceExtension> {
    let d = dialect_for(path);
    if d == Dialect::None {
        return None;
    }
    // Decode once for every dialect: no listener decompresses a request body.
    let body = decode_request_body(headers, body);
    let mut ext = match d {
        Dialect::Anthropic => anthropic::parse_anthropic_request(&body),
        Dialect::OpenAI => openai::parse_openai_request(&body),
        Dialect::Responses => responses::parse_responses_request(&body),
        Dialect::None => None,
    }?;
    ext.agent_role = subagent::agent_role(&ext.messages);
    ext.anthropic_beta = served_model::anthropic_beta(headers);
    Some(ext)
}

/// Reads a whole response body into a fresh extension. The bytes decide the
/// shape: a body framed as SSE is read as a stream. `None` when `path` is not
/// an inference endpoint.
pub fn parse_response(path: &str, body: &[u8]) -> Option<InferenceExtension> {
    let path = path.split_once('?').map_or(path, |(p, _)| p);
    if dialect_for(path) == Dialect::None {
        return None;
    }
    let stream = carries_sse_framing(body);
    let mut ext = InferenceExtension {
        stream,
        ..Default::default()
    };
    read_response(path, stream, body, &mut ext);
    Some(ext)
}

/// Reads a whole response body into `ext`, the extension of its request.
/// `sse` selects the SSE reader, else the JSON reader. `path` must be
/// query-free. A path with no dialect gets the OpenAI readers.
pub fn read_response(path: &str, sse: bool, body: &[u8], ext: &mut InferenceExtension) {
    match (sse, dialect_for(path)) {
        (true, Dialect::Anthropic) => anthropic::parse_anthropic_sse(body, ext),
        (true, Dialect::Responses) => responses::parse_responses_sse(body, ext),
        (true, _) => openai::parse_inference_sse(body, ext),
        (false, Dialect::Anthropic) => anthropic::parse_anthropic_json(body, ext),
        (false, Dialect::Responses) => responses::parse_responses_json(body, ext),
        (false, _) => openai::parse_inference_json(body, ext),
    }
}

/// Folds one response that arrives frame by frame, each frame one SSE event's
/// payload. Fold each frame, then call `finish` once.
pub struct Stream {
    d: Dialect,
    state: InferenceStreamState,
}

impl Stream {
    /// Starts the fold of a streamed response to a request on a query-free `path`.
    pub fn new(path: &str) -> Self {
        Self {
            d: dialect_for(path),
            state: InferenceStreamState::default(),
        }
    }

    /// Folds one frame. It marks `streamed_response` even for an empty frame.
    pub fn fold(&mut self, frame: &[u8], ext: &mut InferenceExtension) {
        ext.streamed_response = true;
        if !frame.is_empty() {
            fold_response_frame(self.d, frame, &mut self.state, ext);
        }
    }

    /// Copies what the stream assembled onto `ext`. A second call writes the
    /// same values again.
    pub fn finish(&mut self, ext: &mut InferenceExtension) {
        self.state.finalize(ext);
    }
}

/// The scratch state of one streamed response.
#[derive(Default)]
pub(crate) struct InferenceStreamState {
    pub(crate) completion: String,
    pub(crate) usage: TokenUsage,
    pub(crate) has_usage: bool,

    /// Anthropic tool calls in emission order. `tools_by_index` and
    /// `open_tool` hold indexes into it.
    pub(crate) tool_calls: Vec<ToolCallState>,
    pub(crate) tools_by_index: HashMap<i64, usize>,
    pub(crate) open_tool: Option<usize>,

    /// The Chat Completions tool calls, keyed by the index each
    /// delta.tool_calls fragment carries. A separate namespace from the
    /// Anthropic block index.
    pub(crate) openai_tool_calls: Vec<ToolCallState>,
    pub(crate) openai_by_index: HashMap<i64, usize>,

    /// The Responses API tool calls, in the order they opened.
    pub(crate) responses_tool_calls: Vec<responses::ResponsesCallState>,

    /// The Anthropic thinking blocks.
    pub(crate) thinking: InferenceThinking,
}

/// One streamed tool call, assembled from its fragments.
#[derive(Default)]
pub(crate) struct ToolCallState {
    pub(crate) id: String,
    pub(crate) name: String,
    pub(crate) args: String,
}

impl InferenceStreamState {
    /// Copies the state onto `ext`. Every write is an assignment, so a second
    /// call does not double-count.
    fn finalize(&self, ext: &mut InferenceExtension) {
        ext.completion = self.completion.clone();
        if self.has_usage {
            self.usage.fill(ext);
        }
        if self.thinking.blocks > 0 {
            ext.response_thinking = Some(self.thinking.clone());
        }
        let calls: Vec<_> = self
            .tool_calls
            .iter()
            .chain(&self.openai_tool_calls)
            .chain(self.responses_tool_calls.iter().map(|c| &c.call))
            .map(|tc| InferenceToolCall {
                id: tc.id.clone(),
                name: tc.name.clone(),
                arguments: tc.args.clone(),
            })
            .collect();
        if calls.is_empty() {
            return;
        }
        ext.tool_calls = calls;
    }
}

/// Reports whether a frame holds raw SSE wire bytes (it has a `data:` field),
/// not one payload with the framing stripped.
pub fn carries_sse_framing(frame: &[u8]) -> bool {
    let frame = normalize_sse(frame);
    let start = frame
        .iter()
        .position(|b| !b" \t\r\n".contains(b))
        .unwrap_or(frame.len());
    frame[start..].starts_with(b"data:") || frame.windows(6).any(|w| w == b"\ndata:")
}

/// Marks `streamed_response` when the bytes carry SSE framing.
pub(crate) fn mark_streamed_response(body: &[u8], ext: &mut InferenceExtension) {
    if carries_sse_framing(body) {
        ext.streamed_response = true;
    }
}

/// Folds one streamed frame through the dialect `d`.
fn fold_response_frame(
    d: Dialect,
    frame: &[u8],
    state: &mut InferenceStreamState,
    ext: &mut InferenceExtension,
) {
    match d {
        Dialect::Anthropic => anthropic::fold_anthropic_frame(frame, state, ext),
        Dialect::Responses => responses::fold_responses_frame(frame, state, ext),
        _ => openai::fold_openai_frame(frame, state, ext),
    }
}

/// Reports whether a field was sent with a value: not absent, not null.
pub(crate) fn json_present(raw: Option<&RawValue>) -> bool {
    raw.is_some_and(|r| !gojson::is_null(r))
}

/// The wire size of a content value, in the original bytes. 0 for absent and null.
pub(crate) fn content_bytes(raw: Option<&RawValue>, body: &LossyBody) -> i64 {
    match raw {
        Some(r) if !gojson::is_null(r) => body.raw_len(r.get()) as i64,
        _ => 0,
    }
}

/// A JSON string's value, or the raw JSON text of any other value. "" when absent or null.
pub(crate) fn raw_text(raw: Option<&RawValue>) -> String {
    let Some(r) = raw.filter(|_| json_present(raw)) else {
        return String::new();
    };
    match gojson::string(r) {
        Ok(Some(s)) => s,
        _ => r.get().to_owned(),
    }
}

/// The text of a content value: a string, or the text parts of an array
/// joined with newlines. "" when absent, null, or with no text part.
pub(crate) fn flatten_content(raw: Option<&RawValue>) -> String {
    let Some(r) = raw.filter(|_| json_present(raw)) else {
        return String::new();
    };
    if let Ok(Some(s)) = gojson::string(r) {
        return s;
    }
    let parts = gojson::slice(r, |e| {
        let (mut typ, mut text) = (String::new(), String::new());
        for (k, v) in gojson::object(e)?.unwrap_or_default() {
            match k.as_str() {
                k if gojson::is(k, "type") => gojson::string_into(&mut typ, v)?,
                k if gojson::is(k, "text") => gojson::string_into(&mut text, v)?,
                _ => {}
            }
        }
        Ok((typ, text))
    });
    let Ok(parts) = parts else {
        return String::new();
    };
    let mut b = String::new();
    for (typ, text) in parts.unwrap_or_default() {
        if typ == "text" && !text.is_empty() {
            if !b.is_empty() {
                b.push('\n');
            }
            b.push_str(&text);
        }
    }
    b
}

/// A tool's JSON schema as it arrived when it is an object, else nothing.
pub(crate) fn schema_object(raw: Option<&RawValue>) -> Option<Box<RawValue>> {
    raw.filter(|r| r.get().starts_with('{'))
        .map(RawValue::to_owned)
}

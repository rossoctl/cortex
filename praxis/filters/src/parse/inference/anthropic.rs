//! Port of core/parse/inference/anthropic.go.

use serde_json::Value;
use serde_json::value::RawValue;

use super::gojson::{self, R, is};
use super::served_model::note_body_model;
use super::text::{LossyBody, go_lossy_utf8, go_trim_space};
use super::usage::{
    KIND_CACHE_READ, KIND_CACHE_WRITE, KIND_INPUT, KIND_OUTPUT, KIND_REASONING, TokenUsage,
};
use super::{
    InferenceStreamState, ToolCallState, content_bytes, flatten_content, mark_streamed_response,
    raw_text, schema_object,
};
use crate::parse::model::{
    InferenceExtension, InferenceMessage, InferenceThinking, InferenceTool, InferenceToolCall,
    InferenceToolResult,
};
use crate::parse::sse::normalize_sse;

// --- request ---

/// The subset of the Anthropic Messages request that is surfaced.
#[derive(Default)]
struct AnthropicRequest<'a> {
    model: String,
    messages: Option<Vec<AnthropicReqMessage>>,
    system: Option<&'a RawValue>,
    temperature: Option<f64>,
    max_tokens: Option<i64>,
    top_p: Option<f64>,
    stream: bool,
    tools: Vec<AnthropicTool<'a>>,
    tool_choice: Option<Value>,
    thinking: Option<&'a RawValue>,
    output_config: Option<&'a RawValue>,
}

fn decode_anthropic_request<'a>(
    raw: &'a RawValue,
    body: &LossyBody,
    req: &mut AnthropicRequest<'a>,
) -> R {
    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
        match k.as_str() {
            k if is(k, "model") => gojson::string_into(&mut req.model, v)?,
            k if is(k, "messages") => {
                req.messages = gojson::slice(v, |e| anthropic_req_message_unmarshal_json(e, body))?
            }
            k if is(k, "system") => req.system = Some(v),
            k if is(k, "temperature") => req.temperature = gojson::float(v)?,
            k if is(k, "max_tokens") => req.max_tokens = gojson::int(v)?,
            k if is(k, "top_p") => req.top_p = gojson::float(v)?,
            k if is(k, "stream") => gojson::bool_into(&mut req.stream, v)?,
            k if is(k, "tools") => {
                req.tools = gojson::slice(v, |e| {
                    let mut t = AnthropicTool::default();
                    decode_anthropic_tool(e, &mut t)?;
                    Ok(t)
                })?
                .unwrap_or_default()
            }
            k if is(k, "tool_choice") => req.tool_choice = gojson::any(v)?,
            k if is(k, "thinking") => req.thinking = Some(v),
            k if is(k, "output_config") => req.output_config = Some(v),
            _ => {}
        }
    }
    Ok(())
}

/// The string value of `key` in the JSON object `raw`, or "" when `raw` is not
/// an object or the value is not a string. It decodes a map, so the key match
/// is exact and the last duplicate wins.
pub(crate) fn string_field(raw: Option<&RawValue>, key: &str) -> String {
    let Some(Ok(Some(entries))) = raw.map(gojson::object) else {
        return String::new();
    };
    let v = entries.into_iter().rev().find(|(k, _)| k == key);
    json_string(v.map(|(_, v)| v))
}

/// The value of the JSON string `raw`, or "" when it is not a string.
pub(crate) fn json_string(raw: Option<&RawValue>) -> String {
    match raw.map(gojson::string) {
        Some(Ok(Some(s))) => s,
        _ => String::new(),
    }
}

/// The length of the JSON string `raw`, or 0 when it is not a string.
fn raw_string_len(raw: Option<&RawValue>) -> i64 {
    json_string(raw).len() as i64
}

/// An Anthropic tool definition.
#[derive(Default)]
struct AnthropicTool<'a> {
    name: String,
    description: String,
    input_schema: Option<&'a RawValue>,
}

fn decode_anthropic_tool<'a>(raw: &'a RawValue, t: &mut AnthropicTool<'a>) -> R {
    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
        match k.as_str() {
            k if is(k, "name") => gojson::string_into(&mut t.name, v)?,
            k if is(k, "description") => gojson::string_into(&mut t.description, v)?,
            k if is(k, "input_schema") => t.input_schema = Some(v),
            _ => {}
        }
    }
    Ok(())
}

/// One request message, its content flattened to text.
#[derive(Default)]
struct AnthropicReqMessage {
    role: String,
    content: String,
    content_bytes: i64,
    tool_results: Vec<InferenceToolResult>,
    tool_calls: Vec<InferenceToolCall>,
    tool_call_names: String,
    tool_use_ids: String,
    thinking: InferenceThinking,
}

/// Port of anthropicReqMessage.UnmarshalJSON.
fn anthropic_req_message_unmarshal_json(
    data: &RawValue,
    body: &LossyBody,
) -> R<AnthropicReqMessage> {
    let mut m = AnthropicReqMessage::default();
    let mut content = None;
    for (k, v) in gojson::object(data)?.unwrap_or_default() {
        match k.as_str() {
            k if is(k, "role") => gojson::string_into(&mut m.role, v)?,
            k if is(k, "content") => content = Some(v),
            _ => {}
        }
    }
    (m.content, m.tool_results, m.tool_calls, m.thinking) = flatten_anthropic_content(content);
    let names: Vec<&str> = m.tool_calls.iter().map(|c| c.name.as_str()).collect();
    let ids: Vec<&str> = m
        .tool_calls
        .iter()
        .map(|c| c.id.as_str())
        .chain(m.tool_results.iter().map(|r| r.tool_use_id.as_str()))
        .collect();
    (m.tool_call_names, m.tool_use_ids) = (names.join(", "), ids.join(", "));
    m.content_bytes = content_bytes(content, body);
    Ok(m)
}

/// One content block of a request message.
#[derive(Default)]
struct ReqBlock<'a> {
    typ: String,
    text: String,
    id: Option<&'a RawValue>,
    name: String,
    input: Option<&'a RawValue>,
    tool_use_id: String,
    content: Option<&'a RawValue>,
    is_error: bool,
    thinking: Option<&'a RawValue>,
    signature: Option<&'a RawValue>,
    data: Option<&'a RawValue>,
}

fn decode_req_block<'a>(raw: &'a RawValue, b: &mut ReqBlock<'a>) -> R {
    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
        match k.as_str() {
            k if is(k, "type") => gojson::string_into(&mut b.typ, v)?,
            k if is(k, "text") => gojson::string_into(&mut b.text, v)?,
            k if is(k, "id") => b.id = Some(v),
            k if is(k, "name") => gojson::string_into(&mut b.name, v)?,
            k if is(k, "input") => b.input = Some(v),
            k if is(k, "tool_use_id") => gojson::string_into(&mut b.tool_use_id, v)?,
            k if is(k, "content") => b.content = Some(v),
            k if is(k, "is_error") => gojson::bool_into(&mut b.is_error, v)?,
            k if is(k, "thinking") => b.thinking = Some(v),
            k if is(k, "signature") => b.signature = Some(v),
            k if is(k, "data") => b.data = Some(v),
            _ => {}
        }
    }
    Ok(())
}

/// `flatten_content` for an Anthropic message, which can also carry
/// tool_result, tool_use and thinking blocks.
fn flatten_anthropic_content(
    raw: Option<&RawValue>,
) -> (
    String,
    Vec<InferenceToolResult>,
    Vec<InferenceToolCall>,
    InferenceThinking,
) {
    let mut thinking = InferenceThinking::default();
    let blocks = raw.map(|r| {
        gojson::slice(r, |e| {
            let mut b = ReqBlock::default();
            decode_req_block(e, &mut b)?;
            Ok(b)
        })
    });
    let Some(Ok(blocks)) = blocks else {
        // A plain string, absent, or a shape not modelled: none of them holds a tool block.
        return (flatten_content(raw), Vec::new(), Vec::new(), thinking);
    };
    let mut text = String::new();
    let mut results = Vec::new();
    let mut calls = Vec::new();
    for b in blocks.unwrap_or_default() {
        thinking.add(
            &b.typ,
            raw_string_len(b.thinking),
            raw_string_len(b.signature),
            raw_string_len(b.data),
        );
        match b.typ.as_str() {
            "text" => {
                if !b.text.is_empty() {
                    if !text.is_empty() {
                        text.push('\n');
                    }
                    text.push_str(&b.text);
                }
            }
            "tool_result" => results.push(InferenceToolResult {
                tool_use_id: b.tool_use_id,
                content: flatten_content(b.content),
                is_error: b.is_error,
            }),
            "tool_use" => calls.push(InferenceToolCall {
                id: raw_text(b.id),
                name: b.name,
                arguments: b.input.map_or_else(String::new, |r| r.get().to_owned()),
            }),
            _ => {}
        }
    }
    (text, results, calls, thinking)
}

/// Builds an extension from an Anthropic Messages request body. `None` for an
/// empty or non-JSON body, or one without a messages array.
pub(super) fn parse_anthropic_request(body: &[u8]) -> Option<InferenceExtension> {
    if body.is_empty() {
        return None;
    }
    let lossy = go_lossy_utf8(body);
    let raw = gojson::parse(&lossy.text).ok()?;
    let mut req = AnthropicRequest::default();
    decode_anthropic_request(raw, &lossy, &mut req).ok()?;

    // The path said Anthropic; the body has to agree.
    let messages = req.messages?;

    let mut ext = InferenceExtension {
        model: req.model,
        temperature: req.temperature,
        max_tokens: req.max_tokens,
        top_p: req.top_p,
        stream: req.stream,
        tool_choice: req.tool_choice,
        is_action: true,
        thinking_type: string_field(req.thinking, "type"),
        thinking_display: string_field(req.thinking, "display"),
        effort: string_field(req.output_config, "effort"),
        ..Default::default()
    };

    // The system prompt is top-level: surface it as a leading system message.
    let sys = flatten_content(req.system);
    if !sys.is_empty() {
        ext.messages.push(InferenceMessage {
            role: "system".into(),
            content: sys,
            content_bytes: content_bytes(req.system, &lossy),
            ..Default::default()
        });
    }
    let mut history = InferenceThinking::default();
    for msg in messages {
        ext.messages.push(InferenceMessage {
            role: msg.role,
            content: msg.content,
            content_bytes: msg.content_bytes,
            tool_call_names: msg.tool_call_names,
            tool_use_ids: msg.tool_use_ids,
            ..Default::default()
        });
        ext.tool_results.extend(msg.tool_results);
        ext.history_tool_calls.extend(msg.tool_calls);
        history.merge(&msg.thinking);
    }
    ext.history_thinking = history.or_nil();
    for tool in req.tools {
        if tool.name.is_empty() {
            continue;
        }
        ext.tools.push(InferenceTool {
            name: tool.name,
            description: tool.description,
            parameters: schema_object(tool.input_schema),
        });
    }
    Some(ext)
}

// --- usage (shared by response + streaming) ---

/// The Messages API usage block. The pointer fields of Go are `Option`s, so
/// an absent field stays absent in `present`.
#[derive(Default)]
struct AnthropicUsage {
    input_tokens: i64,
    output_tokens: i64,
    cache_creation_input_tokens: Option<i64>,
    cache_read_input_tokens: Option<i64>,
    /// `output_tokens_details.thinking_tokens`, a subset of `output_tokens`.
    output_tokens_details: Option<Option<i64>>,
}

fn decode_anthropic_usage(raw: &RawValue, u: &mut AnthropicUsage) -> R {
    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
        match k.as_str() {
            k if is(k, "input_tokens") => gojson::int_into(&mut u.input_tokens, v)?,
            k if is(k, "output_tokens") => gojson::int_into(&mut u.output_tokens, v)?,
            k if is(k, "cache_creation_input_tokens") => {
                u.cache_creation_input_tokens = gojson::int(v)?
            }
            k if is(k, "cache_read_input_tokens") => u.cache_read_input_tokens = gojson::int(v)?,
            k if is(k, "output_tokens_details") => {
                gojson::ptr_into(&mut u.output_tokens_details, v, |raw, t| {
                    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
                        if is(&k, "thinking_tokens") {
                            *t = gojson::int(v)?;
                        }
                    }
                    Ok(())
                })?
            }
            _ => {}
        }
    }
    Ok(())
}

impl AnthropicUsage {
    /// Maps Anthropic's usage onto `TokenUsage`.
    fn to_neutral(&self) -> TokenUsage {
        let mut n = TokenUsage {
            input: self.input_tokens,
            output: self.output_tokens,
            present: KIND_INPUT | KIND_OUTPUT,
            ..Default::default()
        };
        if let Some(c) = self.cache_read_input_tokens {
            n.cache_read = c;
            n.present |= KIND_CACHE_READ;
        }
        if let Some(c) = self.cache_creation_input_tokens {
            n.cache_write = c;
            n.present |= KIND_CACHE_WRITE;
        }
        // Both pointers: a details object without the count reports nothing.
        if let Some(Some(t)) = self.output_tokens_details {
            n.reasoning = t;
            n.present |= KIND_REASONING;
        }
        n
    }
}

// --- non-streaming response ---

#[derive(Default)]
struct AnthropicResponse<'a> {
    model: String,
    content: Vec<AnthropicContentBlock<'a>>,
    stop_reason: String,
    usage: AnthropicUsage,
}

#[derive(Default)]
struct AnthropicContentBlock<'a> {
    typ: String,
    text: String,
    id: String,
    name: String,
    input: Option<&'a RawValue>,
    thinking: Option<&'a RawValue>,
    signature: Option<&'a RawValue>,
    data: Option<&'a RawValue>,
}

fn decode_anthropic_response<'a>(raw: &'a RawValue, r: &mut AnthropicResponse<'a>) -> R {
    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
        match k.as_str() {
            k if is(k, "model") => gojson::string_into(&mut r.model, v)?,
            k if is(k, "content") => {
                r.content = gojson::slice(v, |e| {
                    let mut b = AnthropicContentBlock::default();
                    decode_anthropic_content_block(e, &mut b)?;
                    Ok(b)
                })?
                .unwrap_or_default()
            }
            k if is(k, "stop_reason") => gojson::string_into(&mut r.stop_reason, v)?,
            k if is(k, "usage") => decode_anthropic_usage(v, &mut r.usage)?,
            _ => {}
        }
    }
    Ok(())
}

fn decode_anthropic_content_block<'a>(raw: &'a RawValue, b: &mut AnthropicContentBlock<'a>) -> R {
    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
        match k.as_str() {
            k if is(k, "type") => gojson::string_into(&mut b.typ, v)?,
            k if is(k, "text") => gojson::string_into(&mut b.text, v)?,
            k if is(k, "id") => gojson::string_into(&mut b.id, v)?,
            k if is(k, "name") => gojson::string_into(&mut b.name, v)?,
            k if is(k, "input") => b.input = Some(v),
            k if is(k, "thinking") => b.thinking = Some(v),
            k if is(k, "signature") => b.signature = Some(v),
            k if is(k, "data") => b.data = Some(v),
            _ => {}
        }
    }
    Ok(())
}

/// Parses a non-streaming Messages response: text blocks to the completion,
/// tool_use blocks to tool calls, usage to token counts.
pub(super) fn parse_anthropic_json(body: &[u8], ext: &mut InferenceExtension) {
    let lossy = go_lossy_utf8(body);
    let Ok(raw) = gojson::parse(&lossy.text) else {
        return;
    };
    let mut resp = AnthropicResponse::default();
    if decode_anthropic_response(raw, &mut resp).is_err() {
        return;
    }
    note_body_model(ext, &resp.model);
    let mut b = String::new();
    let mut thinking = InferenceThinking::default();
    for blk in resp.content {
        thinking.add(
            &blk.typ,
            raw_string_len(blk.thinking),
            raw_string_len(blk.signature),
            raw_string_len(blk.data),
        );
        match blk.typ.as_str() {
            "text" => {
                if !blk.text.is_empty() {
                    if !b.is_empty() {
                        b.push('\n');
                    }
                    b.push_str(&blk.text);
                }
            }
            "tool_use" => ext.tool_calls.push(InferenceToolCall {
                id: blk.id,
                name: blk.name,
                arguments: blk.input.map_or_else(String::new, |r| r.get().to_owned()),
            }),
            _ => {}
        }
    }
    ext.completion = b;
    ext.response_thinking = thinking.or_nil();
    if !resp.stop_reason.is_empty() {
        ext.finish_reason = resp.stop_reason;
    }
    resp.usage.to_neutral().fill(ext);
}

// --- streaming ---

/// One SSE event's data payload.
#[derive(Default)]
struct AnthropicStreamEvent<'a> {
    typ: String,
    message: Option<StreamMessage>,
    delta: Option<StreamDelta<'a>>,
    usage: Option<AnthropicUsage>,
    /// Which content block the event belongs to.
    index: Option<i64>,
    content_block: Option<StreamContentBlock<'a>>,
}

#[derive(Default)]
struct StreamMessage {
    model: String,
    usage: AnthropicUsage,
}

#[derive(Default)]
struct StreamDelta<'a> {
    typ: String,
    text: String,
    stop_reason: String,
    partial_json: String,
    thinking: Option<&'a RawValue>,
    signature: Option<&'a RawValue>,
}

#[derive(Default)]
struct StreamContentBlock<'a> {
    typ: String,
    id: String,
    name: String,
    data: Option<&'a RawValue>,
}

fn decode_anthropic_stream_event<'a>(raw: &'a RawValue, ev: &mut AnthropicStreamEvent<'a>) -> R {
    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
        match k.as_str() {
            k if is(k, "type") => gojson::string_into(&mut ev.typ, v)?,
            k if is(k, "message") => gojson::ptr_into(&mut ev.message, v, |raw, m| {
                for (k, v) in gojson::object(raw)?.unwrap_or_default() {
                    match k.as_str() {
                        k if is(k, "model") => gojson::string_into(&mut m.model, v)?,
                        k if is(k, "usage") => decode_anthropic_usage(v, &mut m.usage)?,
                        _ => {}
                    }
                }
                Ok(())
            })?,
            k if is(k, "delta") => gojson::ptr_into(&mut ev.delta, v, |raw, d| {
                for (k, v) in gojson::object(raw)?.unwrap_or_default() {
                    match k.as_str() {
                        k if is(k, "type") => gojson::string_into(&mut d.typ, v)?,
                        k if is(k, "text") => gojson::string_into(&mut d.text, v)?,
                        k if is(k, "stop_reason") => gojson::string_into(&mut d.stop_reason, v)?,
                        k if is(k, "partial_json") => gojson::string_into(&mut d.partial_json, v)?,
                        k if is(k, "thinking") => d.thinking = Some(v),
                        k if is(k, "signature") => d.signature = Some(v),
                        _ => {}
                    }
                }
                Ok(())
            })?,
            k if is(k, "usage") => gojson::ptr_into(&mut ev.usage, v, decode_anthropic_usage)?,
            // Only logged in Go, but a value of the wrong type still fails the decode.
            k if is(k, "error") => {
                let mut e: Option<(String, String)> = None;
                gojson::ptr_into(&mut e, v, |raw, (typ, msg)| {
                    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
                        match k.as_str() {
                            k if is(k, "type") => gojson::string_into(typ, v)?,
                            k if is(k, "message") => gojson::string_into(msg, v)?,
                            _ => {}
                        }
                    }
                    Ok(())
                })?
            }
            k if is(k, "index") => ev.index = gojson::int(v)?,
            k if is(k, "content_block") => gojson::ptr_into(&mut ev.content_block, v, |raw, b| {
                for (k, v) in gojson::object(raw)?.unwrap_or_default() {
                    match k.as_str() {
                        k if is(k, "type") => gojson::string_into(&mut b.typ, v)?,
                        k if is(k, "id") => gojson::string_into(&mut b.id, v)?,
                        k if is(k, "name") => gojson::string_into(&mut b.name, v)?,
                        k if is(k, "data") => b.data = Some(v),
                        _ => {}
                    }
                }
                Ok(())
            })?,
            _ => {}
        }
    }
    Ok(())
}

impl InferenceStreamState {
    /// Starts a tool call for content block `index`.
    fn open_anthropic_tool(&mut self, index: Option<i64>, id: String, name: String) {
        self.tool_calls.push(ToolCallState {
            id,
            name,
            args: String::new(),
        });
        let tc = self.tool_calls.len() - 1;
        if let Some(i) = index {
            self.tools_by_index.insert(i, tc);
        }
        self.open_tool = Some(tc);
    }

    /// The tool call a delta belongs to. No index falls back to the most
    /// recently opened call; an index never opened names none.
    fn anthropic_tool(&mut self, index: Option<i64>) -> Option<&mut ToolCallState> {
        let tc = match index {
            Some(i) => *self.tools_by_index.get(&i)?,
            None => self.open_tool?,
        };
        self.tool_calls.get_mut(tc)
    }

    /// Drops the fallback target at a content_block_stop.
    fn close_anthropic_tool(&mut self) {
        self.open_tool = None;
    }
}

/// Folds one Messages SSE event into the stream state. Unknown events (ping,
/// message_stop) and malformed frames are ignored.
pub(super) fn fold_anthropic_frame(
    frame: &[u8],
    state: &mut InferenceStreamState,
    ext: &mut InferenceExtension,
) {
    let lossy = go_lossy_utf8(frame);
    let Ok(raw) = gojson::parse(&lossy.text) else {
        return;
    };
    let mut ev = AnthropicStreamEvent::default();
    if decode_anthropic_stream_event(raw, &mut ev).is_err() {
        return;
    }
    match ev.typ.as_str() {
        "message_start" => {
            if let Some(m) = &ev.message {
                note_body_model(ext, &m.model);
                merge_anthropic_usage_max_seen(state, m.usage.to_neutral());
                state.has_usage = true;
            }
        }
        "content_block_start" => {
            if let Some(b) = ev.content_block {
                // A tool call opens here; later deltas fill it.
                if b.typ == "tool_use" {
                    state.open_anthropic_tool(ev.index, b.id, b.name);
                }
                // A thinking block's text and signature follow as deltas, which add only bytes.
                state.thinking.add(&b.typ, 0, 0, raw_string_len(b.data));
            }
        }
        "content_block_delta" => {
            let Some(d) = ev.delta else { return };
            match d.typ.as_str() {
                "text_delta" => state.completion.push_str(&d.text),
                "input_json_delta" => {
                    if let Some(tc) = state.anthropic_tool(ev.index) {
                        tc.args.push_str(&d.partial_json);
                    }
                }
                "thinking_delta" => {
                    state.thinking.text_bytes = state
                        .thinking
                        .text_bytes
                        .wrapping_add(raw_string_len(d.thinking))
                }
                "signature_delta" => {
                    state.thinking.signature_bytes = state
                        .thinking
                        .signature_bytes
                        .wrapping_add(raw_string_len(d.signature))
                }
                _ => {}
            }
        }
        "content_block_stop" => state.close_anthropic_tool(),
        "message_delta" => {
            if let Some(d) = &ev.delta
                && !d.stop_reason.is_empty()
            {
                ext.finish_reason = d.stop_reason.clone();
            }
            if let Some(u) = &ev.usage {
                // The ?beta=true path defers cache counts to message_delta: max-seen
                // per sub-field handles both paths.
                let neutral = u.to_neutral();
                merge_anthropic_usage_max_seen(state, neutral);
                if neutral.output > 0 {
                    state.usage.output = neutral.output; // cumulative
                }
                state.has_usage = true;
            }
        }
        // "error" is only logged in Go.
        _ => {}
    }
}

/// Merges every sub-field it owns with max-seen semantics, and unions the
/// present bits. Output is not here: it is cumulative on the wire.
fn merge_anthropic_usage_max_seen(state: &mut InferenceStreamState, incoming: TokenUsage) {
    let u = &mut state.usage;
    u.present |= incoming.present;
    u.reasoning = u.reasoning.max(incoming.reasoning);
    u.input = u.input.max(incoming.input);
    u.cache_read = u.cache_read.max(incoming.cache_read);
    u.cache_write = u.cache_write.max(incoming.cache_write);
}

/// Folds a whole buffered Messages SSE body.
pub(super) fn parse_anthropic_sse(body: &[u8], ext: &mut InferenceExtension) {
    mark_streamed_response(body, ext);
    let mut state = InferenceStreamState::default();
    for line in normalize_sse(body).split(|&b| b == b'\n') {
        let Some(rest) = go_trim_space(line).strip_prefix(b"data:") else {
            continue;
        };
        let data = go_trim_space(rest);
        if data.is_empty() {
            continue;
        }
        fold_anthropic_frame(data, &mut state, ext);
    }
    state.finalize(ext);
}

#[cfg(test)]
mod tests {
    use super::super::{parse_request, parse_response};
    use crate::parse::model::InferenceMessage;
    use http::HeaderMap;
    use serde_json::json;

    #[test]
    fn invalid_utf8_matches_go() {
        let ext = parse_request(
            "/v1/messages",
            &HeaderMap::new(),
            b"{\"model\":\"m\",\"max_tokens\":5,\"messages\":[{\"role\":\"user\",\"content\":\"a\xff\xfeb\"}]}",
        )
        .expect("claimed");
        assert_eq!(ext.model, "m");
        assert_eq!(ext.max_tokens, Some(5));
        assert_eq!(
            ext.messages,
            vec![InferenceMessage {
                role: "user".into(),
                content: "a\u{FFFD}\u{FFFD}b".into(),
                content_bytes: 6,
                ..Default::default()
            }]
        );
    }

    fn req(body: &str) -> serde_json::Value {
        serde_json::to_value(parse_request(
            "/v1/messages",
            &HeaderMap::new(),
            body.as_bytes(),
        ))
        .unwrap()
    }

    // Each expected value is Go's ParseRequest or ParseResponse output, measured on 2026-11-09.
    #[test]
    fn go_json_rules_match_go() {
        assert_eq!(
            req(r#"{"MODEL":"m","meſſages":[{"Role":"user","Content":"hi"}]}"#),
            json!({"model":"m","messages":[{"role":"user","content":"hi","contentBytes":4}],"isAction":true})
        );
        assert_eq!(
            req(r#"{"model":"a","model":"b","messages":[]}"#),
            json!({"model":"b","isAction":true})
        );
        assert_eq!(
            req(r#"{"model":"m","messages":[{"role":"user","content":"\ud800x\udc00😀"}]}"#),
            json!({"model":"m","messages":[{"role":"user","content":"\u{FFFD}x\u{FFFD}\u{1F600}","contentBytes":19}],"isAction":true})
        );
        assert_eq!(
            req(r#"{"model":"m","messages":[null]}"#),
            json!({"model":"m","messages":[{"role":""}],"isAction":true})
        );
        assert_eq!(
            req(r#"{"model":"m","messages":[],"tool_choice":1e400}"#),
            json!(null)
        );
        assert_eq!(
            req(r#"{"model":"m","messages":[],"tool_choice":{"b":1,"a":[2.5,null]}}"#),
            json!({"model":"m","toolChoice":{"a":[2.5,null],"b":1.0},"isAction":true})
        );
        assert_eq!(
            req(r#"{"model":"m","messages":[],"max_tokens":1.0}"#),
            json!(null)
        );
        assert_eq!(
            req(r#"{"model":null,"messages":[],"stream":null,"max_tokens":null}"#),
            json!({"isAction":true})
        );
        assert_eq!(
            req(
                r#"{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"tool_result","tool_use_id":"t","is_error":"yes"}]}]}"#
            ),
            json!({"model":"m","messages":[{"role":"user","content":"a","contentBytes":86}],"isAction":true})
        );
        assert_eq!(
            req(
                r#"{"model":"m","messages":[],"thinking":{"type":"x","type":"enabled","TYPE":"no"}}"#
            ),
            json!({"model":"m","thinkingType":"enabled","isAction":true})
        );
        assert_eq!(
            req(
                r#"{"model":"m","messages":[{"role":"user","content":"x"}],"messages":[{"content":"y"}]}"#
            ),
            json!({"model":"m","messages":[{"role":"","content":"y","contentBytes":3}],"isAction":true})
        );

        let resp = |body: &str| {
            serde_json::to_value(parse_response("/v1/messages", body.as_bytes())).unwrap()
        };
        assert_eq!(
            resp(
                "data: {\"type\":\"message_start\",\"message\":{\"model\":\"x\"},\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n"
            ),
            json!({"stream":true,"servedModel":"x","streamedResponse":true,"promptTokens":3,"totalTokens":3,"inputTokens":3,"presentKinds":9})
        );
        assert_eq!(
            resp(
                "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\u{a0}\n\n"
            ),
            json!({"stream":true,"streamedResponse":true,"completion":"hi"})
        );
    }

    // Go takes a tool_choice 10000 levels deep. The port drops only that field.
    #[test]
    fn deep_tool_choice_keeps_the_rest() {
        let deep = format!("{}{}", "[".repeat(200), "]".repeat(200));
        let got = req(&format!(
            r#"{{"model":"m","messages":[],"tool_choice":{deep}}}"#
        ));
        assert_eq!(got["model"], "m");
        assert!(got.get("toolChoice").is_none());
    }

    // Go's int wraps; the expected values are Go's ParseResponse output, measured on 2026-11-09.
    #[test]
    fn token_overflow_wraps_as_go() {
        let resp = |body: &str| {
            serde_json::to_value(parse_response("/v1/messages", body.as_bytes())).unwrap()
        };
        assert_eq!(
            resp(
                r#"{"model":"m","usage":{"input_tokens":9223372036854775807,"cache_read_input_tokens":1,"output_tokens":1}}"#
            ),
            json!({"servedModel":"m","promptTokens":i64::MIN,"completionTokens":1,"totalTokens":-9223372036854775807_i64,"inputTokens":i64::MAX,"cacheReadTokens":1,"outputTokens":1,"presentKinds":11})
        );
        assert_eq!(
            resp(
                "data: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{\"input_tokens\":9223372036854775807,\"cache_read_input_tokens\":1}}}\n\n"
            ),
            json!({"stream":true,"servedModel":"m","streamedResponse":true,"promptTokens":i64::MIN,"totalTokens":i64::MIN,"inputTokens":i64::MAX,"cacheReadTokens":1,"presentKinds":11})
        );
    }
}

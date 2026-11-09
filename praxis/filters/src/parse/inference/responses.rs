//! Port of core/parse/inference/responses.go: the OpenAI Responses API, which
//! Codex uses.

use serde_json::Value;
use serde_json::value::RawValue;

use super::anthropic::string_field;
use super::gojson::{self, R, is};
use super::served_model::note_body_model;
use super::text::{LossyBody, go_lossy_utf8, go_trim_space};
use super::usage::{
    KIND_CACHE_READ, KIND_CACHE_WRITE, KIND_INPUT, KIND_OUTPUT, KIND_REASONING, TokenUsage,
};
use super::{
    InferenceStreamState, ToolCallState, content_bytes, json_present, mark_streamed_response,
    schema_object,
};
use crate::parse::model::{
    InferenceExtension, InferenceMessage, InferenceTool, InferenceToolCall, InferenceToolResult,
};
use crate::parse::sse::normalize_sse;

// --- request ---

/// The subset of a Responses API request that is read. Go reads no
/// top-level `tools`: the manifest comes from an "additional_tools" input item.
#[derive(Default)]
struct ResponsesRequest<'a> {
    model: String,
    input: Option<&'a RawValue>,
    stream: bool,
    tool_choice: Option<Value>,
    temperature: Option<f64>,
    max_output_tokens: Option<i64>,
    top_p: Option<f64>,
    reasoning: Option<&'a RawValue>,
}

fn decode_responses_request<'a>(raw: &'a RawValue, req: &mut ResponsesRequest<'a>) -> R {
    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
        match k.as_str() {
            k if is(k, "model") => gojson::string_into(&mut req.model, v)?,
            k if is(k, "input") => req.input = Some(v),
            k if is(k, "stream") => gojson::bool_into(&mut req.stream, v)?,
            k if is(k, "tool_choice") => req.tool_choice = gojson::any(v)?,
            k if is(k, "temperature") => req.temperature = gojson::float(v)?,
            k if is(k, "max_output_tokens") => req.max_output_tokens = gojson::int(v)?,
            k if is(k, "top_p") => req.top_p = gojson::float(v)?,
            k if is(k, "reasoning") => req.reasoning = Some(v),
            _ => {}
        }
    }
    Ok(())
}

/// One entry of the request's `input` array, as responsesInputItem.UnmarshalJSON
/// leaves it.
#[derive(Default)]
struct ResponsesInputItem {
    typ: String,
    role: String,
    content: String,
    content_bytes: i64,
    tools: Vec<InferenceTool>,
    tool_results: Vec<InferenceToolResult>,
}

/// Port of responsesInputItem.UnmarshalJSON.
fn responses_input_item_unmarshal_json(data: &RawValue, body: &LossyBody) -> R<ResponsesInputItem> {
    let (mut typ, mut role, mut call_id) = (String::new(), String::new(), String::new());
    let (mut content, mut output) = (None, None);
    let mut namespaces = Vec::new();
    for (k, v) in gojson::object(data)?.unwrap_or_default() {
        match k.as_str() {
            k if is(k, "type") => gojson::string_into(&mut typ, v)?,
            k if is(k, "role") => gojson::string_into(&mut role, v)?,
            k if is(k, "content") => content = Some(v),
            k if is(k, "tools") => {
                namespaces = gojson::slice(v, decode_responses_tool_namespace)?.unwrap_or_default()
            }
            k if is(k, "call_id") => gojson::string_into(&mut call_id, v)?,
            k if is(k, "output") => output = Some(v),
            _ => {}
        }
    }
    let mut it = ResponsesInputItem::default();
    if typ == "message" {
        it.content = flatten_responses_content(content);
        it.content_bytes = content_bytes(content, body);
    }
    for t in namespaces.into_iter().flatten() {
        if t.name.is_empty() {
            continue;
        }
        it.tools.push(InferenceTool {
            name: t.name,
            description: t.description,
            parameters: schema_object(t.parameters),
        });
    }
    if typ == "function_call_output" {
        it.tool_results.push(InferenceToolResult {
            tool_use_id: call_id,
            content: flatten_responses_content(output),
            ..Default::default()
        });
    }
    (it.typ, it.role) = (typ, role);
    Ok(it)
}

/// One real tool definition inside a namespace.
#[derive(Default)]
struct ResponsesToolDef<'a> {
    name: String,
    description: String,
    parameters: Option<&'a RawValue>,
}

/// Decodes a responsesToolNamespace into the tool definitions it holds. Its
/// `type` and `name` are read only so that a non-string fails the decode.
fn decode_responses_tool_namespace(raw: &RawValue) -> R<Vec<ResponsesToolDef<'_>>> {
    let (mut typ, mut name) = (String::new(), String::new());
    let mut tools = Vec::new();
    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
        match k.as_str() {
            k if is(k, "type") => gojson::string_into(&mut typ, v)?,
            k if is(k, "name") => gojson::string_into(&mut name, v)?,
            k if is(k, "tools") => {
                tools = gojson::slice(v, |e| {
                    let mut t = ResponsesToolDef::default();
                    let mut typ = String::new();
                    for (k, v) in gojson::object(e)?.unwrap_or_default() {
                        match k.as_str() {
                            k if is(k, "type") => gojson::string_into(&mut typ, v)?,
                            k if is(k, "name") => gojson::string_into(&mut t.name, v)?,
                            k if is(k, "description") => {
                                gojson::string_into(&mut t.description, v)?
                            }
                            k if is(k, "parameters") => t.parameters = Some(v),
                            _ => {}
                        }
                    }
                    Ok(t)
                })?
                .unwrap_or_default()
            }
            _ => {}
        }
    }
    Ok(tools)
}

/// The text of a Responses content value: a string, or every non-empty `text`
/// of a part array joined with newlines, whatever the part's type. "" otherwise.
fn flatten_responses_content(raw: Option<&RawValue>) -> String {
    let Some(r) = raw.filter(|_| json_present(raw)) else {
        return String::new();
    };
    if let Ok(Some(s)) = gojson::string(r) {
        return s;
    }
    let Ok(parts) = gojson::slice(r, decode_text_part) else {
        return String::new();
    };
    join_texts(parts.unwrap_or_default())
}

/// Decodes a `struct{ Text string }` part.
fn decode_text_part(raw: &RawValue) -> R<String> {
    let mut text = String::new();
    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
        if is(&k, "text") {
            gojson::string_into(&mut text, v)?;
        }
    }
    Ok(text)
}

/// Joins the non-empty texts with newlines.
fn join_texts(texts: impl IntoIterator<Item = String>) -> String {
    let mut b = String::new();
    for t in texts {
        if t.is_empty() {
            continue;
        }
        if !b.is_empty() {
            b.push('\n');
        }
        b.push_str(&t);
    }
    b
}

/// Builds an extension from a Responses API request body. `None` for an empty
/// or non-JSON body, or one without an input array or string.
pub(super) fn parse_responses_request(body: &[u8]) -> Option<InferenceExtension> {
    if body.is_empty() {
        return None;
    }
    let lossy = go_lossy_utf8(body);
    let raw = gojson::parse(&lossy.text).ok()?;
    let mut req = ResponsesRequest::default();
    decode_responses_request(raw, &mut req).ok()?;

    // The array first, then the string shorthand for one user message. An
    // absent input fails both, as Go's Unmarshal of no bytes does.
    let input = req.input?;
    let input = match gojson::slice(input, |e| responses_input_item_unmarshal_json(e, &lossy)) {
        Ok(items) => items?,
        Err(_) => {
            let text = gojson::string(input).ok()??;
            vec![ResponsesInputItem {
                typ: "message".into(),
                role: "user".into(),
                content: text,
                content_bytes: content_bytes(Some(input), &lossy),
                ..Default::default()
            }]
        }
    };
    let mut ext = InferenceExtension {
        model: req.model,
        temperature: req.temperature,
        max_tokens: req.max_output_tokens,
        top_p: req.top_p,
        stream: req.stream,
        tool_choice: req.tool_choice,
        is_action: true,
        effort: string_field(req.reasoning, "effort"),
        ..Default::default()
    };
    for item in input {
        if item.typ == "message" {
            ext.messages.push(InferenceMessage {
                role: item.role,
                content: item.content,
                content_bytes: item.content_bytes,
                ..Default::default()
            });
        }
        ext.tools.extend(item.tools);
        // Tool results ride whichever item carried them, message or not.
        ext.tool_results.extend(item.tool_results);
    }
    Some(ext)
}

// --- usage ---

/// The Responses API usage block. `Option` keeps "key absent" apart from "key
/// present with 0", as Go's pointers do.
#[derive(Default)]
struct ResponsesUsage {
    input_tokens: Option<i64>,
    output_tokens: Option<i64>,
    total_tokens: Option<i64>,
    input_tokens_details: Option<InputTokensDetails>,
    /// `output_tokens_details.reasoning_tokens`.
    output_tokens_details: Option<i64>,
}

#[derive(Default)]
struct InputTokensDetails {
    cached_tokens: i64,
    cache_write_tokens: i64,
}

fn decode_responses_usage(raw: &RawValue, u: &mut ResponsesUsage) -> R {
    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
        match k.as_str() {
            k if is(k, "input_tokens") => u.input_tokens = gojson::int(v)?,
            k if is(k, "output_tokens") => u.output_tokens = gojson::int(v)?,
            k if is(k, "total_tokens") => u.total_tokens = gojson::int(v)?,
            k if is(k, "input_tokens_details") => {
                gojson::ptr_into(&mut u.input_tokens_details, v, |raw, d| {
                    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
                        match k.as_str() {
                            k if is(k, "cached_tokens") => {
                                gojson::int_into(&mut d.cached_tokens, v)?
                            }
                            k if is(k, "cache_write_tokens") => {
                                gojson::int_into(&mut d.cache_write_tokens, v)?
                            }
                            _ => {}
                        }
                    }
                    Ok(())
                })?
            }
            k if is(k, "output_tokens_details") => {
                gojson::ptr_into(&mut u.output_tokens_details, v, |raw, n| {
                    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
                        if is(&k, "reasoning_tokens") {
                            gojson::int_into(n, v)?;
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

impl ResponsesUsage {
    fn has_any(&self) -> bool {
        self.input_tokens.is_some()
            || self.output_tokens.is_some()
            || self.total_tokens.is_some()
            || self.input_tokens_details.is_some()
            || self.output_tokens_details.is_some()
    }

    /// Maps the usage onto `TokenUsage`. input_tokens includes both
    /// cached_tokens and cache_write_tokens: subtract them, and clamp at 0.
    fn to_neutral(&self) -> TokenUsage {
        let mut usage = TokenUsage::default();
        if let Some(t) = self.total_tokens {
            usage.reported_total = t;
        }
        if let Some(i) = self.input_tokens {
            usage.input = i;
            usage.present |= KIND_INPUT;
        }
        if let Some(o) = self.output_tokens {
            usage.output = o;
            usage.present |= KIND_OUTPUT;
        }
        if let Some(d) = &self.input_tokens_details {
            usage.input = usage
                .input
                .wrapping_sub(d.cached_tokens.wrapping_add(d.cache_write_tokens));
            if usage.input < 0 {
                usage.input = 0;
            }
            usage.cache_read = d.cached_tokens;
            usage.present |= KIND_CACHE_READ;
            usage.cache_write = d.cache_write_tokens;
            usage.present |= KIND_CACHE_WRITE;
        }
        if let Some(r) = self.output_tokens_details {
            usage.reasoning = r;
            usage.present |= KIND_REASONING;
        }
        usage
    }
}

// --- non-streaming response ---

#[derive(Default)]
struct ResponsesNonStreaming {
    model: String,
    status: String,
    usage: ResponsesUsage,
    output: Vec<ResponsesOutputItem>,
}

/// One item of a response's output.
#[derive(Default)]
struct ResponsesOutputItem {
    id: String,
    typ: String,
    call_id: String,
    name: String,
    /// custom_tool_call's freeform argument text.
    input: String,
    /// function_call's JSON-schema arguments.
    arguments: String,
    /// A "message" item's text parts: the non-streaming path only.
    content: Vec<String>,
}

fn decode_responses_output_item(raw: &RawValue, it: &mut ResponsesOutputItem) -> R {
    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
        match k.as_str() {
            k if is(k, "id") => gojson::string_into(&mut it.id, v)?,
            k if is(k, "type") => gojson::string_into(&mut it.typ, v)?,
            k if is(k, "call_id") => gojson::string_into(&mut it.call_id, v)?,
            k if is(k, "name") => gojson::string_into(&mut it.name, v)?,
            k if is(k, "input") => gojson::string_into(&mut it.input, v)?,
            k if is(k, "arguments") => gojson::string_into(&mut it.arguments, v)?,
            k if is(k, "content") => {
                it.content = gojson::slice(v, decode_text_part)?.unwrap_or_default()
            }
            _ => {}
        }
    }
    Ok(())
}

fn decode_responses_non_streaming(raw: &RawValue, r: &mut ResponsesNonStreaming) -> R {
    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
        match k.as_str() {
            k if is(k, "model") => gojson::string_into(&mut r.model, v)?,
            k if is(k, "status") => gojson::string_into(&mut r.status, v)?,
            k if is(k, "usage") => decode_responses_usage(v, &mut r.usage)?,
            k if is(k, "output") => {
                r.output = gojson::slice(v, |e| {
                    let mut it = ResponsesOutputItem::default();
                    decode_responses_output_item(e, &mut it)?;
                    Ok(it)
                })?
                .unwrap_or_default()
            }
            _ => {}
        }
    }
    Ok(())
}

/// Parses a non-streaming Responses API response.
pub(super) fn parse_responses_json(body: &[u8], ext: &mut InferenceExtension) {
    let lossy = go_lossy_utf8(body);
    let Ok(raw) = gojson::parse(&lossy.text) else {
        return;
    };
    let mut resp = ResponsesNonStreaming::default();
    if decode_responses_non_streaming(raw, &mut resp).is_err() {
        return;
    }
    note_body_model(ext, &resp.model);
    let mut calls = Vec::new();
    for item in &resp.output {
        match item.typ.as_str() {
            "custom_tool_call" => calls.push(InferenceToolCall {
                id: item.call_id.clone(),
                name: item.name.clone(),
                arguments: item.input.clone(),
            }),
            "function_call" => calls.push(InferenceToolCall {
                id: item.call_id.clone(),
                name: item.name.clone(),
                arguments: item.arguments.clone(),
            }),
            _ => {}
        }
    }
    ext.completion = join_texts(resp.output.into_iter().flat_map(|i| i.content));
    ext.tool_calls = calls;
    if !resp.status.is_empty() {
        ext.finish_reason = resp.status;
    }
    if resp.usage.has_any() {
        resp.usage.to_neutral().fill(ext);
    }
}

// --- streaming ---

/// One SSE event's data payload.
#[derive(Default)]
struct ResponsesStreamEvent {
    typ: String,
    delta: Option<String>,
    /// The snapshot on response.completed, .incomplete and .failed.
    response: Option<ResponsesSnapshot>,
    /// The item on response.output_item.added and .done.
    item: Option<ResponsesOutputItem>,
    item_id: String,
    output_index: Option<i64>,
}

#[derive(Default)]
struct ResponsesSnapshot {
    model: String,
    status: String,
    usage: ResponsesUsage,
}

fn decode_responses_stream_event(raw: &RawValue, ev: &mut ResponsesStreamEvent) -> R {
    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
        match k.as_str() {
            k if is(k, "type") => gojson::string_into(&mut ev.typ, v)?,
            k if is(k, "delta") => ev.delta = gojson::string(v)?,
            k if is(k, "response") => gojson::ptr_into(&mut ev.response, v, |raw, s| {
                for (k, v) in gojson::object(raw)?.unwrap_or_default() {
                    match k.as_str() {
                        k if is(k, "model") => gojson::string_into(&mut s.model, v)?,
                        k if is(k, "status") => gojson::string_into(&mut s.status, v)?,
                        k if is(k, "usage") => decode_responses_usage(v, &mut s.usage)?,
                        _ => {}
                    }
                }
                Ok(())
            })?,
            k if is(k, "item") => gojson::ptr_into(&mut ev.item, v, decode_responses_output_item)?,
            k if is(k, "item_id") => gojson::string_into(&mut ev.item_id, v)?,
            k if is(k, "output_index") => ev.output_index = gojson::int(v)?,
            _ => {}
        }
    }
    Ok(())
}

/// One tool call of a Responses API stream: opened by output_item.added, grown
/// by its argument deltas, and replaced whole by output_item.done.
#[derive(Default)]
pub(crate) struct ResponsesCallState {
    pub(crate) call: ToolCallState,
    item_id: String,
    index: Option<i64>,
    done: bool,
}

impl InferenceStreamState {
    /// Port of responsesCall: the index of the open call that `ev` concerns,
    /// by its item id, output index or call id. An event with neither id nor
    /// index matches the oldest call that has neither and no done yet.
    fn responses_call(&self, ev: &ResponsesStreamEvent) -> Option<usize> {
        let (mut id, mut call_id) = (ev.item_id.as_str(), "");
        if let Some(item) = &ev.item {
            call_id = &item.call_id;
            if !item.id.is_empty() {
                id = &item.id;
            }
        }
        self.responses_tool_calls.iter().position(|c| {
            (!id.is_empty() && c.item_id == id)
                || (ev.output_index.is_some() && c.index == ev.output_index)
                || (!call_id.is_empty() && c.call.id == call_id)
                || (id.is_empty()
                    && ev.output_index.is_none()
                    && c.item_id.is_empty()
                    && c.index.is_none()
                    && !c.done)
        })
    }
}

/// The arguments so far when `item` carries a tool call, else `None`.
fn responses_tool_item(item: &ResponsesOutputItem) -> Option<&str> {
    match item.typ.as_str() {
        "custom_tool_call" => Some(&item.input),
        "function_call" => Some(&item.arguments),
        _ => None,
    }
}

/// Folds one Responses API SSE event into the stream state.
pub(super) fn fold_responses_frame(
    frame: &[u8],
    state: &mut InferenceStreamState,
    ext: &mut InferenceExtension,
) {
    let lossy = go_lossy_utf8(frame);
    let Ok(raw) = gojson::parse(&lossy.text) else {
        return;
    };
    let mut ev = ResponsesStreamEvent::default();
    if decode_responses_stream_event(raw, &mut ev).is_err() {
        return;
    }
    if let Some(r) = &ev.response {
        note_body_model(ext, &r.model);
    }
    match ev.typ.as_str() {
        "response.output_text.delta" => {
            if let Some(d) = &ev.delta {
                state.completion.push_str(d);
            }
        }
        "response.output_item.added" | "response.output_item.done" => {
            let Some(item) = &ev.item else { return };
            let Some(args) = responses_tool_item(item) else {
                return;
            };
            let done = ev.typ == "response.output_item.done";
            let found = state.responses_call(&ev).filter(|&i| {
                let c = &state.responses_tool_calls[i];
                done || !c.item_id.is_empty() || c.index.is_some() || !item.call_id.is_empty()
            });
            let i = found.unwrap_or_else(|| {
                state.responses_tool_calls.push(ResponsesCallState {
                    item_id: item.id.clone(),
                    index: ev.output_index,
                    ..Default::default()
                });
                state.responses_tool_calls.len() - 1
            });
            let call = &mut state.responses_tool_calls[i];
            call.done = done;
            if call.item_id.is_empty() {
                call.item_id.clone_from(&item.id);
            }
            if call.index.is_none() {
                call.index = ev.output_index;
            }
            call.call.id.clone_from(&item.call_id);
            call.call.name.clone_from(&item.name);
            // Only a done restates the whole call, so only it replaces what the deltas built.
            if done || call.call.args.is_empty() {
                call.call.args = args.to_owned();
            }
        }
        "response.function_call_arguments.delta" | "response.custom_tool_call_input.delta" => {
            let Some(d) = &ev.delta else { return };
            if let Some(i) = state.responses_call(&ev) {
                state.responses_tool_calls[i].call.args.push_str(d);
            }
        }
        "response.completed" | "response.incomplete" | "response.failed" => {
            let Some(r) = &ev.response else { return };
            if !r.status.is_empty() {
                ext.finish_reason.clone_from(&r.status);
            }
            if r.usage.has_any() {
                state.usage = r.usage.to_neutral();
                state.has_usage = true;
            }
        }
        _ => {}
    }
}

/// Folds a whole buffered Responses API SSE body.
pub(super) fn parse_responses_sse(body: &[u8], ext: &mut InferenceExtension) {
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
        fold_responses_frame(data, &mut state, ext);
    }
    state.finalize(ext);
}

#[cfg(test)]
mod tests {
    use super::super::{parse_request, parse_response};
    use http::HeaderMap;
    use serde_json::{Value, json};

    const PATH: &str = "/v1/responses";

    fn req(body: &str) -> Value {
        serde_json::to_value(parse_request(PATH, &HeaderMap::new(), body.as_bytes())).unwrap()
    }

    fn resp(body: &str) -> Value {
        serde_json::to_value(parse_response(PATH, body.as_bytes())).unwrap()
    }

    // Each expected value is Go's ParseRequest or ParseResponse output, measured on 2026-11-09.
    #[test]
    fn go_rules_match_go() {
        assert_eq!(
            req(
                r#"{"model":"m","input":"hi","tools":[{"type":"function","name":"x"}],"reasoning":{"effort":"high"},"max_output_tokens":9}"#
            ),
            json!({"model":"m","messages":[{"role":"user","content":"hi","contentBytes":4}],"maxTokens":9,"effort":"high","isAction":true})
        );
        assert_eq!(req(r#"{"model":"m","input":null}"#), json!(null));
        assert_eq!(req(r#"{"model":"m"}"#), json!(null));
        assert_eq!(req(r#"{"model":"m","input":[{"type":5}]}"#), json!(null));
        assert_eq!(
            req(
                r#"{"model":"m","input":[null,{"type":"function_call","call_id":"c"},{"type":"function_call_output","call_id":"c","output":[{"type":"output_text","text":"a"},{"text":"b"}]},{"type":"message","role":"user","content":[{"type":"input_image"},{"type":"input_text","text":"q"}]}]}"#
            ),
            json!({"model":"m","messages":[{"role":"user","content":"q","contentBytes":57}],"toolResults":[{"toolUseId":"c","content":"a\nb"}],"isAction":true})
        );
        assert_eq!(
            req(
                r#"{"model":"m","input":[{"type":"message","tools":[{"tools":[{"name":"t","parameters":null},{"name":""}]}]}]}"#
            ),
            json!({"model":"m","messages":[{"role":""}],"tools":[{"name":"t"}],"isAction":true})
        );

        assert_eq!(
            resp(
                r#"{"model":"x","status":"incomplete","output":[{"type":"custom_tool_call","call_id":"c","name":"exec","input":"ls"},{"type":"message","content":[{"text":"a"},{"text":""},{"text":"b"}]}],"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":9}}}"#
            ),
            json!({"servedModel":"x","completion":"a\nb","finishReason":"incomplete","toolCalls":[{"id":"c","name":"exec","arguments":"ls"}],"promptTokens":12,"totalTokens":12,"cacheReadTokens":3,"cacheWriteTokens":9,"presentKinds":7})
        );
        assert_eq!(resp(r#"{"output":[{"content":"s"}]}"#), json!({}));
        // Calls with no id and no index: a new added opens a new call, and a
        // delta goes to the oldest call without a done.
        assert_eq!(
            resp(
                "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"custom_tool_call\",\"name\":\"exec\",\"input\":\"\"}}\n\ndata: {\"type\":\"response.custom_tool_call_input.delta\",\"delta\":\"l\"}\n\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"custom_tool_call\",\"name\":\"exec2\"}}\n\ndata: {\"type\":\"response.custom_tool_call_input.delta\",\"delta\":\"s\"}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"model\":\"y\",\"status\":\"failed\",\"usage\":null}}\n\n"
            ),
            json!({"stream":true,"servedModel":"y","streamedResponse":true,"finishReason":"failed","toolCalls":[{"name":"exec","arguments":"ls"},{"name":"exec2"}]})
        );
        // The done finds the call by call id and replaces what the deltas built.
        assert_eq!(
            resp(
                "data: {\"type\":\"response.output_item.added\",\"output_index\":2,\"item\":{\"type\":\"function_call\",\"call_id\":\"k\",\"name\":\"f\",\"arguments\":\"{\"}}\n\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"zz\",\"output_index\":2,\"delta\":\"x\"}\n\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"call_id\":\"k\",\"name\":\"f\",\"arguments\":\"{}\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"output_tokens_details\":{\"reasoning_tokens\":4}}}}\n\n"
            ),
            json!({"stream":true,"streamedResponse":true,"finishReason":"completed","toolCalls":[{"id":"k","name":"f","arguments":"{}"}],"reasoningTokens":4,"presentKinds":16})
        );
    }

    // Go takes a tool_choice 10000 levels deep. The port drops only that field.
    #[test]
    fn deep_tool_choice_keeps_the_rest() {
        let deep = format!("{}{}", "[".repeat(200), "]".repeat(200));
        let got = req(&format!(
            r#"{{"model":"m","input":"hi","tool_choice":{deep}}}"#
        ));
        assert_eq!(got["model"], "m");
        assert!(got.get("toolChoice").is_none());
    }
}

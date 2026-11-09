//! Port of the Chat Completions half of core/parse/inference/inference.go,
//! including the legacy `/completions` request shape.

use serde_json::Value;
use serde_json::value::RawValue;

use super::anthropic::json_string;
use super::gojson::{self, R, is};
use super::served_model::note_body_model;
use super::text::{LossyBody, go_lossy_utf8, go_trim_space};
use super::usage::{KIND_CACHE_READ, KIND_INPUT, KIND_OUTPUT, KIND_REASONING, TokenUsage};
use super::{
    InferenceStreamState, ToolCallState, content_bytes, flatten_content, json_present,
    mark_streamed_response, raw_text, schema_object,
};
use crate::parse::model::{
    InferenceExtension, InferenceMessage, InferenceTool, InferenceToolCall, InferenceToolResult,
};
use crate::parse::sse::normalize_sse;

// --- request ---

/// The subset of the chat/completions (or completions) request that is read.
#[derive(Default)]
struct InferenceRequest<'a> {
    model: String,
    messages: Option<Vec<InferenceReqMessage>>,
    temperature: Option<f64>,
    max_tokens: Option<i64>,
    top_p: Option<f64>,
    stream: bool,
    tools: Vec<InferenceToolDef<'a>>,
    tool_choice: Option<Value>,
    /// Read only to tell a legacy completions request from a body that is not inference.
    prompt: Option<&'a RawValue>,
    reasoning_effort: Option<&'a RawValue>,
}

fn decode_inference_request<'a>(
    raw: &'a RawValue,
    body: &LossyBody,
    req: &mut InferenceRequest<'a>,
) -> R {
    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
        match k.as_str() {
            k if is(k, "model") => gojson::string_into(&mut req.model, v)?,
            k if is(k, "messages") => {
                req.messages = gojson::slice(v, |e| inference_message_unmarshal_json(e, body))?
            }
            k if is(k, "temperature") => req.temperature = gojson::float(v)?,
            k if is(k, "max_tokens") => req.max_tokens = gojson::int(v)?,
            k if is(k, "top_p") => req.top_p = gojson::float(v)?,
            k if is(k, "stream") => gojson::bool_into(&mut req.stream, v)?,
            k if is(k, "tools") => {
                req.tools = gojson::slice(v, |e| {
                    let mut t = InferenceToolDef::default();
                    decode_inference_tool(e, &mut t)?;
                    Ok(t)
                })?
                .unwrap_or_default()
            }
            k if is(k, "tool_choice") => req.tool_choice = gojson::any(v)?,
            k if is(k, "prompt") => req.prompt = Some(v),
            k if is(k, "reasoning_effort") => req.reasoning_effort = Some(v),
            _ => {}
        }
    }
    Ok(())
}

/// One request message: the content flattened to text, and the tool calls of
/// an assistant message.
#[derive(Default)]
struct InferenceReqMessage {
    role: String,
    content: String,
    content_bytes: i64,
    /// The tool_call_id of a "role":"tool" result message.
    tool_call_id: String,
    tool_call_names: String,
    tool_use_ids: String,
    tool_calls: Vec<InferenceToolCall>,
}

/// One entry of a request message's tool_calls array.
#[derive(Default)]
struct ReqToolCall<'a> {
    id: Option<&'a RawValue>,
    name: String,
    arguments: Option<&'a RawValue>,
}

/// Port of inferenceMessage.UnmarshalJSON.
fn inference_message_unmarshal_json(data: &RawValue, body: &LossyBody) -> R<InferenceReqMessage> {
    let mut m = InferenceReqMessage::default();
    let mut content = None;
    let mut calls = Vec::new();
    for (k, v) in gojson::object(data)?.unwrap_or_default() {
        match k.as_str() {
            k if is(k, "role") => gojson::string_into(&mut m.role, v)?,
            k if is(k, "content") => content = Some(v),
            k if is(k, "tool_call_id") => gojson::string_into(&mut m.tool_call_id, v)?,
            k if is(k, "tool_calls") => {
                calls = gojson::slice(v, |e| {
                    let mut c = ReqToolCall::default();
                    for (k, v) in gojson::object(e)?.unwrap_or_default() {
                        match k.as_str() {
                            k if is(k, "id") => c.id = Some(v),
                            // A non-pointer struct: a second "function" merges into the first.
                            k if is(k, "function") => {
                                for (k, v) in gojson::object(v)?.unwrap_or_default() {
                                    match k.as_str() {
                                        k if is(k, "name") => gojson::string_into(&mut c.name, v)?,
                                        k if is(k, "arguments") => c.arguments = Some(v),
                                        _ => {}
                                    }
                                }
                            }
                            _ => {}
                        }
                    }
                    Ok(c)
                })?
                .unwrap_or_default()
            }
            _ => {}
        }
    }
    m.content = flatten_content(content);
    m.content_bytes = content_bytes(content, body);
    let mut names = Vec::with_capacity(calls.len());
    let mut ids = Vec::with_capacity(calls.len());
    for c in calls {
        let id = raw_text(c.id);
        names.push(c.name.clone());
        ids.push(id.clone());
        m.tool_calls.push(InferenceToolCall {
            id,
            name: c.name,
            arguments: raw_text(c.arguments),
        });
    }
    (m.tool_call_names, m.tool_use_ids) = (names.join(", "), ids.join(", "));
    Ok(m)
}

/// An OpenAI tool definition. `typ` is read only so that a non-string fails
/// the decode, as in Go.
#[derive(Default)]
struct InferenceToolDef<'a> {
    typ: String,
    name: String,
    description: String,
    parameters: Option<&'a RawValue>,
}

fn decode_inference_tool<'a>(raw: &'a RawValue, t: &mut InferenceToolDef<'a>) -> R {
    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
        match k.as_str() {
            k if is(k, "type") => gojson::string_into(&mut t.typ, v)?,
            k if is(k, "function") => {
                for (k, v) in gojson::object(v)?.unwrap_or_default() {
                    match k.as_str() {
                        k if is(k, "name") => gojson::string_into(&mut t.name, v)?,
                        k if is(k, "description") => gojson::string_into(&mut t.description, v)?,
                        k if is(k, "parameters") => t.parameters = Some(v),
                        _ => {}
                    }
                }
            }
            _ => {}
        }
    }
    Ok(())
}

/// Builds an extension from a chat/completions (or completions) request body.
/// `None` for an empty or non-JSON body, and for one that is not an inference
/// request: it has no messages array and no legacy prompt.
pub(super) fn parse_openai_request(body: &[u8]) -> Option<InferenceExtension> {
    if body.is_empty() {
        return None;
    }
    let lossy = go_lossy_utf8(body);
    let raw = gojson::parse(&lossy.text).ok()?;
    let mut req = InferenceRequest::default();
    decode_inference_request(raw, &lossy, &mut req).ok()?;

    // The path said OpenAI; the body has to agree. A model is not required.
    if req.messages.is_none() && !json_present(req.prompt) {
        return None;
    }
    let mut ext = InferenceExtension {
        model: req.model,
        temperature: req.temperature,
        max_tokens: req.max_tokens,
        top_p: req.top_p,
        stream: req.stream,
        tool_choice: req.tool_choice,
        is_action: true,
        effort: json_string(req.reasoning_effort),
        ..Default::default()
    };
    for msg in req.messages.unwrap_or_default() {
        // A "role":"tool" message is the result of the call it names.
        if msg.role == "tool" && !msg.tool_call_id.is_empty() {
            ext.tool_results.push(InferenceToolResult {
                tool_use_id: msg.tool_call_id.clone(),
                content: msg.content.clone(),
                ..Default::default()
            });
        }
        ext.messages.push(InferenceMessage {
            role: msg.role,
            content: msg.content,
            content_bytes: msg.content_bytes,
            tool_use_id: msg.tool_call_id,
            tool_call_names: msg.tool_call_names,
            tool_use_ids: msg.tool_use_ids,
        });
        ext.history_tool_calls.extend(msg.tool_calls);
    }
    for tool in req.tools {
        if tool.name.is_empty() {
            continue;
        }
        ext.tools.push(InferenceTool {
            name: tool.name,
            description: tool.description,
            parameters: schema_object(tool.parameters),
        });
    }
    Some(ext)
}

// --- usage ---

/// The OpenAI usage block. `Option` keeps "key absent" apart from "key
/// present with 0", as Go's pointers do.
#[derive(Default)]
struct InferenceUsage {
    prompt_tokens: Option<i64>,
    completion_tokens: Option<i64>,
    total_tokens: Option<i64>,
    /// `prompt_tokens_details.cached_tokens`.
    prompt_tokens_details: Option<i64>,
    /// `completion_tokens_details.reasoning_tokens`.
    completion_tokens_details: Option<i64>,
}

/// Decodes a Go `*struct{ X int }` whose one field is `name`.
fn decode_details(dst: &mut Option<i64>, raw: &RawValue, name: &str) -> R {
    gojson::ptr_into(dst, raw, |raw, n| {
        for (k, v) in gojson::object(raw)?.unwrap_or_default() {
            if is(&k, name) {
                gojson::int_into(n, v)?;
            }
        }
        Ok(())
    })
}

fn decode_inference_usage(raw: &RawValue, u: &mut InferenceUsage) -> R {
    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
        match k.as_str() {
            k if is(k, "prompt_tokens") => u.prompt_tokens = gojson::int(v)?,
            k if is(k, "completion_tokens") => u.completion_tokens = gojson::int(v)?,
            k if is(k, "total_tokens") => u.total_tokens = gojson::int(v)?,
            k if is(k, "prompt_tokens_details") => {
                decode_details(&mut u.prompt_tokens_details, v, "cached_tokens")?
            }
            k if is(k, "completion_tokens_details") => {
                decode_details(&mut u.completion_tokens_details, v, "reasoning_tokens")?
            }
            _ => {}
        }
    }
    Ok(())
}

impl InferenceUsage {
    /// Reports whether any recognized field was on the wire.
    fn has_any(&self) -> bool {
        self.prompt_tokens.is_some()
            || self.completion_tokens.is_some()
            || self.total_tokens.is_some()
            || self.prompt_tokens_details.is_some()
            || self.completion_tokens_details.is_some()
    }

    /// Maps OpenAI's usage onto `TokenUsage`. prompt_tokens includes
    /// cached_tokens: subtract it, and clamp at 0.
    fn to_neutral(&self) -> TokenUsage {
        let mut usage = TokenUsage::default();
        if let Some(t) = self.total_tokens {
            usage.reported_total = t;
        }
        if let Some(p) = self.prompt_tokens {
            usage.input = p;
            usage.present |= KIND_INPUT;
        }
        if let Some(c) = self.completion_tokens {
            usage.output = c;
            usage.present |= KIND_OUTPUT;
        }
        if let Some(cached) = self.prompt_tokens_details {
            usage.input -= cached;
            if usage.input < 0 {
                usage.input = 0;
            }
            usage.cache_read = cached;
            usage.present |= KIND_CACHE_READ;
        }
        if let Some(r) = self.completion_tokens_details {
            usage.reasoning = r;
            usage.present |= KIND_REASONING;
        }
        usage
    }
}

// --- non-streaming response ---

#[derive(Default)]
struct InferenceResponse {
    model: String,
    choices: Vec<InferenceChoice>,
    usage: InferenceUsage,
}

#[derive(Default)]
struct InferenceChoice {
    content: String,
    tool_calls: Vec<InferenceToolCall>,
    finish_reason: String,
}

/// Decodes one Go `struct{ ID, Type string; Function struct{ Name, Arguments string } }`
/// tool call. With `index`, the streamed shape: `Index int` and no `Type`.
fn decode_tool_call(raw: &RawValue, tc: &mut InferenceToolCall, index: Option<&mut i64>) -> R {
    let mut typ = String::new();
    let mut index = index;
    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
        match k.as_str() {
            k if is(k, "id") => gojson::string_into(&mut tc.id, v)?,
            k if index.is_none() && is(k, "type") => gojson::string_into(&mut typ, v)?,
            k if is(k, "index") => {
                if let Some(i) = index.as_deref_mut() {
                    gojson::int_into(i, v)?
                }
            }
            k if is(k, "function") => {
                for (k, v) in gojson::object(v)?.unwrap_or_default() {
                    match k.as_str() {
                        k if is(k, "name") => gojson::string_into(&mut tc.name, v)?,
                        k if is(k, "arguments") => gojson::string_into(&mut tc.arguments, v)?,
                        _ => {}
                    }
                }
            }
            _ => {}
        }
    }
    Ok(())
}

fn decode_inference_response(raw: &RawValue, r: &mut InferenceResponse) -> R {
    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
        match k.as_str() {
            k if is(k, "model") => gojson::string_into(&mut r.model, v)?,
            k if is(k, "choices") => {
                r.choices = gojson::slice(v, |e| {
                    let mut c = InferenceChoice::default();
                    let mut role = String::new();
                    for (k, v) in gojson::object(e)?.unwrap_or_default() {
                        match k.as_str() {
                            k if is(k, "message") => {
                                for (k, v) in gojson::object(v)?.unwrap_or_default() {
                                    match k.as_str() {
                                        k if is(k, "role") => gojson::string_into(&mut role, v)?,
                                        k if is(k, "content") => {
                                            gojson::string_into(&mut c.content, v)?
                                        }
                                        k if is(k, "tool_calls") => {
                                            c.tool_calls = gojson::slice(v, |e| {
                                                let mut tc = InferenceToolCall::default();
                                                decode_tool_call(e, &mut tc, None)?;
                                                Ok(tc)
                                            })?
                                            .unwrap_or_default()
                                        }
                                        _ => {}
                                    }
                                }
                            }
                            k if is(k, "finish_reason") => {
                                gojson::string_into(&mut c.finish_reason, v)?
                            }
                            _ => {}
                        }
                    }
                    Ok(c)
                })?
                .unwrap_or_default()
            }
            k if is(k, "usage") => decode_inference_usage(v, &mut r.usage)?,
            _ => {}
        }
    }
    Ok(())
}

/// Parses a non-streaming chat/completions response.
pub(super) fn parse_inference_json(body: &[u8], ext: &mut InferenceExtension) {
    let lossy = go_lossy_utf8(body);
    let Ok(raw) = gojson::parse(&lossy.text) else {
        return;
    };
    let mut resp = InferenceResponse::default();
    if decode_inference_response(raw, &mut resp).is_err() {
        return;
    }
    note_body_model(ext, &resp.model);
    if let Some(c) = resp.choices.into_iter().next() {
        ext.completion = c.content;
        ext.finish_reason = c.finish_reason;
        ext.tool_calls.extend(c.tool_calls);
    }
    // No usage block: leave present_kinds at 0, as the SSE path does.
    if resp.usage.has_any() {
        resp.usage.to_neutral().fill(ext);
    }
}

// --- streaming ---

#[derive(Default)]
struct InferenceStreamChunk {
    model: String,
    choices: Vec<InferenceStreamChoice>,
    usage: InferenceUsage,
}

#[derive(Default)]
struct InferenceStreamChoice {
    content: String,
    tool_calls: Vec<InferenceStreamToolCall>,
    finish_reason: String,
}

/// One fragment of a streamed tool call. A fragment without an index reads as index 0.
#[derive(Default)]
struct InferenceStreamToolCall {
    index: i64,
    call: InferenceToolCall,
}

fn decode_inference_stream_chunk(raw: &RawValue, ch: &mut InferenceStreamChunk) -> R {
    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
        match k.as_str() {
            k if is(k, "model") => gojson::string_into(&mut ch.model, v)?,
            k if is(k, "choices") => {
                ch.choices = gojson::slice(v, |e| {
                    let mut c = InferenceStreamChoice::default();
                    for (k, v) in gojson::object(e)?.unwrap_or_default() {
                        match k.as_str() {
                            k if is(k, "delta") => {
                                for (k, v) in gojson::object(v)?.unwrap_or_default() {
                                    match k.as_str() {
                                        k if is(k, "content") => {
                                            gojson::string_into(&mut c.content, v)?
                                        }
                                        k if is(k, "tool_calls") => {
                                            c.tool_calls = gojson::slice(v, |e| {
                                                let mut tc = InferenceStreamToolCall::default();
                                                decode_tool_call(
                                                    e,
                                                    &mut tc.call,
                                                    Some(&mut tc.index),
                                                )?;
                                                Ok(tc)
                                            })?
                                            .unwrap_or_default()
                                        }
                                        _ => {}
                                    }
                                }
                            }
                            k if is(k, "finish_reason") => {
                                gojson::string_into(&mut c.finish_reason, v)?
                            }
                            _ => {}
                        }
                    }
                    Ok(c)
                })?
                .unwrap_or_default()
            }
            k if is(k, "usage") => decode_inference_usage(v, &mut ch.usage)?,
            _ => {}
        }
    }
    Ok(())
}

impl InferenceStreamState {
    /// Folds one delta.tool_calls fragment into the call at its index, opening
    /// one on first sight. Each field is written only when present.
    fn fold_openai_tool_call_fragment(&mut self, tc: &InferenceStreamToolCall) {
        let i = *self.openai_by_index.entry(tc.index).or_insert_with(|| {
            self.openai_tool_calls.push(ToolCallState::default());
            self.openai_tool_calls.len() - 1
        });
        let acc = &mut self.openai_tool_calls[i];
        if !tc.call.id.is_empty() {
            acc.id.clone_from(&tc.call.id);
        }
        if !tc.call.name.is_empty() {
            acc.name.clone_from(&tc.call.name);
        }
        acc.args.push_str(&tc.call.arguments);
    }
}

/// Folds one streaming chunk into the stream state. The "[DONE]" sentinel and
/// malformed chunks are skipped. Usage is cumulative: the latest usage-bearing
/// chunk replaces the state.
pub(super) fn fold_openai_frame(
    frame: &[u8],
    state: &mut InferenceStreamState,
    ext: &mut InferenceExtension,
) {
    if go_trim_space(frame) == b"[DONE]" {
        return;
    }
    let lossy = go_lossy_utf8(frame);
    let Ok(raw) = gojson::parse(&lossy.text) else {
        return;
    };
    let mut chunk = InferenceStreamChunk::default();
    if decode_inference_stream_chunk(raw, &mut chunk).is_err() {
        return;
    }
    note_body_model(ext, &chunk.model);
    for c in &chunk.choices {
        state.completion.push_str(&c.content);
        for tc in &c.tool_calls {
            state.fold_openai_tool_call_fragment(tc);
        }
        if !c.finish_reason.is_empty() {
            ext.finish_reason.clone_from(&c.finish_reason);
        }
    }
    if chunk.usage.has_any() {
        state.usage = chunk.usage.to_neutral();
        state.has_usage = true;
    }
}

/// Folds a whole buffered chat/completions SSE body. The "data: [DONE]"
/// marker is skipped.
pub(super) fn parse_inference_sse(body: &[u8], ext: &mut InferenceExtension) {
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
        // fold_openai_frame skips "[DONE]" as this loop does in Go.
        fold_openai_frame(data, &mut state, ext);
    }
    state.finalize(ext);
}

#[cfg(test)]
mod tests {
    use super::super::{parse_request, parse_response};
    use http::HeaderMap;
    use serde_json::{Value, json};

    const PATH: &str = "/v1/chat/completions";

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
            req(r#"{"model":"m","prompt":"hi","reasoning_effort":"low"}"#),
            json!({"model":"m","effort":"low","isAction":true})
        );
        assert_eq!(req(r#"{"model":"m","prompt":null}"#), json!(null));
        assert_eq!(req(r#"{"model":"m","messages":null}"#), json!(null));
        assert_eq!(
            req(r#"{"model":"m","messages":[],"tools":[{"type":5,"function":{"name":"a"}}]}"#),
            json!(null)
        );
        assert_eq!(
            req(
                r#"{"Model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":7,"function":{"name":"f"},"function":{"arguments":{"a":1}}}]},{"role":"tool","tool_call_id":"7","content":[{"type":"text","text":"x"},{"type":"image_url"}]}]}"#
            ),
            json!({"model":"m","messages":[{"role":"assistant","toolCallNames":"f","toolUseIds":"7"},{"role":"tool","content":"x","contentBytes":49,"toolUseId":"7"}],"toolResults":[{"toolUseId":"7","content":"x"}],"historyToolCalls":[{"id":"7","name":"f","arguments":"{\"a\":1}"}],"isAction":true})
        );

        assert_eq!(
            resp(r#"{"model":"x","choices":[{"message":{"content":[1]}}]}"#),
            json!({})
        );
        assert_eq!(
            resp(
                r#"{"model":"x","choices":[{"message":{"content":"a"},"finish_reason":"stop"},{"message":{"content":"b"}}],"usage":{"total_tokens":9,"prompt_tokens_details":null}}"#
            ),
            json!({"servedModel":"x","completion":"a","finishReason":"stop","totalTokens":9})
        );
        assert_eq!(
            resp(r#"{"usage":{"prompt_tokens":3,"prompt_tokens_details":{"cached_tokens":5}}}"#),
            json!({"promptTokens":5,"totalTokens":5,"cacheReadTokens":5,"presentKinds":3})
        );
        assert_eq!(
            resp(
                "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"c\",\"function\":{\"name\":\"n\",\"arguments\":\"{\"}}]}}]}\n\ndata: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"}\"}}]}}]}\n\ndata: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1.5}]}}]}\n\ndata: [DONE]\n\n"
            ),
            json!({"stream":true,"streamedResponse":true,"toolCalls":[{"id":"c","name":"n","arguments":"{}"}]})
        );
    }
}

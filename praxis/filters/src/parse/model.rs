//! The inference, MCP and A2A extensions, as the Go structs in core/model/extensions.go give them in JSON.

use serde::Serialize;
use serde_json::value::RawValue;
use serde_json::{Map, Value};

// Go's omitempty skips a zero number.
fn is_zero<T: Default + PartialEq>(v: &T) -> bool {
    *v == T::default()
}

// Go's omitempty skips false.
fn is_false(v: &bool) -> bool {
    !*v
}

/// One inference request and its response. Mirrors model.InferenceExtension.
#[derive(Clone, Debug, Default, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct InferenceExtension {
    #[serde(skip_serializing_if = "String::is_empty")]
    pub model: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub messages: Vec<InferenceMessage>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub temperature: Option<f64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub max_tokens: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub top_p: Option<f64>,
    #[serde(skip_serializing_if = "is_false")]
    pub stream: bool,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub tools: Vec<InferenceTool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub tool_choice: Option<Value>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub requested_model: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub served_model: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub tool_results: Vec<InferenceToolResult>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub history_tool_calls: Vec<InferenceToolCall>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub thinking_type: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub thinking_display: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub effort: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub anthropic_beta: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub history_thinking: Option<InferenceThinking>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub response_thinking: Option<InferenceThinking>,
    #[serde(skip_serializing_if = "is_zero")]
    pub message_count: i64,
    #[serde(skip_serializing_if = "is_zero")]
    pub tool_count: i64,
    /// "main" or "subagent".
    #[serde(skip_serializing_if = "String::is_empty")]
    pub agent_role: String,
    #[serde(skip_serializing_if = "is_false")]
    pub streamed_response: bool,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub completion: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub finish_reason: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub tool_calls: Vec<InferenceToolCall>,
    #[serde(skip_serializing_if = "is_zero")]
    pub prompt_tokens: i64,
    #[serde(skip_serializing_if = "is_zero")]
    pub completion_tokens: i64,
    #[serde(skip_serializing_if = "is_zero")]
    pub total_tokens: i64,
    #[serde(skip_serializing_if = "is_zero")]
    pub input_tokens: i64,
    #[serde(skip_serializing_if = "is_zero")]
    pub cache_read_tokens: i64,
    #[serde(skip_serializing_if = "is_zero")]
    pub cache_write_tokens: i64,
    #[serde(skip_serializing_if = "is_zero")]
    pub output_tokens: i64,
    #[serde(skip_serializing_if = "is_zero")]
    pub reasoning_tokens: i64,
    #[serde(skip_serializing_if = "is_zero")]
    pub present_kinds: u8,
    #[serde(skip_serializing_if = "is_false")]
    pub is_action: bool,
}

/// One message of the request. Mirrors model.InferenceMessage.
#[derive(Clone, Debug, Default, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct InferenceMessage {
    pub role: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub content: String,
    #[serde(skip_serializing_if = "is_zero")]
    pub content_bytes: i64,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub tool_use_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub tool_call_names: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub tool_use_ids: String,
}

/// One tool the request offers. Mirrors model.InferenceTool.
#[derive(Clone, Debug, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct InferenceTool {
    pub name: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub description: String,
    /// The JSON schema as the client sent it, byte for byte.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub parameters: Option<Box<RawValue>>,
}

// RawValue has no PartialEq, so compare its text.
impl PartialEq for InferenceTool {
    fn eq(&self, o: &Self) -> bool {
        self.name == o.name
            && self.description == o.description
            && self.parameters.as_ref().map(|p| p.get()) == o.parameters.as_ref().map(|p| p.get())
    }
}

/// One tool call. Mirrors model.InferenceToolCall.
#[derive(Clone, Debug, Default, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct InferenceToolCall {
    #[serde(skip_serializing_if = "String::is_empty")]
    pub id: String,
    pub name: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub arguments: String,
}

/// Counts of thinking blocks. Mirrors model.InferenceThinking.
#[derive(Clone, Debug, Default, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct InferenceThinking {
    pub blocks: i64,
    #[serde(skip_serializing_if = "is_zero")]
    pub redacted: i64,
    #[serde(skip_serializing_if = "is_zero")]
    pub text_bytes: i64,
    #[serde(skip_serializing_if = "is_zero")]
    pub signature_bytes: i64,
    #[serde(skip_serializing_if = "is_zero")]
    pub redacted_bytes: i64,
}

impl InferenceThinking {
    /// Counts one content block of type `typ`, with the byte sizes of its
    /// fields. Other block types count nothing.
    pub fn add(&mut self, typ: &str, text: i64, signature: i64, data: i64) {
        match typ {
            "thinking" => {
                self.blocks = self.blocks.wrapping_add(1);
                self.text_bytes = self.text_bytes.wrapping_add(text);
                self.signature_bytes = self.signature_bytes.wrapping_add(signature);
            }
            "redacted_thinking" => {
                self.blocks = self.blocks.wrapping_add(1);
                self.redacted = self.redacted.wrapping_add(1);
                self.redacted_bytes = self.redacted_bytes.wrapping_add(data);
            }
            _ => {}
        }
    }

    /// Adds the counts of `o`.
    pub fn merge(&mut self, o: &InferenceThinking) {
        self.blocks = self.blocks.wrapping_add(o.blocks);
        self.redacted = self.redacted.wrapping_add(o.redacted);
        self.text_bytes = self.text_bytes.wrapping_add(o.text_bytes);
        self.signature_bytes = self.signature_bytes.wrapping_add(o.signature_bytes);
        self.redacted_bytes = self.redacted_bytes.wrapping_add(o.redacted_bytes);
    }

    /// The counts, or `None` when no block was counted.
    pub fn or_nil(self) -> Option<InferenceThinking> {
        (self.blocks != 0).then_some(self)
    }
}

/// One tool result in the request. Mirrors model.InferenceToolResult.
#[derive(Clone, Debug, Default, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct InferenceToolResult {
    #[serde(skip_serializing_if = "String::is_empty")]
    pub tool_use_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub content: String,
    #[serde(skip_serializing_if = "is_false")]
    pub is_error: bool,
}

// Go's omitempty skips a nil map and an empty map.
fn map_is_empty(m: &Option<Map<String, Value>>) -> bool {
    m.as_ref().is_none_or(Map::is_empty)
}

/// One MCP JSON-RPC request and its response. Mirrors model.MCPExtension.
/// The parser keeps an empty `result` as `Some`: Go checks `Result != nil`.
#[derive(Clone, Debug, Default, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct McpExtension {
    #[serde(skip_serializing_if = "String::is_empty")]
    pub method: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub rpc_id: Option<Value>,
    #[serde(skip_serializing_if = "map_is_empty")]
    pub params: Option<Map<String, Value>>,
    #[serde(skip_serializing_if = "map_is_empty")]
    pub result: Option<Map<String, Value>>,
    #[serde(rename = "error", skip_serializing_if = "Option::is_none")]
    pub err: Option<McpError>,
    #[serde(skip_serializing_if = "is_false")]
    pub is_action: bool,
}

/// A JSON-RPC 2.0 error object. Mirrors model.MCPError.
#[derive(Clone, Debug, Default, PartialEq, Serialize)]
pub struct McpError {
    pub code: i64,
    pub message: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub data: Option<Value>,
}

/// One A2A request and a summary of its response. Mirrors model.A2AExtension.
#[derive(Clone, Debug, Default, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct A2aExtension {
    #[serde(skip_serializing_if = "String::is_empty")]
    pub method: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub rpc_id: Option<Value>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub session_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub message_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub task_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub role: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub parts: Vec<A2aPart>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub final_status: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub artifact: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub error_message: String,
    #[serde(skip_serializing_if = "is_false")]
    pub is_action: bool,
}

/// One message part of an A2A request. Mirrors model.A2APart.
#[derive(Clone, Debug, Default, PartialEq, Serialize)]
pub struct A2aPart {
    pub kind: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub content: String,
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::{BTreeMap, BTreeSet};

    const NOT_SET_IN_PRAXIS_MODE: &[&str] = &[
        "inference.requestedModel", // only Context.SetRequestModel sets it; no plugin runs in praxis mode
        "inference.messageCount",   // only sessionapi.summarizeEvent sets it, in Go
        "inference.toolCount",      // the same
        "inference.toolCallCount",  // the same
    ];

    fn thinking() -> InferenceThinking {
        InferenceThinking {
            blocks: 1,
            redacted: 1,
            text_bytes: 1,
            signature_bytes: 1,
            redacted_bytes: 1,
        }
    }

    fn call() -> InferenceToolCall {
        InferenceToolCall {
            id: "id".into(),
            name: "name".into(),
            arguments: "{}".into(),
        }
    }

    fn full_sample() -> InferenceExtension {
        InferenceExtension {
            model: "m".into(),
            messages: vec![InferenceMessage {
                role: "user".into(),
                content: "c".into(),
                content_bytes: 1,
                tool_use_id: "t".into(),
                tool_call_names: "n".into(),
                tool_use_ids: "t".into(),
            }],
            temperature: Some(0.5),
            max_tokens: Some(1),
            top_p: Some(0.5),
            stream: true,
            tools: vec![InferenceTool {
                name: "n".into(),
                description: "d".into(),
                parameters: Some(RawValue::from_string(r#"{"type":"object"}"#.into()).unwrap()),
            }],
            tool_choice: Some(Value::String("auto".into())),
            requested_model: "m".into(),
            served_model: "m".into(),
            tool_results: vec![InferenceToolResult {
                tool_use_id: "t".into(),
                content: "c".into(),
                is_error: true,
            }],
            history_tool_calls: vec![call()],
            thinking_type: "enabled".into(),
            thinking_display: "d".into(),
            effort: "high".into(),
            anthropic_beta: "b".into(),
            history_thinking: Some(thinking()),
            response_thinking: Some(thinking()),
            message_count: 1,
            tool_count: 1,
            agent_role: "main".into(),
            streamed_response: true,
            completion: "c".into(),
            finish_reason: "stop".into(),
            tool_calls: vec![call()],
            prompt_tokens: 1,
            completion_tokens: 1,
            total_tokens: 1,
            input_tokens: 1,
            cache_read_tokens: 1,
            cache_write_tokens: 1,
            output_tokens: 1,
            reasoning_tokens: 1,
            present_kinds: 1,
            is_action: true,
        }
    }

    /// Collects the key paths of v, as parsefixture.checkFieldPaths walks them.
    fn walk(path: &str, v: &Value, kinds: &BTreeMap<String, String>, out: &mut BTreeSet<String>) {
        match v {
            Value::Object(m) => {
                let is_map = kinds.get(path).map(String::as_str) == Some("map");
                for (k, child) in m {
                    // The children of a Go map share one path, as in parsefixture.checkFieldPaths.
                    let p = if is_map {
                        format!("{path}{{}}")
                    } else {
                        format!("{path}.{k}")
                    };
                    out.insert(p.clone());
                    if kinds.get(&p).map(String::as_str) != Some("any") {
                        walk(&p, child, kinds, out);
                    }
                }
            }
            Value::Array(a) => {
                let p = format!("{path}[]");
                out.insert(p.clone());
                for child in a {
                    walk(&p, child, kinds, out);
                }
            }
            _ => {}
        }
    }

    #[test]
    fn zero_values_follow_go_omitempty() {
        assert_eq!(
            serde_json::to_string(&InferenceExtension::default()).unwrap(),
            "{}"
        );
        assert_eq!(
            serde_json::to_string(&InferenceMessage::default()).unwrap(),
            r#"{"role":""}"#
        );
        assert_eq!(
            serde_json::to_string(&InferenceTool::default()).unwrap(),
            r#"{"name":""}"#
        );
        assert_eq!(
            serde_json::to_string(&InferenceToolCall::default()).unwrap(),
            r#"{"name":""}"#
        );
        assert_eq!(
            serde_json::to_string(&InferenceThinking::default()).unwrap(),
            r#"{"blocks":0}"#
        );
        assert_eq!(
            serde_json::to_string(&InferenceToolResult::default()).unwrap(),
            "{}"
        );
    }

    #[test]
    fn mcp_a2a_zero_values_follow_go_omitempty() {
        assert_eq!(json(&McpExtension::default()), "{}");
        // Go's omitempty omits an empty map too.
        let empty = McpExtension {
            params: Some(Map::new()),
            result: Some(Map::new()),
            ..Default::default()
        };
        assert_eq!(json(&empty), "{}");
        assert_eq!(json(&McpError::default()), r#"{"code":0,"message":""}"#);
        assert_eq!(json(&A2aExtension::default()), "{}");
        assert_eq!(json(&A2aPart::default()), r#"{"kind":""}"#);
    }

    fn json<T: Serialize>(v: &T) -> String {
        serde_json::to_string(v).unwrap()
    }

    fn mcp_sample() -> McpExtension {
        let map: Map<String, Value> = [("k".to_string(), Value::from(1.0))].into_iter().collect();
        McpExtension {
            method: "tools/call".into(),
            rpc_id: Some(Value::from(1.0)),
            params: Some(map.clone()),
            result: Some(map),
            err: Some(McpError {
                code: 1,
                message: "m".into(),
                data: Some(Value::from("d")),
            }),
            is_action: true,
        }
    }

    fn a2a_sample() -> A2aExtension {
        A2aExtension {
            method: "message/send".into(),
            rpc_id: Some(Value::from(1.0)),
            session_id: "s".into(),
            message_id: "m".into(),
            task_id: "t".into(),
            role: "user".into(),
            parts: vec![A2aPart {
                kind: "text".into(),
                content: "c".into(),
            }],
            final_status: "completed".into(),
            artifact: "a".into(),
            error_message: "e".into(),
            is_action: true,
        }
    }

    #[test]
    fn every_go_inference_field_is_set_or_listed() {
        check_fields(
            "inference",
            serde_json::to_value(full_sample()).unwrap(),
            NOT_SET_IN_PRAXIS_MODE,
        );
    }

    #[test]
    fn every_go_mcp_field_is_set_or_listed() {
        check_fields("mcp", serde_json::to_value(mcp_sample()).unwrap(), &[]);
    }

    #[test]
    fn every_go_a2a_field_is_set_or_listed() {
        check_fields("a2a", serde_json::to_value(a2a_sample()).unwrap(), &[]);
    }

    /// Compares the key paths of `sample` with the `<prefix>.` lines of the Go field list.
    fn check_fields(prefix: &str, sample: Value, not_set: &[&str]) {
        let file = concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../../core/pipeline/testdata/parsed-request.fields"
        );
        let text = std::fs::read_to_string(file).unwrap();
        let dot = format!("{prefix}.");
        let kinds: BTreeMap<String, String> = text
            .lines()
            .filter(|l| l.starts_with(&dot))
            .filter_map(|l| l.split_once(' '))
            .map(|(p, k)| (p.to_string(), k.to_string()))
            .collect();
        assert!(!kinds.is_empty(), "no {prefix} fields in {file}");

        let mut rust = BTreeSet::new();
        walk(prefix, &sample, &kinds, &mut rust);

        let missing: Vec<_> = kinds
            .keys()
            .filter(|p| !rust.contains(*p) && !not_set.contains(&p.as_str()))
            .collect();
        assert!(
            missing.is_empty(),
            "Go fields the Rust model does not give: {missing:?}"
        );
        let extra: Vec<_> = rust.iter().filter(|p| !kinds.contains_key(*p)).collect();
        assert!(
            extra.is_empty(),
            "Rust fields that Go does not have: {extra:?}"
        );
    }
}

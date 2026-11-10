//! Port of core/parse/a2a/a2a.go: parses A2A JSON-RPC 2.0 requests and responses.

mod marshal;
mod observer;

pub use observer::A2aObserver;

use http::HeaderMap;
use serde_json::Value;
use serde_json::value::RawValue;

pub(crate) use marshal::go_marshal;

use crate::parse::decode::decode_request_body;
use crate::parse::inference::gojson::{self, R};
use crate::parse::inference::text::go_lossy_utf8;
use crate::parse::jsonrpc::{decode_request, for_each_data_payload};
use crate::parse::model::{A2aExtension, A2aPart};

/// Port of ParseRequest. `None` when the parser does not claim the request.
/// The Go `path` argument is used only in log lines, so it is not ported.
pub fn parse_request(headers: &HeaderMap, body: &[u8]) -> Option<A2aExtension> {
    let body = decode_request_body(headers, body);
    if body.is_empty() {
        return None;
    }
    let rpc = decode_request(&body)?;
    if !is_a2a_method(&rpc.method) {
        return None;
    }
    let mut ext = A2aExtension {
        is_action: is_a2a_action(&rpc.method),
        method: rpc.method.clone(),
        rpc_id: rpc.id.clone(),
        ..Default::default()
    };
    ext.session_id = rpc.string_param("contextId");
    if ext.session_id.is_empty() {
        ext.session_id = rpc.string_param("sessionId");
    }
    if let Some(msg) = rpc.map_param("message") {
        let str_of = |k: &str| msg.get(k).and_then(Value::as_str).map(str::to_owned);
        if let Some(s) = str_of("taskId") {
            ext.task_id = s;
        }
        if ext.session_id.is_empty()
            && let Some(s) = str_of("contextId")
        {
            ext.session_id = s;
        }
        if let Some(s) = str_of("role") {
            ext.role = s;
        }
        if let Some(s) = str_of("messageId") {
            ext.message_id = s;
        }
        if let Some(Value::Array(raw)) = msg.get("parts") {
            ext.parts = parse_a2a_parts(raw);
        }
    }
    if ext.task_id.is_empty() && task_id_param_is_id(&rpc.method) {
        ext.task_id = rpc.string_param("id");
    }
    if ext.task_id.is_empty() {
        ext.task_id = rpc.string_param("taskId");
    }
    Some(ext)
}

/// Port of ParseResponse: folds one whole body or one SSE frame into `ext`.
pub fn parse_response(body: &[u8], ext: &mut A2aExtension) {
    extract_response_summary(body, ext);
    if ext.session_id.is_empty() {
        let sid = extract_session_id(body);
        if !sid.is_empty() {
            ext.session_id = sid;
        }
    }
}

/// maxArtifactBytes: the cap on the artifact text of one response.
const MAX_ARTIFACT_BYTES: usize = 64 * 1024;

/// Port of appendCapped. Go cuts `s` at a byte index, so a Go string can hold a
/// part of a UTF-8 sequence, and Go's json.Marshal writes one U+FFFD for each of
/// its bytes. A Rust `String` cannot hold it, so the U+FFFDs go in here.
fn append_capped(dst: &mut String, s: &str) {
    const TRUNC_MARKER: &str = "…(truncated)";
    if dst.len() >= MAX_ARTIFACT_BYTES {
        return;
    }
    let remaining = MAX_ARTIFACT_BYTES - dst.len();
    if s.len() <= remaining {
        dst.push_str(s);
        return;
    }
    let mut cut = remaining;
    while !s.is_char_boundary(cut) {
        cut -= 1;
    }
    dst.push_str(&s[..cut]);
    dst.extend(std::iter::repeat_n('\u{FFFD}', remaining - cut));
    dst.push_str(TRUNC_MARKER);
}

/// Decodes `body` as Go's json.Unmarshal does into the struct that `f` fills.
/// `None` on a syntax error or a type error.
fn decode<T: Default>(body: &[u8], f: impl FnOnce(&RawValue, &mut T) -> R) -> Option<T> {
    let text = go_lossy_utf8(body).text;
    let raw = gojson::parse(&text).ok()?;
    let mut v = T::default();
    f(raw, &mut v).ok()?;
    Some(v)
}

/// Calls `f` on each entry of a Go struct's JSON object. null changes nothing.
fn fields<'a>(raw: &'a RawValue, mut f: impl FnMut(&str, &'a RawValue) -> R) -> R {
    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
        f(&k, v)?;
    }
    Ok(())
}

/// The Go `struct { Kind string; Text string }` of a part.
#[derive(Default)]
struct TextPart {
    kind: String,
    text: String,
}

/// Decodes a `[]struct{Kind, Text}` field. null sets it to nil.
fn parts_into(dst: &mut Vec<TextPart>, raw: &RawValue) -> R {
    *dst = gojson::slice(raw, |r| {
        let mut p = TextPart::default();
        fields(r, |k, v| {
            if gojson::is(k, "kind") {
                gojson::string_into(&mut p.kind, v)
            } else if gojson::is(k, "text") {
                gojson::string_into(&mut p.text, v)
            } else {
                Ok(())
            }
        })?;
        Ok(p)
    })?
    .unwrap_or_default();
    Ok(())
}

/// Decodes `{"parts": [...]}` into `dst`.
fn parts_field_into(dst: &mut Vec<TextPart>, raw: &RawValue) -> R {
    fields(raw, |k, v| {
        if gojson::is(k, "parts") {
            parts_into(dst, v)
        } else {
            Ok(())
        }
    })
}

/// Port of extractResponseSummary.
fn extract_response_summary(body: &[u8], ext: &mut A2aExtension) {
    if gojson::parse(&go_lossy_utf8(body).text).is_ok() {
        fold_response(body, ext);
        return;
    }
    // Port of extractStreamResponse: fold each data: line as one event.
    for_each_data_payload(body, |data| {
        fold_response(data, ext);
        false
    });
}

/// Port of foldResponse.
fn fold_response(data: &[u8], ext: &mut A2aExtension) {
    if !extract_send_response(data, ext) {
        extract_stream_event(data, ext);
    }
}

/// The `result` of the Go struct in extractSendResponse.
#[derive(Default)]
struct SendResult {
    state: String,
    status_parts: Vec<TextPart>,
    artifacts: Vec<Vec<TextPart>>,
    kind: Option<Value>,
    id: Option<Value>,
    task_id: String,
}

/// Port of extractSendResponse.
fn extract_send_response(body: &[u8], ext: &mut A2aExtension) -> bool {
    let Some(r) = decode(body, |raw, r: &mut SendResult| {
        fields(raw, |k, v| {
            if !gojson::is(k, "result") {
                return Ok(());
            }
            fields(v, |k, v| {
                if gojson::is(k, "status") {
                    fields(v, |k, v| {
                        if gojson::is(k, "state") {
                            gojson::string_into(&mut r.state, v)
                        } else if gojson::is(k, "message") {
                            parts_field_into(&mut r.status_parts, v)
                        } else {
                            Ok(())
                        }
                    })
                } else if gojson::is(k, "artifacts") {
                    r.artifacts = gojson::slice(v, |a| {
                        let mut parts = Vec::new();
                        parts_field_into(&mut parts, a)?;
                        Ok(parts)
                    })?
                    .unwrap_or_default();
                    Ok(())
                } else if gojson::is(k, "kind") {
                    r.kind = gojson::any(v)?;
                    Ok(())
                } else if gojson::is(k, "id") {
                    r.id = gojson::any(v)?;
                    Ok(())
                } else if gojson::is(k, "taskId") {
                    gojson::string_into(&mut r.task_id, v)
                } else {
                    Ok(())
                }
            })
        })
    }) else {
        return false;
    };
    if r.state.is_empty() {
        return false;
    }
    let id = result_task_id(r.kind.as_ref(), r.id.as_ref(), &r.task_id);
    if ext.task_id.is_empty() && !id.is_empty() {
        ext.task_id = id;
    }
    for part in r.artifacts.iter().flatten() {
        if part.kind == "text" && !part.text.is_empty() {
            ext.artifact.clone_from(&part.text);
        }
    }
    if r.state == "failed"
        && let Some(p) = r
            .status_parts
            .iter()
            .find(|p| p.kind == "text" && !p.text.is_empty())
    {
        ext.error_message.clone_from(&p.text);
    }
    ext.final_status = r.state;
    true
}

/// The `result` of the Go struct in extractStreamEvent.
#[derive(Default)]
struct StreamResult {
    kind: String,
    id: Option<Value>,
    task_id: String,
    artifact_parts: Vec<TextPart>,
}

/// Port of extractStreamEvent.
fn extract_stream_event(data: &[u8], ext: &mut A2aExtension) {
    let Some(r) = decode(data, |raw, r: &mut StreamResult| {
        fields(raw, |k, v| {
            if !gojson::is(k, "result") {
                return Ok(());
            }
            fields(v, |k, v| {
                if gojson::is(k, "kind") {
                    gojson::string_into(&mut r.kind, v)
                } else if gojson::is(k, "id") {
                    r.id = gojson::any(v)?;
                    Ok(())
                } else if gojson::is(k, "taskId") {
                    gojson::string_into(&mut r.task_id, v)
                } else if gojson::is(k, "artifact") {
                    parts_field_into(&mut r.artifact_parts, v)
                } else {
                    Ok(())
                }
            })
        })
    }) else {
        return;
    };
    let kind = Value::String(r.kind.clone());
    let id = result_task_id(Some(&kind), r.id.as_ref(), &r.task_id);
    if ext.task_id.is_empty() && !id.is_empty() {
        ext.task_id = id;
    }
    if matches!(r.kind.as_str(), "artifact-update" | "artifact") {
        for part in &r.artifact_parts {
            if part.kind == "text" && !part.text.is_empty() {
                append_capped(&mut ext.artifact, &part.text);
            }
        }
    }
}

/// Port of taskIDParamIsID: the params of the method are TaskIdParams or extend them.
fn task_id_param_is_id(method: &str) -> bool {
    matches!(
        method,
        "tasks/get"
            | "tasks/cancel"
            | "tasks/resubscribe"
            | "tasks/pushNotificationConfig/get"
            | "tasks/pushNotificationConfig/list"
            | "tasks/pushNotificationConfig/delete"
    )
}

/// Port of resultTaskID. A Task (kind "task") has its id in `id`; other results in `taskId`.
fn result_task_id(kind: Option<&Value>, id: Option<&Value>, task_id: &str) -> String {
    if kind.and_then(Value::as_str) == Some("task") {
        return id.and_then(Value::as_str).unwrap_or_default().to_owned();
    }
    task_id.to_owned()
}

/// Port of extractSessionID: contextId (first) or sessionId, from the body or its SSE events.
fn extract_session_id(body: &[u8]) -> String {
    let sid = session_id_from_json(body);
    if !sid.is_empty() {
        return sid;
    }
    let mut found = String::new();
    for_each_data_payload(body, |data| {
        found = session_id_from_json(data);
        !found.is_empty()
    });
    found
}

/// Port of sessionIDFromJSON.
fn session_id_from_json(data: &[u8]) -> String {
    let ids = decode(data, |raw, ids: &mut (String, String)| {
        fields(raw, |k, v| {
            if !gojson::is(k, "result") {
                return Ok(());
            }
            fields(v, |k, v| {
                if gojson::is(k, "contextId") {
                    gojson::string_into(&mut ids.0, v)
                } else if gojson::is(k, "sessionId") {
                    gojson::string_into(&mut ids.1, v)
                } else {
                    Ok(())
                }
            })
        })
    });
    match ids {
        Some((ctx, _)) if !ctx.is_empty() => ctx,
        Some((_, sid)) => sid,
        None => String::new(),
    }
}

/// Port of parseA2AParts.
fn parse_a2a_parts(raw_parts: &[Value]) -> Vec<A2aPart> {
    let mut parts = Vec::with_capacity(raw_parts.len());
    for raw in raw_parts {
        let Some(part) = raw.as_object() else {
            continue;
        };
        let kind = match part.get("kind").and_then(Value::as_str) {
            Some(k) if !k.is_empty() => k,
            _ => continue,
        };
        let str_of = |k: &str| part.get(k).and_then(Value::as_str).unwrap_or_default();
        let content = match kind {
            "text" => str_of("text").to_owned(),
            "file" => match str_of("data") {
                "" => str_of("uri").to_owned(),
                d => d.to_owned(),
            },
            "data" => match part.get("data") {
                Some(v) if !v.is_null() => go_marshal(v),
                _ => String::new(),
            },
            _ => String::new(),
        };
        parts.push(A2aPart {
            kind: kind.to_owned(),
            content,
        });
    }
    parts
}

/// Port of isA2AMethod: the method is in the message/, tasks/ or agent/ namespace.
fn is_a2a_method(method: &str) -> bool {
    ["message/", "tasks/", "agent/"]
        .iter()
        .any(|p| method.starts_with(p))
}

/// Port of isA2AAction: the method carries a user message.
fn is_a2a_action(method: &str) -> bool {
    matches!(method, "message/send" | "message/stream")
}

#[cfg(test)]
mod tests {
    use super::*;

    fn request(body: &str) -> Option<A2aExtension> {
        parse_request(&HeaderMap::new(), body.as_bytes())
    }

    #[test]
    fn append_capped_cuts_once() {
        let mut a = "a".repeat(MAX_ARTIFACT_BYTES - 2);
        append_capped(&mut a, "bcd");
        assert_eq!(&a[MAX_ARTIFACT_BYTES - 3..], "abc…(truncated)");
        let len = a.len();
        append_capped(&mut a, "e");
        assert_eq!(a.len(), len);

        let mut b = String::new();
        append_capped(&mut b, "xy");
        append_capped(&mut b, "z");
        assert_eq!(b, "xyz");
    }

    #[test]
    fn append_capped_cut_rune_is_replaced() {
        // "€" is 3 bytes; the cap falls after its first 2. Go keeps 2 bytes, and
        // its json.Marshal writes one U+FFFD for each.
        let mut a = "a".repeat(MAX_ARTIFACT_BYTES - 3);
        append_capped(&mut a, "b€");
        assert!(a.ends_with("ab\u{FFFD}\u{FFFD}…(truncated)"), "{a:?}");
    }

    #[test]
    fn data_part_content_is_go_json() {
        let ext = request(
            r#"{"method":"message/send","params":{"message":{"parts":[
                {"kind":"data","data":{"z":1,"a":"<x>"}},
                {"kind":"data","data":null},
                {"kind":"file","uri":"u"},
                {"kind":"file","data":"d","uri":"u"},
                {"kind":""},
                {"kind":1},
                7,
                {"kind":"other","text":"t"}
            ]}}}"#,
        )
        .unwrap();
        let got: Vec<(&str, &str)> = ext
            .parts
            .iter()
            .map(|p| (p.kind.as_str(), p.content.as_str()))
            .collect();
        assert_eq!(
            got,
            [
                ("data", r#"{"a":"\u003cx\u003e","z":1}"#),
                ("data", ""),
                ("file", "u"),
                ("file", "d"),
                ("other", ""),
            ]
        );
    }

    #[test]
    fn number_task_id_gives_no_task_id() {
        let mut ext = A2aExtension::default();
        parse_response(
            br#"{"result":{"kind":"task","id":123,"status":{"state":"completed"}}}"#,
            &mut ext,
        );
        assert_eq!(ext.final_status, "completed");
        assert_eq!(ext.task_id, "");
    }

    #[test]
    fn request_rules() {
        assert!(request("").is_none());
        assert!(request(r#"{"method":"tools/call"}"#).is_none());
        assert!(request(r#"[{"method":"message/send"}]"#).is_none());
        let ext = request(
            r#"{"method":"tasks/get","id":1,"params":{"id":"t1","taskId":"t2","sessionId":"s"}}"#,
        )
        .unwrap();
        assert_eq!((ext.task_id.as_str(), ext.session_id.as_str()), ("t1", "s"));
        assert!(!ext.is_action);
        let ext = request(
            r#"{"method":"tasks/pushNotificationConfig/set","params":{"id":"t1","taskId":"t2"}}"#,
        )
        .unwrap();
        assert_eq!(ext.task_id, "t2");
        let ext =
            request(r#"{"method":"message/send","params":{"message":{"contextId":"c"}}}"#).unwrap();
        assert_eq!(ext.session_id, "c");
        assert!(ext.is_action);
    }

    #[test]
    fn response_rules() {
        // A failed task gives the first text part of its status message.
        let mut ext = A2aExtension::default();
        parse_response(
            br#"{"result":{"status":{"state":"failed","message":{"parts":[{"kind":"text","text":""},{"kind":"text","text":"e1"},{"kind":"text","text":"e2"}]}},"artifacts":[{"parts":[{"kind":"text","text":"a1"}]},{"parts":[{"kind":"text","text":"a2"},{"kind":"data"}]}],"contextId":"c","sessionId":"s"}}"#,
            &mut ext,
        );
        assert_eq!(ext.error_message, "e1");
        assert_eq!(ext.artifact, "a2");
        assert_eq!(ext.session_id, "c");

        // A number part kind fails the send decode; the stream decode then sees no artifact kind.
        let mut ext = A2aExtension::default();
        parse_response(
            br#"{"result":{"kind":"artifact-update","taskId":"t","status":{"state":"x","message":{"parts":[{"kind":1}]}},"artifact":{"parts":[{"kind":"text","text":"p"}]}}}"#,
            &mut ext,
        );
        assert_eq!(ext.final_status, "");
        assert_eq!(ext.task_id, "t");
        assert_eq!(ext.artifact, "p");

        // An SSE body: artifact-update texts are joined, and the session id comes from a later event.
        let mut ext = A2aExtension::default();
        parse_response(
            b"data: {\"result\":{\"kind\":\"artifact-update\",\"artifact\":{\"parts\":[{\"kind\":\"text\",\"text\":\"a\"}]}}}\n\ndata: {\"result\":{\"kind\":\"artifact\",\"sessionId\":\"s\",\"artifact\":{\"parts\":[{\"kind\":\"text\",\"text\":\"b\"}]}}}\n",
            &mut ext,
        );
        assert_eq!(ext.artifact, "ab");
        assert_eq!(ext.session_id, "s");
    }
}

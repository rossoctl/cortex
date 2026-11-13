//! Port of core/parse/mcp/mcp.go: parses MCP JSON-RPC 2.0 requests and responses.

mod observer;

use http::HeaderMap;
use serde_json::Map;
use serde_json::value::RawValue;

use crate::parse::decode::decode_request_body;
use crate::parse::inference::gojson::{self, R};
use crate::parse::inference::text::go_lossy_utf8;
use crate::parse::jsonrpc;
use crate::parse::model::{McpError, McpExtension};

pub use observer::McpObserver;

const SYNTHETIC_TRANSPORT_STREAM: &str = "$transport/stream";
const SYNTHETIC_TRANSPORT_TERMINATE: &str = "$transport/terminate";

/// Port of isMCPAction (MCP spec revision 2025-03-26).
fn is_mcp_action(method: &str) -> bool {
    matches!(method, "tools/call" | "prompts/get" | "resources/read")
}

/// Port of isMCPMethod (MCP spec revision 2025-06-18).
fn is_mcp_method(method: &str) -> bool {
    matches!(method, "initialize" | "ping")
        || [
            "notifications/",
            "tools/",
            "resources/",
            "prompts/",
            "completion/",
            "logging/",
            "sampling/",
            "roots/",
            "elicitation/",
        ]
        .iter()
        .any(|p| method.starts_with(p))
}

/// Port of bypass.Matcher.Match for the default mcp-parser paths, `["/mcp"]`.
/// shortcut: only the default "/mcp" pattern, no globs; port path.Match when praxis mode reads mcp-parser config
fn is_mcp_path(path: &str) -> bool {
    let path = path.split_once('?').map_or(path, |(p, _)| p);
    go_path_clean(path) == "/mcp"
}

/// Port of Go's path.Clean.
fn go_path_clean(path: &str) -> String {
    if path.is_empty() {
        return ".".to_owned();
    }
    let rooted = path.starts_with('/');
    let mut out: Vec<&str> = Vec::new();
    for elem in path.split('/') {
        match elem {
            "" | "." => {}
            ".." => match out.last() {
                Some(&l) if l != ".." => {
                    out.pop();
                }
                // A rooted path cannot go above the root.
                _ if rooted => {}
                _ => out.push(".."),
            },
            e => out.push(e),
        }
    }
    let joined = out.join("/");
    match (rooted, joined.is_empty()) {
        (true, _) => format!("/{joined}"),
        (false, true) => ".".to_owned(),
        (false, false) => joined,
    }
}

/// Port of mcp.ParseRequest with the default paths matcher `["/mcp"]`. `path` may
/// have a query. `None` when the parser does not claim the request.
pub fn parse_request(
    method: &str,
    path: &str,
    headers: &HeaderMap,
    body: &[u8],
) -> Option<McpExtension> {
    let body = decode_request_body(headers, body);
    if body.is_empty() {
        if !is_mcp_path(path) {
            return None;
        }
        let session = headers.get("mcp-session-id").is_some_and(|v| !v.is_empty());
        let synthetic = match method {
            "DELETE" if session => SYNTHETIC_TRANSPORT_TERMINATE,
            "GET" => SYNTHETIC_TRANSPORT_STREAM,
            _ => return None,
        };
        return Some(McpExtension {
            method: synthetic.to_owned(),
            ..Default::default()
        });
    }
    let rpc = jsonrpc::decode_request(&body)?;
    if !is_mcp_method(&rpc.method) {
        return None;
    }
    Some(McpExtension {
        is_action: is_mcp_action(&rpc.method),
        method: rpc.method,
        rpc_id: rpc.id,
        params: rpc.params,
        ..Default::default()
    })
}

/// Reports whether each of the top-level keys `method` and `params` occurs at
/// most once, with the exact name. `parse_request` folds case and merges a
/// repeated `params` as Go does. A TypeScript or Python MCP server does
/// neither, so a policy decision must not use a body that fails this check.
pub fn top_level_keys_exact(headers: &HeaderMap, body: &[u8]) -> bool {
    let body = decode_request_body(headers, body);
    if body.is_empty() {
        return true;
    }
    let text = go_lossy_utf8(&body).text;
    let Some(entries) = gojson::parse(&text)
        .ok()
        .and_then(|raw| gojson::object(raw).ok().flatten())
    else {
        return false;
    };
    ["method", "params"].iter().all(|name| {
        let mut hits = entries.iter().filter(|(k, _)| gojson::is(k, name));
        hits.next().is_none_or(|(k, _)| k == name) && hits.next().is_none()
    })
}

/// Port of mcp.ParseResponse: reads one MCP response (plain JSON-RPC, or an SSE
/// body or frame) into `ext`. False, with `ext` unchanged, when `body` holds no
/// JSON-RPC result or error.
pub fn parse_response(body: &[u8], ext: &mut McpExtension) -> bool {
    let Some(rpc) = parse_rpc_response(body) else {
        return false;
    };
    if let Some(e) = rpc.error {
        ext.err = Some(e);
    } else if rpc.result.is_some() {
        ext.result = rpc.result;
    }
    true
}

/// Port of jsonRPCResponse. `id` is decoded only for its type errors.
#[derive(Default)]
struct RpcResponse {
    result: Option<Map<String, serde_json::Value>>,
    error: Option<McpError>,
}

impl RpcResponse {
    fn answers(&self) -> bool {
        self.result.is_some() || self.error.is_some()
    }
}

/// Port of parseResponse: a whole-body decode first, then the SSE `data:` lines.
/// The first one with a result or an error wins.
fn parse_rpc_response(body: &[u8]) -> Option<RpcResponse> {
    if let Some(r) = decode_response(body).filter(RpcResponse::answers) {
        return Some(r);
    }
    let mut found = None;
    jsonrpc::for_each_data_payload(body, |data| {
        found = decode_response(data).filter(RpcResponse::answers);
        found.is_some()
    });
    found
}

/// `json.Unmarshal` into a jsonRPCResponse. `None` on any decode error.
fn decode_response(body: &[u8]) -> Option<RpcResponse> {
    let text = go_lossy_utf8(body).text;
    let raw = gojson::parse(&text).ok()?;
    decode_response_into(raw).ok()
}

fn decode_response_into(raw: &RawValue) -> R<RpcResponse> {
    let mut r = RpcResponse::default();
    // A non-object top-level value is a type error for a struct target.
    let Some(entries) = gojson::object(raw)? else {
        return Ok(r);
    };
    for (k, v) in entries {
        if gojson::is(&k, "id") {
            gojson::any(v)?;
        } else if gojson::is(&k, "result") {
            match gojson::object(v)? {
                None => r.result = None,
                // Go decodes a second object into the map it already has.
                Some(entries) => {
                    let m = r.result.get_or_insert_with(Map::new);
                    for (rk, rv) in entries {
                        m.insert(rk, gojson::any(rv)?.unwrap_or(serde_json::Value::Null));
                    }
                }
            }
        } else if gojson::is(&k, "error") {
            gojson::ptr_into(&mut r.error, v, decode_error_into)?;
        }
    }
    Ok(r)
}

/// Port of jsonRPCError: `code` is a Go int, so a fraction is a type error.
fn decode_error_into(raw: &RawValue, e: &mut McpError) -> R {
    for (k, v) in gojson::object(raw)?.unwrap_or_default() {
        if gojson::is(&k, "code") {
            gojson::int_into(&mut e.code, v)?;
        } else if gojson::is(&k, "message") {
            gojson::string_into(&mut e.message, v)?;
        } else if gojson::is(&k, "data") {
            e.data = gojson::any(v)?;
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn post(body: &str) -> Option<McpExtension> {
        parse_request("POST", "/mcp", &HeaderMap::new(), body.as_bytes())
    }

    #[test]
    fn later_response_frame_is_ignored() {
        let mut obs = McpObserver::on_request(
            "POST",
            "/mcp",
            &HeaderMap::new(),
            br#"{"id":1,"method":"tools/call"}"#,
        );
        obs.on_response_frame(br#"{"id":1,"result":{"a":1}}"#, false);
        obs.on_response_frame(br#"{"id":1,"error":{"code":-1,"message":"m"}}"#, false);
        obs.on_response_frame(b"", true);
        let ext = obs.extension().unwrap();
        assert!(ext.result.is_some());
        assert_eq!(ext.err, None);
    }

    #[test]
    fn path_clean_matches_go() {
        for p in ["/mcp", "/mcp/", "//mcp", "/a/../mcp", "/mcp?x=1", "/../mcp"] {
            assert!(is_mcp_path(p), "{p}");
        }
        for p in ["/mcpx", "/mcp/x", "mcp", "", "/"] {
            assert!(!is_mcp_path(p), "{p}");
        }
        assert_eq!(go_path_clean(""), ".");
        assert_eq!(go_path_clean("a/../.."), "..");
        assert_eq!(go_path_clean("a/./b/"), "a/b");
    }

    #[test]
    fn bodyless_requests() {
        let mut h = HeaderMap::new();
        assert_eq!(parse_request("DELETE", "/mcp", &h, b""), None);
        assert_eq!(
            parse_request("GET", "/mcp?x", &h, b"").unwrap().method,
            SYNTHETIC_TRANSPORT_STREAM
        );
        assert_eq!(parse_request("GET", "/other", &h, b""), None);
        h.insert("Mcp-Session-Id", "s".parse().unwrap());
        assert_eq!(
            parse_request("DELETE", "/mcp", &h, b"").unwrap().method,
            SYNTHETIC_TRANSPORT_TERMINATE
        );
        // The method compare is exact, as in Go.
        assert_eq!(parse_request("get", "/mcp", &h, b""), None);
    }

    #[test]
    fn non_object_params_is_not_claimed() {
        assert_eq!(post(r#"{"method":"tools/call","params":[1]}"#), None);
        assert_eq!(post(r#"{"method":"tasks/get"}"#), None);
        assert_eq!(post(r#"["tools/call"]"#), None);
        let ext = post(r#"{"method":"tools/list","id":"a"}"#).unwrap();
        assert!(!ext.is_action);
        assert_eq!(ext.rpc_id, Some("a".into()));
    }

    #[test]
    fn empty_result_object_counts() {
        let mut ext = McpExtension::default();
        assert!(parse_response(br#"{"result":{}}"#, &mut ext));
        assert_eq!(ext.result, Some(Map::new()));
        assert_eq!(serde_json::to_string(&ext).unwrap(), "{}");
    }

    #[test]
    fn error_code_fraction_is_not_a_response() {
        let mut ext = McpExtension::default();
        assert!(!parse_response(
            br#"{"error":{"code":1.5,"message":"m"}}"#,
            &mut ext
        ));
        assert_eq!(ext, McpExtension::default());
    }

    #[test]
    fn sse_body_first_answer_wins() {
        let body = b"data: {\"method\":\"notifications/message\"}\n\ndata: bad\n\n\
            data: {\"error\":{\"code\":-1,\"message\":\"m\",\"data\":[1]}}\n\n\
            data: {\"result\":{\"a\":1}}\n\n";
        let mut ext = McpExtension::default();
        assert!(parse_response(body, &mut ext));
        let e = ext.err.unwrap();
        assert_eq!((e.code, e.message.as_str()), (-1, "m"));
        assert_eq!(e.data, Some(serde_json::json!([1.0])));
        assert_eq!(ext.result, None);
    }
}

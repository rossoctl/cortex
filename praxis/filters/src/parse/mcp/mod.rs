//! Port of core/parse/mcp/mcp.go: parses MCP JSON-RPC 2.0 requests and responses.

mod observer;

use http::HeaderMap;
use serde_json::Map;
use serde_json::value::RawValue;

use crate::parse::decode::{Coding, Decoder, MAX_DECODED_REQUEST_SIZE, decode_request_body};
use crate::parse::inference::gojson::{self, R};
use crate::parse::inference::text::go_lossy_utf8;
use crate::parse::jsonrpc;
use crate::parse::model::{McpError, McpExtension};

pub use observer::{McpObserver, PolicyView};

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

/// Reports whether the envelope and `params` of the raw body read the same in
/// `parse_request`, in the Praxis `policy` filter and in a TypeScript or Python
/// MCP server. `parse_request` decodes zstd, replaces bad UTF-8, folds case and
/// merges a repeated `params`, as Go does. The policy filter parses the raw bytes
/// with `serde_json`. True for an empty body, and for strict JSON in which:
/// - each of the top-level keys `method` and `params` occurs at most once, with
///   the exact name;
/// - no object in `params`, at any depth, has two keys that are equal when case
///   is ignored. A Go server keeps the last of them for a struct field, and
///   merges a repeated object into a map field.
pub fn reads_alike(body: &[u8]) -> bool {
    if body.is_empty() {
        return true;
    }
    if serde_json::from_slice::<serde_json::Value>(body).is_err() {
        return false;
    }
    let Ok(text) = std::str::from_utf8(body) else {
        return false;
    };
    let Some(entries) = gojson::parse(text)
        .ok()
        .and_then(|raw| gojson::object(raw).ok().flatten())
    else {
        return false;
    };
    ["method", "params"].iter().all(|name| {
        let mut hits = entries.iter().filter(|(k, _)| gojson::is(k, name));
        hits.next().is_none_or(|(k, _)| k == name) && hits.next().is_none()
    }) && entries
        .iter()
        .filter(|(k, _)| k == "params")
        .all(|(_, v)| !keys_collide(v))
}

/// Reports whether a body that `parse_request` does not claim can hold an MCP
/// request for a reader that is more lenient than `serde_json`. The body is
/// decoded (zstd or gzip), a UTF-8 byte order mark is removed, and UTF-16 or
/// UTF-32 is decoded. Then only brackets and strings are read, so a number
/// that `serde_json` refuses (`1e400`, `NaN`, `Infinity`) and data after the
/// first value do not matter. True when the first value closes, and a key of
/// it, or of an object in it when it is an array (a batch), matches `method`
/// when case is ignored and holds an MCP method or a value that is not a
/// string. Also true for a body whose `Content-Type` holds `json`, in another
/// content coding or in one that does not decode, because the scan cannot
/// read it.
pub fn mcp_shaped(headers: &HeaderMap, body: &[u8]) -> bool {
    match decoded(headers, body) {
        Some(body) => scan_methods(&lenient_text(&body)),
        None => names_json(headers),
    }
}

/// The body without its content coding: zstd, as `parse_request` decodes it,
/// or gzip. `None` for another coding, for more than one, and for a body that
/// does not decode.
fn decoded<'a>(headers: &HeaderMap, body: &'a [u8]) -> Option<std::borrow::Cow<'a, [u8]>> {
    let mut codings = headers.get_all(http::header::CONTENT_ENCODING).iter();
    let (Some(coding), None) = (codings.next(), codings.next()) else {
        return headers
            .get(http::header::CONTENT_ENCODING)
            .is_none()
            .then_some(std::borrow::Cow::Borrowed(body));
    };
    match coding.to_str().ok()?.trim().to_ascii_lowercase().as_str() {
        "" | "identity" => Some(std::borrow::Cow::Borrowed(body)),
        "zstd" => match decode_request_body(headers, body) {
            std::borrow::Cow::Owned(b) => Some(std::borrow::Cow::Owned(b)),
            std::borrow::Cow::Borrowed(_) => None,
        },
        "gzip" | "x-gzip" => {
            let mut d = Decoder::new(Coding::Gzip, MAX_DECODED_REQUEST_SIZE);
            let mut out = d.push(body).ok()?;
            out.extend(d.finish().ok()?);
            Some(std::borrow::Cow::Owned(out))
        }
        _ => None,
    }
}

/// Reports whether the `Content-Type` header holds `json` in any place. The
/// TypeScript SDK accepts a header that holds `application/json` in any place,
/// for example in a parameter. The Python SDK compares the media type.
fn names_json(headers: &HeaderMap) -> bool {
    content_type(headers).is_some_and(|ct| ct.contains("json"))
}

/// Reports whether a `Content-Type` that `names_json` matches can name a
/// charset other than UTF-8: it has a byte over 0x7f, or a `charset` parameter
/// that is not UTF-8 or US-ASCII. The TypeScript SDK decodes the body with that
/// charset (UTF-7 or Shift_JIS, for example), but the parse and the scans read
/// UTF-8. Each `charset` parameter counts, also one in a quoted string.
pub fn foreign_charset(headers: &HeaderMap) -> bool {
    let Some(ct) = content_type(headers).filter(|ct| ct.contains("json")) else {
        return false;
    };
    !ct.is_ascii()
        || ct
            .split(';')
            .skip(1)
            .filter_map(|p| p.split_once('='))
            .any(|(k, v)| {
                k.trim().trim_start_matches('"').trim() == "charset"
                    && !matches!(
                        v.trim().trim_matches('"').trim(),
                        "utf-8" | "utf8" | "us-ascii"
                    )
            })
}

/// The `Content-Type` header in lower case. An invalid UTF-8 byte becomes U+FFFD.
fn content_type(headers: &HeaderMap) -> Option<String> {
    let v = headers.get(http::header::CONTENT_TYPE)?;
    Some(String::from_utf8_lossy(v.as_bytes()).to_ascii_lowercase())
}

/// The text of a body as a lenient JSON reader sees it: UTF-32 or UTF-16 when
/// a byte order mark or the NUL bytes of the first character show it, else
/// UTF-8 without a byte order mark. A bad code unit becomes U+FFFD.
fn lenient_text(b: &[u8]) -> std::borrow::Cow<'_, str> {
    match b {
        [0, 0, 0xfe, 0xff, rest @ ..] => utf32(rest, u32::from_be_bytes),
        [0xff, 0xfe, 0, 0, rest @ ..] => utf32(rest, u32::from_le_bytes),
        [0xfe, 0xff, rest @ ..] => utf16(rest, u16::from_be_bytes),
        [0xff, 0xfe, rest @ ..] => utf16(rest, u16::from_le_bytes),
        [0, 0, 0, _, ..] => utf32(b, u32::from_be_bytes),
        [_, 0, 0, 0, ..] => utf32(b, u32::from_le_bytes),
        [0, _, ..] => utf16(b, u16::from_be_bytes),
        [_, 0, ..] => utf16(b, u16::from_le_bytes),
        [0xef, 0xbb, 0xbf, rest @ ..] => go_lossy_utf8(rest).text,
        _ => go_lossy_utf8(b).text,
    }
}

fn utf16(b: &[u8], unit: fn([u8; 2]) -> u16) -> std::borrow::Cow<'static, str> {
    let units = b.chunks_exact(2).map(|c| unit([c[0], c[1]]));
    char::decode_utf16(units)
        .map(|r| r.unwrap_or(char::REPLACEMENT_CHARACTER))
        .collect::<String>()
        .into()
}

fn utf32(b: &[u8], unit: fn([u8; 4]) -> u32) -> std::borrow::Cow<'static, str> {
    b.chunks_exact(4)
        .map(|c| {
            char::from_u32(unit([c[0], c[1], c[2], c[3]])).unwrap_or(char::REPLACEMENT_CHARACTER)
        })
        .collect::<String>()
        .into()
}

/// The scan of `mcp_shaped`. Brackets, `:` and `,` are ASCII, and a string
/// ends at a `"` that no backslash escapes, so the scan reads bytes.
fn scan_methods(text: &str) -> bool {
    let b = text.as_bytes();
    let mut stack: Vec<u8> = Vec::new();
    let mut shaped = false;
    // The next string in the current object is a key.
    let mut key_next = false;
    // The next value is the value of a `method` key that counts.
    let mut method_value = false;
    let mut i = 0;
    while i < b.len() {
        match b[i] {
            b'"' => {
                let start = i;
                i += 1;
                while i < b.len() && b[i] != b'"' {
                    i += if b[i] == b'\\' { 2 } else { 1 };
                }
                let Some(lit) = text.get(start..=i) else {
                    return false;
                };
                if stack.is_empty() {
                    return false;
                }
                let counts = matches!(stack.as_slice(), [b'{'] | [b'[', b'{']);
                let value = || escapes_valid(lit).then(|| gojson::unquote(lit));
                if key_next {
                    key_next = false;
                    method_value = counts && value().is_some_and(|k| gojson::is(&k, "method"));
                } else if std::mem::take(&mut method_value) {
                    shaped |= value().is_none_or(|m| is_mcp_method(&m));
                }
            }
            c @ (b'{' | b'[') => {
                shaped |= std::mem::take(&mut method_value);
                stack.push(c);
                key_next = c == b'{';
            }
            b'}' | b']' => {
                stack.pop();
                if stack.is_empty() {
                    return shaped;
                }
            }
            b',' => key_next = stack.last() == Some(&b'{'),
            b':' => {}
            c if c.is_ascii_whitespace() => {}
            _ => {
                if stack.is_empty() {
                    return false;
                }
                shaped |= std::mem::take(&mut method_value);
            }
        }
        i += 1;
    }
    false
}

/// Reports whether each escape in a JSON string literal is one that JSON
/// allows, so that `gojson::unquote` can read it.
fn escapes_valid(lit: &str) -> bool {
    let s = lit.as_bytes();
    let mut i = 1;
    while i + 1 < s.len() {
        if s[i] != b'\\' {
            i += 1;
            continue;
        }
        match s.get(i + 1) {
            Some(b'u')
                if s.get(i + 2..i + 6)
                    .is_some_and(|h| h.iter().all(u8::is_ascii_hexdigit)) =>
            {
                i += 6
            }
            Some(b'"' | b'\\' | b'/' | b'b' | b'f' | b'n' | b'r' | b't') => i += 2,
            _ => return false,
        }
    }
    true
}

/// Reports whether an object in `raw`, at any depth, has two keys with the
/// same `gojson::fold_key`. The serde_json parse in `reads_alike` limits the depth.
fn keys_collide(raw: &RawValue) -> bool {
    match raw.get().as_bytes().first() {
        Some(b'{') => {
            let Ok(Some(entries)) = gojson::object(raw) else {
                return true;
            };
            let mut seen = std::collections::HashSet::new();
            entries
                .iter()
                .any(|(k, v)| !seen.insert(gojson::fold_key(k)) || keys_collide(v))
        }
        Some(b'[') => serde_json::from_str::<Vec<&RawValue>>(raw.get())
            .map_or(true, |items| items.into_iter().any(keys_collide)),
        _ => false,
    }
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

    // The scan reads bodies that no parser checked: a short or bad escape, a
    // backslash before a multibyte character, or a cut in any place.
    #[test]
    fn mcp_shaped_reads_any_prefix_without_panic() {
        let bodies = [
            r#"{"\u12":1,"method":"tools/call","\é":"\ud800\u","params":{"name":"é\\"}}"#,
            r#"[{"method":"tools/call"},{"m":"\\"}] x"#,
        ];
        for body in bodies {
            for end in 0..=body.len() {
                if let Some(prefix) = body.get(..end) {
                    mcp_shaped(&HeaderMap::new(), prefix.as_bytes());
                }
            }
        }
        assert!(mcp_shaped(&HeaderMap::new(), bodies[0].as_bytes()));
        assert!(mcp_shaped(&HeaderMap::new(), bodies[1].as_bytes()));
    }

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

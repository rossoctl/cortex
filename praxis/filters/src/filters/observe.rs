//! The `cortex_observe` filter.
//!
//! It adapts the Praxis hooks to [`Exchange`] and submits each row to the
//! emitter. It records only requests that `cortex_attribution` let through
//! with [`TokenValid`].

use std::net::{SocketAddr, ToSocketAddrs};
use std::sync::{Arc, Mutex};
use std::time::SystemTime;

use async_trait::async_trait;
use bytes::Bytes;
use http::Method;
use http::header::HOST;
use praxis_filter::{
    BodyAccess, BodyMode, FilterAction, FilterError, HttpFilter, HttpFilterContext, Rejection,
};
use serde::Deserialize;

use super::attribution::{TokenValid, read_token};
use crate::parse::inference::gojson;
use crate::parse::mcp::PolicyView;
use crate::record::emitter::{self, Emitter};
use crate::record::line::Line;
use crate::record::row::{EGRESS_ERROR_HEADER, Exchange};

/// The request id that the forward proxy sets on the hop.
pub const REQUEST_ID_HEADER: &str = "X-Cortex-Request-Id";

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Config {
    token_file: String,
    /// `host:port` of the Cortex session API.
    ingest: String,
}

/// One exchange, whether the upstream answered, and whether its response row
/// was submitted. In a `Mutex`, because `emit_deferred_record` gets only
/// `&HttpFilterContext`.
struct Observed {
    exchange: Exchange,
    answered: bool,
    done: bool,
}

type State = Arc<Mutex<Observed>>;

pub struct CortexObserve {
    emitter: Arc<Emitter>,
}

impl CortexObserve {
    pub fn from_config(config: &serde_yaml::Value) -> Result<Box<dyn HttpFilter>, FilterError> {
        let cfg: Config = praxis_filter::parse_filter_config("cortex_observe", config)?;
        let token = read_token(&cfg.token_file)?;
        let addr: SocketAddr = cfg
            .ingest
            .to_socket_addrs()
            .map_err(|e| format!("cortex_observe: bad ingest address {}: {e}", cfg.ingest))?
            .next()
            .ok_or_else(|| format!("cortex_observe: no address for {}", cfg.ingest))?;
        Ok(Box::new(CortexObserve {
            emitter: emitter::global(addr, token),
        }))
    }

    fn submit(&self, mut line: Line) {
        crate::policy_recorder::attach(&mut line);
        self.emitter.submit(line.queued());
    }

    /// Submits the response row once.
    fn finish(&self, o: &mut Observed, row: impl FnOnce(&mut Exchange) -> Line) {
        if !o.done {
            o.done = true;
            self.submit(row(&mut o.exchange));
        }
    }
}

fn state(ctx: &HttpFilterContext<'_>) -> Option<State> {
    ctx.get_filter_state::<State>().cloned()
}

fn lock(s: &State) -> std::sync::MutexGuard<'_, Observed> {
    s.lock().unwrap_or_else(|p| p.into_inner())
}

/// The longest metadata value that `cortex_observe` writes. `set_metadata`
/// also refuses a longer value, but a dropped key is the safe result: the
/// policy filter then fails closed (500 or deny), and a truncated name could
/// match an allow rule.
const MAX_METADATA_VALUE: usize = 256;

/// The `filter_metadata` keys that the Praxis `policy` filter reads for an
/// MCP request.
fn mcp_metadata(view: PolicyView<'_>) -> Vec<(&'static str, &str)> {
    // A policy filter with a tool, prompt, resource or llm route refuses an
    // entity method with no `mcp.name`: 500, or a deny when the policy has an
    // `llm:` route and the body has a top-level `model`. With no `mcp.method`,
    // a policy with an `llm:` route can admit the body.
    let ext = match view {
        PolicyView::Exact(ext) => ext,
        PolicyView::Ambiguous => return vec![("mcp.method", "tools/call")],
    };
    // A synthetic `$transport/...` method (bodyless GET, or DELETE with an
    // `mcp-session-id`) is set too:
    // without `mcp.method`, a policy with only tool/prompt/resource routes answers 500.
    let mut keys = vec![("mcp.method", ext.method.as_str())];
    let param = match ext.method.as_str() {
        "tools/call" | "prompts/get" => Some("name"),
        "resources/read" => Some("uri"),
        _ => None,
    };
    // A Go server folds case, so `{"name":"a","Name":"b"}` can run `b`, and
    // `Arguments` can hold the arguments that the policy does not read. No
    // `mcp.name` then.
    if let (Some(param), Some(p)) = (param, ext.params.as_ref())
        && p.keys().all(|k| {
            [param, "arguments"]
                .iter()
                .all(|w| k == w || !gojson::is(k, w))
        })
        && let Some(name) = p.get(param).and_then(|v| v.as_str())
    {
        keys.push(("mcp.name", name));
    }
    keys.retain(|(_, v)| v.len() <= MAX_METADATA_VALUE);
    keys
}

/// A response that has no body, so no body hook ends it.
fn bodyless(method: &Method, status: u16, headers: &http::HeaderMap) -> bool {
    *method == Method::HEAD
        || status == 204
        || status == 304
        || (100..200).contains(&status)
        || headers
            .get(http::header::CONTENT_LENGTH)
            .is_some_and(|v| v.as_bytes() == b"0")
}

#[async_trait]
impl HttpFilter for CortexObserve {
    fn name(&self) -> &'static str {
        "cortex_observe"
    }

    // The exchange must exist before the request body pre-read, which runs
    // before `on_request`.
    fn runs_request_head(&self) -> bool {
        true
    }

    async fn on_request_head(
        &self,
        ctx: &mut HttpFilterContext<'_>,
    ) -> Result<FilterAction, FilterError> {
        let Some(TokenValid(attribution)) = ctx.extensions.get::<TokenValid>() else {
            return Ok(FilterAction::Continue);
        };
        let attribution = attribution.clone();
        let req = ctx.request;
        let request_id = req
            .headers
            .get(REQUEST_ID_HEADER)
            .and_then(|v| v.to_str().ok())
            .filter(|v| !v.is_empty())
            .map(str::to_owned)
            .or_else(|| ctx.request_id().map(str::to_owned))
            .unwrap_or_else(|| ctx.id_generator.generate(ctx.time_source));
        let host = req
            .uri
            .host()
            .or_else(|| req.headers.get(HOST).and_then(|v| v.to_str().ok()))
            .unwrap_or("");
        let path = req.uri.path_and_query().map_or("/", |p| p.as_str());
        let exchange = Exchange::new(
            req.method.as_str(),
            host,
            path,
            &req.headers,
            attribution,
            request_id,
            SystemTime::now(),
            ctx.request_start,
        );
        let s: State = Arc::new(Mutex::new(Observed {
            exchange,
            answered: false,
            done: false,
        }));
        ctx.insert_filter_state(s);
        Ok(FilterAction::Continue)
    }

    async fn on_request(
        &self,
        _ctx: &mut HttpFilterContext<'_>,
    ) -> Result<FilterAction, FilterError> {
        Ok(FilterAction::Continue)
    }

    fn request_body_access(&self) -> BodyAccess {
        BodyAccess::ReadOnly
    }

    fn request_body_mode(&self) -> BodyMode {
        BodyMode::StreamBuffer { max_bytes: None }
    }

    fn response_body_access(&self) -> BodyAccess {
        BodyAccess::ReadOnly
    }

    fn response_body_mode(&self) -> BodyMode {
        BodyMode::Stream
    }

    async fn on_request_body(
        &self,
        ctx: &mut HttpFilterContext<'_>,
        body: &mut Option<Bytes>,
        end_of_stream: bool,
    ) -> Result<FilterAction, FilterError> {
        // StreamBuffer: at the end of the stream, `body` is the whole body.
        if end_of_stream && let Some(s) = state(ctx) {
            let (line, metadata) = {
                let mut o = lock(&s);
                let line = o
                    .exchange
                    .on_request_body(body.as_deref().unwrap_or_default());
                let metadata: Vec<(&'static str, String)> = o
                    .exchange
                    .mcp()
                    .map(mcp_metadata)
                    .unwrap_or_default()
                    .into_iter()
                    .map(|(k, v)| (k, v.to_owned()))
                    .collect();
                (line, metadata)
            };
            self.submit(line);
            for (k, v) in metadata {
                ctx.set_metadata(k, v);
            }
        }
        Ok(FilterAction::Continue)
    }

    async fn on_response(
        &self,
        ctx: &mut HttpFilterContext<'_>,
    ) -> Result<FilterAction, FilterError> {
        let Some(s) = state(ctx) else {
            return Ok(FilterAction::Continue);
        };
        let method = ctx.request.method.clone();
        let Some(resp) = ctx.response_header.as_deref_mut() else {
            return Ok(FilterAction::Continue);
        };
        let status = resp.status.as_u16();
        let had_egress_error = resp.headers.contains_key(EGRESS_ERROR_HEADER);
        let empty = bodyless(&method, status, &resp.headers);
        let mut o = lock(&s);
        o.answered = true;
        let head = o.exchange.on_response_head(status, &mut resp.headers);
        if had_egress_error {
            ctx.response_headers_modified = true;
        }
        if let Err(a) = head {
            self.finish(&mut o, |e| e.on_abort(a));
            return Ok(FilterAction::Reject(Rejection::status(502)));
        }
        if empty {
            self.finish(&mut o, Exchange::on_response_end);
        }
        Ok(FilterAction::Continue)
    }

    fn on_response_body(
        &self,
        ctx: &mut HttpFilterContext<'_>,
        body: &mut Option<Bytes>,
        end_of_stream: bool,
    ) -> Result<FilterAction, FilterError> {
        let Some(s) = state(ctx) else {
            return Ok(FilterAction::Continue);
        };
        let mut o = lock(&s);
        if o.done {
            return Ok(FilterAction::Continue);
        }
        if let Some(chunk) = body.as_deref()
            && let Err(a) = o.exchange.on_response_chunk(chunk)
        {
            self.finish(&mut o, |e| e.on_abort(a));
            return Err("cortex_observe: response over a cap".into());
        }
        if end_of_stream {
            self.finish(&mut o, Exchange::on_response_end);
        }
        Ok(FilterAction::Continue)
    }

    fn emit_deferred_record(&self, ctx: &HttpFilterContext<'_>, status: u16) -> bool {
        // The pipeline does not set `current_filter_id` for this hook, so
        // `get_filter_state` cannot find the state. Find it by its type.
        let Some(s) = ctx
            .filter_state
            .values()
            .find_map(|b| b.downcast_ref::<State>())
        else {
            return false;
        };
        let mut o = lock(s);
        if o.done {
            return false;
        }
        o.done = true;
        // Before the response head, Praxis 0.8.0 gives a client that went
        // away as status 499, or as 0 when it wrote nothing. After the head, it gives the written status and
        // `upstream_reached` for both a client and an upstream that went away.
        // shortcut: a stream cut after the head is always `client_canceled`
        // (the frequent case: a user stop or a Cortex idle cut), so an
        // upstream that drops mid-body reads the same; upgrade when Praxis
        // gives the error source to `emit_deferred_record`.
        let upstream_gone = ctx.upstream_reached && ctx.stream_termination().is_some();
        let downstream_gone = o.answered;
        for line in o.exchange.deferred(status, downstream_gone, upstream_gone) {
            self.submit(line);
        }
        true
    }
}

#[cfg(test)]
mod tests {
    use http::HeaderMap;

    use super::*;
    use crate::parse::mcp::parse_request;

    fn keys(method: &str, body: &[u8]) -> Vec<(&'static str, String)> {
        let ext = parse_request(method, "/mcp", &HeaderMap::new(), body).expect("an MCP request");
        mcp_metadata(PolicyView::Exact(&ext))
            .into_iter()
            .map(|(k, v)| (k, v.to_owned()))
            .collect()
    }

    fn rpc(method: &str, params: &str) -> Vec<u8> {
        format!(r#"{{"jsonrpc":"2.0","id":1,"method":"{method}","params":{params}}}"#).into_bytes()
    }

    fn pairs(p: &[(&'static str, &str)]) -> Vec<(&'static str, String)> {
        p.iter().map(|(k, v)| (*k, (*v).to_owned())).collect()
    }

    #[test]
    fn tools_call_sets_method_and_name() {
        let got = keys(
            "POST",
            &rpc("tools/call", r#"{"name":"safe_tool","arguments":{}}"#),
        );
        assert_eq!(
            got,
            pairs(&[("mcp.method", "tools/call"), ("mcp.name", "safe_tool")])
        );
    }

    #[test]
    fn prompts_get_name_comes_from_name() {
        let got = keys("POST", &rpc("prompts/get", r#"{"name":"greet"}"#));
        assert_eq!(
            got,
            pairs(&[("mcp.method", "prompts/get"), ("mcp.name", "greet")])
        );
    }

    #[test]
    fn resources_read_name_comes_from_uri() {
        let got = keys("POST", &rpc("resources/read", r#"{"uri":"file:///a.txt"}"#));
        assert_eq!(
            got,
            pairs(&[
                ("mcp.method", "resources/read"),
                ("mcp.name", "file:///a.txt")
            ])
        );
    }

    #[test]
    fn tools_list_sets_only_method() {
        let got = keys("POST", &rpc("tools/list", "{}"));
        assert_eq!(got, pairs(&[("mcp.method", "tools/list")]));
    }

    #[test]
    fn long_name_is_dropped() {
        let name = "a".repeat(300);
        let got = keys(
            "POST",
            &rpc("tools/call", &format!(r#"{{"name":"{name}"}}"#)),
        );
        assert_eq!(got, pairs(&[("mcp.method", "tools/call")]));
    }

    #[test]
    fn name_that_is_not_a_string_is_dropped() {
        let got = keys("POST", &rpc("tools/call", r#"{"name":1}"#));
        assert_eq!(got, pairs(&[("mcp.method", "tools/call")]));
    }

    #[test]
    fn name_that_matches_only_by_case_is_dropped() {
        for params in [
            r#"{"name":"safe_tool","Name":"danger_tool"}"#,
            r#"{"NAME":"danger_tool"}"#,
        ] {
            let got = keys("POST", &rpc("tools/call", params));
            assert_eq!(got, pairs(&[("mcp.method", "tools/call")]), "{params}");
        }
        let got = keys("POST", &rpc("resources/read", r#"{"uri":"a","URI":"b"}"#));
        assert_eq!(got, pairs(&[("mcp.method", "resources/read")]));
        // The policy reads only the exact `arguments` key for `args.*` rules.
        for (method, params) in [
            (
                "tools/call",
                r#"{"name":"t","Arguments":{"path":"/etc/passwd"}}"#,
            ),
            (
                "tools/call",
                r#"{"name":"t","arguments":{"path":"ok"},"Arguments":{"path":"/etc/passwd"}}"#,
            ),
            ("prompts/get", r#"{"name":"p","ARGUMENTS":{"x":"y"}}"#),
        ] {
            let got = keys("POST", &rpc(method, params));
            assert_eq!(got, pairs(&[("mcp.method", method)]), "{params}");
        }
    }

    #[test]
    fn method_without_entity_sets_no_name() {
        let got = keys("POST", &rpc("tools/list", r#"{"":"safe_tool"}"#));
        assert_eq!(got, pairs(&[("mcp.method", "tools/list")]));
    }

    #[test]
    fn synthetic_transport_row_sets_only_method() {
        assert_eq!(
            keys("GET", b""),
            pairs(&[("mcp.method", "$transport/stream")])
        );
        let mut session = HeaderMap::new();
        session.insert("mcp-session-id", http::HeaderValue::from_static("s1"));
        let ext = parse_request("DELETE", "/mcp", &session, b"").expect("an MCP request");
        assert_eq!(
            mcp_metadata(PolicyView::Exact(&ext)),
            [("mcp.method", "$transport/terminate")]
        );
    }

    /// The keys that `on_request_body` writes for one POST to /mcp.
    fn written(h: &HeaderMap, body: &[u8]) -> Vec<(&'static str, String)> {
        let mut ex = Exchange::new(
            "POST",
            "tools.example",
            "/mcp",
            h,
            crate::record::attribution::Attribution::default(),
            "req-1".into(),
            SystemTime::now(),
            std::time::Instant::now(),
        );
        ex.on_request_body(body);
        ex.mcp()
            .map(mcp_metadata)
            .unwrap_or_default()
            .into_iter()
            .map(|(k, v)| (k, v.to_owned()))
            .collect()
    }

    // The policy filter reads the raw body with serde_json. A TypeScript or
    // Python server reads the exact keys. When either can read the body in
    // another way than the Go-style parse, a policy with entity routes must
    // refuse it: `tools/call` with no `mcp.name`.
    #[test]
    fn body_that_parsers_read_differently_gets_tools_call_without_name() {
        let none = HeaderMap::new();
        let refused = [("mcp.method", "tools/call".to_owned())];
        let ambiguous: [&[u8]; 28] = [
            br#"{"id":1,"Method":"tools/call","params":{"name":"danger_tool"}}"#,
            br#"{"JSONRPC":"2.0","id":1,"Method":"tools/call","params":{"name":"danger_tool"}}"#,
            br#"{"method":"tools/list","Method":"tools/call","params":{"name":"danger_tool"}}"#,
            br#"{"method":"tools/call","params":{"name":"danger"},"Params":{"name":"safe"}}"#,
            br#"{"method":"tools/call","params":{"name":"danger"},"params":{"name":"safe"}}"#,
            br#"{"method":"tools/list","method":"tools/call","params":{"name":"danger"}}"#,
            b"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"write_file\",\"arguments\":{\"x\":\"\xff\"}}}",
            br#"{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_file","arguments":{"x":"\ud800"}}}"#,
            br#"{"method":"tools/call","params":{"name":"t","arguments":{"path":"ok","Path":"/etc/passwd"}}}"#,
            br#"{"method":"tools/call","params":{"name":"t","arguments":{"a":"ok"},"arguments":{"b":"x"}}}"#,
            br#"{"method":"tools/call","params":{"name":"t","arguments":{"list":[{"k":1,"K":2}]}}}"#,
            br#"{"method":"tools/call","params":{"name":"safe","Name":"danger"}}"#,
            br#"{"method":"prompts/get","params":{"name":"p","arguments":{"x":"1","x":"2"}}}"#,
            // Numbers out of f64 range: no claim by the Go-style parse, and no
            // serde_json parse in the policy. Node and Python read them, and Go reads
            // them into a field that it skips.
            br#"{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"danger_tool","arguments":{},"pad":1e400}}"#,
            br#"{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"danger_tool","pad":-1e400}}"#,
            br#"{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"danger_tool","arguments":{"x":1e400}}}"#,
            br#"{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"danger_tool","_meta":{"t":1e400}}}"#,
            br#"{"jsonrpc":"2.0","id":1e400,"method":"tools/call","params":{"name":"danger_tool"}}"#,
            // A Go json.Decoder reads the first value and ignores the rest.
            br#"{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"danger_tool"}} x"#,
            br#"{"jsonrpc":"2.0","id":1,"Method":"tools/call","params":{"name":"danger_tool","pad":1e400}}"#,
            br#"{"jsonrpc":"2.0","id":1,"method":7,"METHOD":"tools/call","params":{"pad":1e400}}"#,
            br#"{"jsonrpc":"2.0","id":1,"method":"tools/call","\ud800":1,"params":{"name":"danger_tool","pad":1e400}}"#,
            // Parsers that accept NaN and Infinity (pydantic in the Python SDK).
            br#"{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"danger_tool","_meta":{"t":NaN}}}"#,
            br#"{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"danger_tool","arguments":{"x":Infinity}}}"#,
            br#"{"jsonrpc":"2.0","id":-Infinity,"method":"tools/call","params":{"name":"danger_tool"}}"#,
            br#"{"jsonrpc":"2.0","id":1,"\u006dethod":"tools/call","params":{"name":"danger_tool","x":NaN}}"#,
            // A UTF-8 BOM: Node's TextDecoder and Python remove it.
            b"\xef\xbb\xbf{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"danger_tool\"}}",
            // A batch: the TypeScript SDK runs each element.
            br#"[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"danger_tool"}}]"#,
        ];
        for body in ambiguous {
            assert_eq!(
                written(&none, body),
                refused,
                "{}",
                String::from_utf8_lossy(body)
            );
        }
        // UTF-16 and UTF-32, with and without a BOM: Python's json.loads reads them.
        let text =
            r#"{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"danger_tool"}}"#;
        let le16: Vec<u8> = text.encode_utf16().flat_map(u16::to_le_bytes).collect();
        let be16: Vec<u8> = text.encode_utf16().flat_map(u16::to_be_bytes).collect();
        let le32: Vec<u8> = text
            .chars()
            .flat_map(|c| (c as u32).to_le_bytes())
            .collect();
        let be32: Vec<u8> = text
            .chars()
            .flat_map(|c| (c as u32).to_be_bytes())
            .collect();
        let bom16: Vec<u8> = [0xff, 0xfe]
            .into_iter()
            .chain(le16.iter().copied())
            .collect();
        for (what, body) in [
            ("utf-16le", &le16),
            ("utf-16be", &be16),
            ("utf-32le", &le32),
            ("utf-32be", &be32),
            ("utf-16le bom", &bom16),
        ] {
            assert_eq!(written(&none, body), refused, "{what}");
        }

        // The other BOM arms of the decode.
        let bom16be: Vec<u8> = [0xfe, 0xff]
            .into_iter()
            .chain(be16.iter().copied())
            .collect();
        let bom32le: Vec<u8> = [0xff, 0xfe, 0, 0]
            .into_iter()
            .chain(le32.iter().copied())
            .collect();
        let bom32be: Vec<u8> = [0, 0, 0xfe, 0xff]
            .into_iter()
            .chain(be32.iter().copied())
            .collect();
        for (what, body) in [
            ("utf-16be bom", &bom16be),
            ("utf-32le bom", &bom32le),
            ("utf-32be bom", &bom32be),
        ] {
            assert_eq!(written(&none, body), refused, "{what}");
        }

        let ex: &[u8] = br#"{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t"}}"#;
        // The TypeScript SDK decodes the body with the Content-Type charset.
        let with = |pairs: &[(http::header::HeaderName, &'static str)]| {
            let mut h = HeaderMap::new();
            for (k, v) in pairs {
                h.insert(k, http::HeaderValue::from_static(v));
            }
            h
        };
        let ct = http::header::CONTENT_TYPE;
        let ce = http::header::CONTENT_ENCODING;
        let utf7 = with(&[(ct.clone(), "application/json; charset=utf-7")]);
        let utf7_body = b"{+ACI-jsonrpc+ACI-:+ACI-2.0+ACI-,+ACI-id+ACI-:1,+ACI-method+ACI-:+ACI-tools/call+ACI-,+ACI-params+ACI-:{+ACI-name+ACI-:+ACI-danger+ACI-}}";
        assert_eq!(written(&utf7, utf7_body), refused, "utf-7");
        let sjis = with(&[(ct.clone(), "application/json;charset=Shift_JIS")]);
        assert_eq!(
            written(&sjis, ex),
            refused,
            "shift_jis, also when the parser claims it"
        );
        let utf8 = with(&[(ct.clone(), "application/json; charset=\"UTF-8\"")]);
        assert_eq!(
            written(&utf8, ex),
            pairs(&[("mcp.method", "tools/call"), ("mcp.name", "t")]),
            "utf-8"
        );
        // The TypeScript SDK accepts a header with `application/json` anywhere,
        // and Node reads a header byte over 0x7f.
        let hidden = with(&[(
            ct.clone(),
            "text/plain; x=\"application/json\"; charset=utf-7",
        )]);
        assert_eq!(written(&hidden, utf7_body), refused, "json in a parameter");
        let hidden_sjis = with(&[(
            ct.clone(),
            "text/plain; x=\"application/json\"; charset=shift_jis",
        )]);
        assert_eq!(
            written(&hidden_sjis, ex),
            refused,
            "json in a parameter, claimed"
        );
        let mut obs = HeaderMap::new();
        obs.insert(
            ct.clone(),
            http::HeaderValue::from_bytes(b"application/json; x=\"\xe9\"; charset=utf-7").unwrap(),
        );
        assert_eq!(written(&obs, utf7_body), refused, "a header byte over 0x7f");
        let mut obs_only = HeaderMap::new();
        obs_only.insert(
            ct.clone(),
            http::HeaderValue::from_bytes(b"application/json; x=\"\xe9\"").unwrap(),
        );
        assert_eq!(
            written(&obs_only, ex),
            refused,
            "a header byte over 0x7f, no charset"
        );
        let ascii = with(&[(ct.clone(), "application/json; charset=US-ASCII")]);
        assert_eq!(
            written(&ascii, ex),
            pairs(&[("mcp.method", "tools/call"), ("mcp.name", "t")]),
            "us-ascii"
        );
        let text = with(&[(ct.clone(), "text/plain; charset=utf-7")]);
        assert_eq!(written(&text, b"hello"), [], "not JSON");

        // gzip is decoded and scanned. Another coding of a JSON body cannot be read.
        let gz = |b: &[u8]| {
            let mut e = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::default());
            std::io::Write::write_all(&mut e, b).unwrap();
            e.finish().unwrap()
        };
        let gzip = with(&[(ce.clone(), "gzip"), (ct.clone(), "application/json")]);
        let inference = br#"{"model":"m","messages":[]}"#;
        assert_eq!(written(&gzip, &gz(ex)), refused, "gzip tools/call");
        assert_eq!(written(&gzip, &gz(inference)), [], "gzip inference");
        let br = with(&[(ce.clone(), "br"), (ct.clone(), "application/json")]);
        assert_eq!(written(&br, b"\x1b\x00\x00"), refused, "br");
        let br_bin = with(&[(ce.clone(), "br"), (ct.clone(), "application/octet-stream")]);
        assert_eq!(written(&br_bin, b"\x1b\x00\x00"), [], "br, not JSON");
        let bad_zstd = with(&[(ce.clone(), "zstd"), (ct.clone(), "application/json")]);
        assert_eq!(
            written(&bad_zstd, b"not zstd"),
            refused,
            "zstd that does not decode"
        );

        // The policy does not decode zstd, so it sees no arguments.
        let mut zstd = HeaderMap::new();
        zstd.insert(
            http::header::CONTENT_ENCODING,
            http::HeaderValue::from_static("zstd"),
        );
        let exact = br#"{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t"}}"#;
        let packed = zstd::encode_all(&exact[..], 0).unwrap();
        assert_eq!(written(&zstd, &packed), refused, "zstd body");

        assert_eq!(
            written(&none, exact),
            pairs(&[("mcp.method", "tools/call"), ("mcp.name", "t")])
        );
        let list = br#"{"jsonrpc":"2.0","id":2,"method":"tools/list"}"#;
        assert_eq!(written(&none, list), pairs(&[("mcp.method", "tools/list")]));
        // No key at the top level, or in a top-level array element, that
        // selects an MCP method: nothing is set. A truncated body sets nothing.
        let not_mcp: [&[u8]; 7] = [
            br#"{"model":"m","messages":[{"role":"user","content":"hi"}],"n":1e400}"#,
            br#"{"model":"m","messages":[{"role":"user","content":[{"type":"tool_use","input":{"method":"tools/call"}}]}],"n":NaN}"#,
            br#"{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"x":1e400}}"#,
            br#"{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t"}"#,
            br#"{"x":"{\"method\":\"tools/call\"}","n":NaN}"#,
            br#"{}{"method":"tools/call"}"#,
            b"not json",
        ];
        for body in not_mcp {
            assert_eq!(
                written(&none, body),
                [],
                "{}",
                String::from_utf8_lossy(body)
            );
        }
    }

    #[test]
    fn batch_and_non_json_bodies_are_not_parsed() {
        let batch = br#"[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t"}}]"#;
        assert!(parse_request("POST", "/mcp", &HeaderMap::new(), batch).is_none());
        assert!(parse_request("POST", "/mcp", &HeaderMap::new(), b"not json").is_none());
    }
}

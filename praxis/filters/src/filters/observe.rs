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
use crate::parse::model::McpExtension;
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

    fn submit(&self, line: &Line) {
        self.emitter.submit(line.queued());
    }

    /// Submits the response row once.
    fn finish(&self, o: &mut Observed, row: impl FnOnce(&mut Exchange) -> Line) {
        if !o.done {
            o.done = true;
            self.submit(&row(&mut o.exchange));
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
fn mcp_metadata(ext: &McpExtension) -> Vec<(&'static str, &str)> {
    // A synthetic `$transport/...` method (bodyless GET or DELETE) is set too:
    // without `mcp.method`, a policy with only tool/prompt/resource routes answers 500.
    let mut keys = vec![("mcp.method", ext.method.as_str())];
    let param = match ext.method.as_str() {
        "tools/call" | "prompts/get" => Some("name"),
        "resources/read" => Some("uri"),
        _ => None,
    };
    // A Go server folds case, so `{"name":"a","Name":"b"}` can run `b`. No
    // `mcp.name` then, and the policy filter denies the entity method.
    if let (Some(param), Some(p)) = (param, ext.params.as_ref())
        && p.keys().all(|k| k == param || !gojson::is(k, param))
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
            self.submit(&line);
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
            self.submit(&line);
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
        mcp_metadata(&ext)
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
        assert_eq!(mcp_metadata(&ext), [("mcp.method", "$transport/terminate")]);
    }

    #[test]
    fn batch_and_non_json_bodies_are_not_parsed() {
        let batch = br#"[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t"}}]"#;
        assert!(parse_request("POST", "/mcp", &HeaderMap::new(), batch).is_none());
        assert!(parse_request("POST", "/mcp", &HeaderMap::new(), b"not json").is_none());
    }
}

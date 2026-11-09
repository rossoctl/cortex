//! The row builder: `Exchange`, one per request. It turns plain values from the
//! Praxis hooks into the request row and the response row, as direct mode
//! records them (core/listener/internal/sessionevent, forwardproxy).

use std::collections::BTreeMap;
use std::time::{Duration, Instant, SystemTime};

use chrono::{DateTime, SecondsFormat, Utc};
use http::HeaderMap;
use http::header::{CONTENT_ENCODING, CONTENT_LENGTH, HOST, USER_AGENT};

use super::attribution::{Attribution, HOP_HEADERS};
use super::client::{
    CLAUDE_CODE_AGENT_ID_HEADER, CLAUDE_CODE_PARENT_AGENT_ID_HEADER, Client, parse_user_agent,
    subagent_header,
};
use super::line::{Capture, Cost, Event, EventError, Line, ResponseCapture, wire_header};
use super::redact;
use crate::parse::MAX_BODY_SIZE;
use crate::parse::decode::{Coding, DecodeError, Decoder};
use crate::parse::inference::{InferenceObserver, is_event_stream};
use crate::parse::sse::FrameReader;

/// `maxRequestBodySize` in core/listener/forwardproxy.
pub const MAX_REQUEST_BODY: usize = 32 << 20;
/// `model.MaxResponseCapture`.
pub const MAX_RESPONSE_CAPTURE: usize = 10 << 20;

/// The response header that the egress listener sets on a failed dial.
pub const EGRESS_ERROR_HEADER: &str = "X-Cortex-Egress-Error";

/// The response headers that a cost record reads.
const COST_HEADERS: [&str; 6] = [
    "X-Litellm-Response-Cost",
    "X-Litellm-Response-Cost-Original",
    "X-Litellm-Response-Cost-Discount-Amount",
    "X-Litellm-Response-Cost-Margin-Amount",
    "X-Litellm-Model-Name",
    "Content-Type",
];

/// Decision 8 of the plan: Rust text where Go puts its error text.
const CLIENT_GONE: &str = "client closed the connection";
const UPSTREAM_GONE: &str = "upstream closed the connection";

/// Why a response stopped before its end.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Abort {
    /// A buffered response over `MAX_BODY_SIZE` decoded bytes.
    BufferedOverCap,
    /// One SSE frame over `MAX_BODY_SIZE`.
    FrameOverCap,
}

/// Port of `isKnownMislabeledSSE` with its 2xx gate (forwardproxy/server.go).
pub fn is_known_mislabeled_sse(status: u16, host: &str, path: &str) -> bool {
    status / 100 == 2 && host == "chatgpt.com" && path == "/backend-api/codex/responses"
}

/// The error kind for an `X-Cortex-Egress-Error` value.
fn egress_kind(v: &str) -> &'static str {
    match v.trim() {
        "upstream_dns" => "upstream_dns",
        "upstream_tls" => "upstream_tls",
        "upstream_timeout" => "upstream_timeout",
        "upstream_refused" => "upstream_refused",
        _ => "upstream_error",
    }
}

/// The coding to decode for parsing. Only one coding is decoded; any other is read as it is.
fn coding(h: &HeaderMap) -> Coding {
    let mut values = h.get_all(CONTENT_ENCODING).iter();
    let (Some(v), None) = (values.next(), values.next()) else {
        return Coding::Identity;
    };
    match v.to_str().map(str::trim) {
        Ok(c) if c.eq_ignore_ascii_case("gzip") => Coding::Gzip,
        Ok(c) if c.eq_ignore_ascii_case("zstd") => Coding::Zstd,
        _ => Coding::Identity,
    }
}

/// Port of `upstreamErrorKind` (core/pipeline/snapshot_context.go): the provider's
/// `error.type`, else `error.code`, when it is a JSON string or number.
fn upstream_error_kind(body: &[u8]) -> String {
    let body = &body[..body.len().min(64 * 1024)];
    let Ok(v) = serde_json::from_slice::<serde_json::Value>(body) else {
        return String::new();
    };
    let pick = |k: &str| match v.get("error").and_then(|e| e.get(k)) {
        Some(serde_json::Value::String(s)) => s.clone(),
        Some(serde_json::Value::Number(n)) => n.to_string(),
        _ => String::new(),
    };
    let mut t = pick("type");
    if t.is_empty() {
        t = pick("code");
    }
    let mut cut = t.len().min(64);
    while !t.is_char_boundary(cut) {
        cut -= 1;
    }
    t.truncate(cut);
    t
}

/// Appends `b` to the capture body, up to `MAX_RESPONSE_CAPTURE`.
fn keep(capture: &mut Option<ResponseCapture>, b: &[u8]) {
    let Some(c) = capture else {
        return;
    };
    let room = MAX_RESPONSE_CAPTURE - c.body.len();
    if b.len() > room {
        c.truncated = true;
    }
    c.body.extend_from_slice(&b[..b.len().min(room)]);
}

/// The response side of an exchange, from its head on.
struct Response {
    status: u16,
    headers: HeaderMap,
    sse: bool,
    /// The body is gzip alone: the capture and `bytesDown` take the decoded bytes.
    gzip: bool,
    decoder: Decoder,
    frames: FrameReader,
    /// The decoded body of a buffered response.
    buffered: Vec<u8>,
    wire_bytes: usize,
    decoded_bytes: usize,
    capture: Option<ResponseCapture>,
    egress_error: Option<&'static str>,
    /// The body does not decode: parsing stops, the bytes still go through.
    corrupt: bool,
}

impl Response {
    /// Gives a decoded chunk (or the decoder's end, for `None`) to the parser.
    fn feed(
        &mut self,
        obs: &mut Option<InferenceObserver>,
        chunk: Option<&[u8]>,
    ) -> Result<(), Abort> {
        if self.corrupt {
            return Ok(());
        }
        let Response {
            headers,
            sse,
            gzip,
            decoder,
            frames,
            buffered,
            decoded_bytes,
            capture,
            ..
        } = self;
        let mut frame_over = false;
        let mut sink = |b: &[u8]| -> Result<(), DecodeError> {
            *decoded_bytes += b.len();
            if *gzip {
                keep(capture, b);
            }
            if !*sse {
                buffered.extend_from_slice(b);
                return Ok(());
            }
            let (done, over) = match frames.push(b) {
                Ok(f) => (f, false),
                Err(e) => (e.completed, true),
            };
            if let Some(o) = obs.as_mut() {
                for f in done {
                    o.on_response_frame(headers, &f, false);
                }
            }
            if over {
                frame_over = true;
                return Err(DecodeError::CapExceeded);
            }
            Ok(())
        };
        let r = match chunk {
            Some(c) => decoder.push_with(c, &mut sink),
            None => decoder.finish_with(&mut sink),
        };
        match r {
            Ok(()) => Ok(()),
            Err(_) if frame_over => Err(Abort::FrameOverCap),
            Err(DecodeError::CapExceeded) => Err(Abort::BufferedOverCap),
            Err(DecodeError::Corrupt) => {
                self.corrupt = true;
                Ok(())
            }
        }
    }
}

/// One request and its response, from the request head to the last row.
pub struct Exchange {
    method: String,
    host: String,
    path: String,
    query: String,
    headers: HeaderMap,
    attribution: Attribution,
    request_id: String,
    started_at: String,
    start: Instant,
    client: Option<Client>,
    subagent: String,
    parent_subagent: String,
    observer: Option<InferenceObserver>,
    request_built: bool,
    capture_kept: bool,
    ttfb: Duration,
    resp: Option<Response>,
}

impl Exchange {
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        method: &str,
        host: &str,
        path_and_query: &str,
        headers: &HeaderMap,
        attribution: Attribution,
        request_id: String,
        started: SystemTime,
        start: Instant,
    ) -> Exchange {
        let (path, query) = path_and_query
            .split_once('?')
            .unwrap_or((path_and_query, ""));
        let header = |name: &str| headers.get(name).and_then(|v| v.to_str().ok());
        Exchange {
            method: method.to_owned(),
            host: host.to_owned(),
            path: path.to_owned(),
            query: query.to_owned(),
            client: parse_user_agent(header(USER_AGENT.as_str()).unwrap_or("")),
            subagent: subagent_header(header(CLAUDE_CODE_AGENT_ID_HEADER)),
            parent_subagent: subagent_header(header(CLAUDE_CODE_PARENT_AGENT_ID_HEADER)),
            headers: headers.clone(),
            attribution,
            request_id,
            started_at: DateTime::<Utc>::from(started).to_rfc3339_opts(SecondsFormat::Nanos, true),
            start,
            observer: None,
            request_built: false,
            capture_kept: false,
            ttfb: Duration::ZERO,
            resp: None,
        }
    }

    /// The fields that both rows carry.
    fn event(&self, phase: &'static str) -> Event {
        Event {
            direction: "outbound",
            phase,
            request_id: self.request_id.clone(),
            host: self.host.clone(),
            http_method: self.method.clone(),
            http_path: self.path.clone(),
            client: self.client.clone(),
            subagent: self.subagent.clone(),
            parent_subagent: self.parent_subagent.clone(),
            proc: self.attribution.proc.clone(),
            ..Event::default()
        }
    }

    fn line(&self, event: Event, capture: Option<Capture>, cost: Option<Cost>) -> Line {
        Line {
            request_id: self.request_id.clone(),
            started_at: self.started_at.clone(),
            session: self.attribution.record_session.clone(),
            event,
            capture,
            cost,
        }
    }

    /// The request row. Parses the raw body (the parser decodes it itself).
    pub fn on_request_body(&mut self, body: &[u8]) -> Line {
        let target = match self.query.is_empty() {
            true => self.path.clone(),
            false => format!("{}?{}", self.path, self.query),
        };
        self.observer = Some(InferenceObserver::on_request(&target, &self.headers, body));
        self.request_row(body)
    }

    fn request_row(&mut self, body: &[u8]) -> Line {
        self.request_built = true;
        let inference = self.observer.as_ref().and_then(|o| o.extension()).cloned();
        // Only inference is kept: the body of any other request can be a credential.
        let capture = inference.is_some().then(|| {
            let mut h = self.headers.clone();
            h.remove(HOST);
            for n in HOP_HEADERS {
                h.remove(n);
            }
            Capture::Request {
                original: body.to_vec(),
                header: wire_header(&redact::headers(&h)),
                query: redact::query(&self.query),
            }
        });
        self.capture_kept = capture.is_some();
        let event = Event {
            inference,
            bytes_up: body.len() as i64,
            client_proc: self.attribution.client_proc.clone(),
            session_header: self.attribution.session_header.clone(),
            ..self.event("request")
        };
        self.line(event, capture, None)
    }

    /// Reads the response head. Removes `X-Cortex-Egress-Error` from `headers`.
    pub fn on_response_head(&mut self, status: u16, headers: &mut HeaderMap) -> Result<(), Abort> {
        self.ttfb = self.start.elapsed();
        let egress_error = headers
            .remove(EGRESS_ERROR_HEADER)
            .map(|v| egress_kind(v.to_str().unwrap_or("")));
        let sse =
            is_event_stream(headers) || is_known_mislabeled_sse(status, &self.host, &self.path);
        let coding = coding(headers);
        let gzip = coding == Coding::Gzip;
        let capture = self.capture_kept.then(|| {
            let mut h = redact::headers(headers);
            if gzip {
                h.remove(CONTENT_ENCODING);
                h.remove(CONTENT_LENGTH);
            }
            ResponseCapture {
                status,
                header: wire_header(&h),
                decoded: gzip,
                ..ResponseCapture::default()
            }
        });
        let over_by_length = !sse
            && !headers.contains_key(CONTENT_ENCODING)
            && headers
                .get(CONTENT_LENGTH)
                .and_then(|v| v.to_str().ok()?.trim().parse::<u64>().ok())
                .is_some_and(|n| n > MAX_BODY_SIZE as u64);
        self.resp = Some(Response {
            status,
            headers: headers.clone(),
            sse,
            gzip,
            // The buffered cap counts decoded bytes; a stream has only the frame cap.
            decoder: Decoder::new(coding, if sse { usize::MAX } else { MAX_BODY_SIZE }),
            frames: FrameReader::new(MAX_BODY_SIZE),
            buffered: Vec::new(),
            wire_bytes: 0,
            decoded_bytes: 0,
            capture,
            egress_error,
            corrupt: false,
        });
        if over_by_length {
            return Err(Abort::BufferedOverCap);
        }
        Ok(())
    }

    /// Counts, captures and parses one response chunk as it came off the wire.
    pub fn on_response_chunk(&mut self, chunk: &[u8]) -> Result<(), Abort> {
        let Some(r) = self.resp.as_mut() else {
            return Ok(());
        };
        r.wire_bytes += chunk.len();
        if !r.gzip {
            keep(&mut r.capture, chunk);
        }
        r.feed(&mut self.observer, Some(chunk))
    }

    /// The response row of a response read to its end.
    pub fn on_response_end(&mut self) -> Line {
        let Some(r) = self.resp.as_mut() else {
            return self.response_row(0, None);
        };
        if let Err(a) = r.feed(&mut self.observer, None) {
            return self.on_abort(a);
        }
        if r.sse && !r.corrupt {
            let (done, over) = match r.frames.finish() {
                Ok(f) => (f, false),
                Err(e) => (e.completed, true),
            };
            if let Some(o) = self.observer.as_mut() {
                for f in done {
                    o.on_response_frame(&r.headers, &f, false);
                }
            }
            if over {
                return self.on_abort(Abort::FrameOverCap);
            }
        }
        if let Some(o) = self.observer.as_mut() {
            let last: &[u8] = if r.sse { b"" } else { &r.buffered };
            o.on_response_frame(&r.headers, last, true);
        }
        if let Some(c) = r.capture.as_mut() {
            c.ended = true;
        }
        let status = r.status;
        let error = match r.egress_error {
            Some(kind) => Some(EventError {
                kind: kind.to_owned(),
                ..EventError::default()
            }),
            // DeriveError: a status the upstream sent >= 400 is backend_error.
            None if status >= 400 => Some(EventError {
                kind: "backend_error".to_owned(),
                code: status.to_string(),
                message: if r.sse {
                    String::new()
                } else {
                    upstream_error_kind(&r.buffered)
                },
            }),
            None => None,
        };
        self.response_row(status as i64, error)
    }

    /// The response row of a response cut at a cap.
    pub fn on_abort(&mut self, a: Abort) -> Line {
        let status = self.resp.as_ref().map_or(0, |r| r.status);
        let (status_code, message) = match a {
            Abort::BufferedOverCap => (
                502,
                format!("response body over the {MAX_BODY_SIZE}-byte buffer limit"),
            ),
            Abort::FrameOverCap => (
                status as i64,
                format!("single SSE frame over the {MAX_BODY_SIZE}-byte frame cap"),
            ),
        };
        let error = EventError {
            kind: "proxy_error".to_owned(),
            code: status.to_string(),
            message,
        };
        self.response_row(status_code, Some(error))
    }

    /// The rows of an exchange that ended early or was refused before the upstream.
    /// The request row comes first when none was built.
    pub fn deferred(
        &mut self,
        status: u16,
        downstream_gone: bool,
        upstream_gone: bool,
    ) -> Vec<Line> {
        let mut out = Vec::with_capacity(2);
        if !self.request_built {
            out.push(self.request_row(&[]));
        }
        // A failure after the upstream's head carries its status as the code.
        let code = self
            .resp
            .as_ref()
            .map_or(String::new(), |r| r.status.to_string());
        let (status_code, error) = if status == 413 {
            let message = match self
                .headers
                .get(CONTENT_LENGTH)
                .and_then(|v| v.to_str().ok()?.trim().parse::<u64>().ok())
            {
                Some(n) => format!(
                    "request body of {n} bytes is over the {MAX_REQUEST_BODY}-byte buffer limit"
                ),
                None => format!("request body over the {MAX_REQUEST_BODY}-byte buffer limit"),
            };
            (413, ("proxy_error", String::new(), message))
        } else if downstream_gone || status == 499 {
            (499, ("client_canceled", code, CLIENT_GONE.to_owned()))
        } else if upstream_gone {
            (
                status as i64,
                ("upstream_error", code, UPSTREAM_GONE.to_owned()),
            )
        } else {
            (
                status as i64,
                ("gateway_refused", String::new(), String::new()),
            )
        };
        let (kind, code, message) = error;
        let error = EventError {
            kind: kind.to_owned(),
            code,
            message,
        };
        out.push(self.response_row(status_code, Some(error)));
        out
    }

    fn response_row(&mut self, status_code: i64, error: Option<EventError>) -> Line {
        let mut event = Event {
            // Line format v1: the request row carries the request-only fields, and Cortex puts
            // them back on this row. They are most of a line's bytes (docs/session-api.md).
            inference: self.observer.as_ref().and_then(|o| o.extension()).map(|i| {
                let mut i = i.clone();
                i.messages = Vec::new();
                i.tools = Vec::new();
                i.tool_results = Vec::new();
                i.history_tool_calls = Vec::new();
                i.history_thinking = None;
                i
            }),
            status_code,
            error,
            duration_ms: self.start.elapsed().as_millis() as i64,
            ttfb_ms: self.ttfb.as_millis() as i64,
            ..self.event("response")
        };
        let (mut capture, mut cost) = (None, None);
        if let Some(r) = self.resp.as_mut() {
            event.bytes_down = if r.gzip {
                r.decoded_bytes
            } else {
                r.wire_bytes
            } as i64;
            capture = r.capture.take().map(|mut c| {
                c.incomplete = !c.ended;
                Capture::Response { response: c }
            });
            let headers: BTreeMap<String, String> = COST_HEADERS
                .iter()
                .filter_map(|&n| {
                    let v = r.headers.get(n)?;
                    Some((
                        n.to_owned(),
                        String::from_utf8_lossy(v.as_bytes()).into_owned(),
                    ))
                })
                .collect();
            cost = Some(Cost { headers });
        }
        self.line(event, capture, cost)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::record::emitter::MAX_LINE;
    use http::{HeaderName, HeaderValue};
    use serde_json::{Value, json};
    use std::io::Write;

    const FIXTURE: &str = concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../../core/plugins/inferenceparser/testdata/claude-code/main-stream-thinking-tool-use"
    );

    fn header_map<'a>(lines: impl Iterator<Item = &'a str>) -> HeaderMap {
        let mut h = HeaderMap::new();
        for line in lines.take_while(|l| !l.is_empty()) {
            let (name, value) = line.split_once(':').unwrap();
            h.append(
                HeaderName::from_bytes(name.trim().as_bytes()).unwrap(),
                HeaderValue::from_str(value.trim()).unwrap(),
            );
        }
        h
    }

    struct Fixture {
        target: String,
        req_headers: HeaderMap,
        req_body: Vec<u8>,
        resp_headers: HeaderMap,
        resp_body: Vec<u8>,
    }

    fn fixture() -> Fixture {
        let head = std::fs::read_to_string(format!("{FIXTURE}/request.headers")).unwrap();
        let mut lines = head.lines();
        let target = lines.next().unwrap().split(' ').nth(1).unwrap().to_owned();
        let resp = std::fs::read_to_string(format!("{FIXTURE}/response.headers")).unwrap();
        Fixture {
            target,
            req_headers: header_map(lines),
            req_body: std::fs::read(format!("{FIXTURE}/request.json")).unwrap(),
            resp_headers: header_map(resp.lines()),
            resp_body: std::fs::read(format!("{FIXTURE}/response.sse")).unwrap(),
        }
    }

    fn attribution() -> Attribution {
        Attribution {
            proc: "/bin/claude".into(),
            client_proc: "/bin/zsh".into(),
            session_header: "x-claude-code-session-id".into(),
            record_session: "s1".into(),
            ..Attribution::default()
        }
    }

    fn exchange(method: &str, host: &str, target: &str, h: &HeaderMap) -> Exchange {
        Exchange::new(
            method,
            host,
            target,
            h,
            attribution(),
            "req-1".into(),
            SystemTime::now(),
            Instant::now(),
        )
    }

    fn json_of(l: &Line) -> Value {
        serde_json::to_value(l).unwrap()
    }

    fn gzip(b: &[u8]) -> Vec<u8> {
        let mut e = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::default());
        e.write_all(b).unwrap();
        e.finish().unwrap()
    }

    /// Runs the fixture with the response body `wire` and the extra response headers.
    fn run(wire: &[u8], extra: &[(&'static str, &str)], chunk: usize) -> (Line, Line, HeaderMap) {
        let f = fixture();
        let mut ex = exchange("POST", "api.example.com", &f.target, &f.req_headers);
        let req = ex.on_request_body(&f.req_body);
        let mut h = f.resp_headers.clone();
        for (k, v) in extra {
            h.insert(*k, v.parse().unwrap());
        }
        ex.on_response_head(200, &mut h).unwrap();
        for c in wire.chunks(chunk) {
            ex.on_response_chunk(c).unwrap();
        }
        (req, ex.on_response_end(), h)
    }

    fn b64(v: &Value) -> Vec<u8> {
        use base64::Engine;
        base64::engine::general_purpose::STANDARD
            .decode(v.as_str().unwrap())
            .unwrap()
    }

    #[test]
    fn request_row_has_inference_and_capture() {
        let f = fixture();
        let mut ex = exchange("POST", "api.example.com", &f.target, &f.req_headers);
        let l = json_of(&ex.on_request_body(&f.req_body));
        assert_eq!(l["session"], "s1");
        assert_eq!(l["requestId"], "req-1");
        let e = &l["event"];
        assert_eq!(e["phase"], "request");
        assert_eq!(e["direction"], "outbound");
        assert_eq!(e["httpPath"], "/v1/messages");
        assert_eq!(e["httpMethod"], "POST");
        assert_eq!(e["bytesUp"], f.req_body.len());
        assert_eq!(e["client"]["name"], "claude-code");
        assert_eq!(e["proc"], "/bin/claude");
        assert_eq!(e["clientProc"], "/bin/zsh");
        assert_eq!(e["sessionHeader"], "x-claude-code-session-id");
        assert!(e["inference"]["model"].is_string());
        assert_eq!(b64(&l["capture"]["original"]), f.req_body);
        assert_eq!(l["capture"]["query"], "beta=true");
        assert!(l.get("cost").is_none());
        let started = chrono::DateTime::parse_from_rfc3339(l["startedAt"].as_str().unwrap());
        assert!(started.is_ok());
    }

    #[test]
    fn response_row_leaves_out_request_fields() {
        let f = fixture();
        let (req, resp, _) = run(&f.resp_body, &[], 4096);
        let (req, resp) = (json_of(&req), json_of(&resp));
        let (qi, ri) = (&req["event"]["inference"], &resp["event"]["inference"]);
        assert!(qi["messages"].as_array().is_some_and(|m| !m.is_empty()));
        assert!(qi["tools"].as_array().is_some_and(|t| !t.is_empty()));
        for k in [
            "messages",
            "tools",
            "toolResults",
            "historyToolCalls",
            "historyThinking",
        ] {
            assert!(ri.get(k).is_none(), "the response row carries {k}");
        }
        assert_eq!(ri["model"], qi["model"]);
        assert!(ri["outputTokens"].as_i64().unwrap() > 0);
    }

    #[test]
    fn capture_headers_have_no_hop_or_credential_headers() {
        let f = fixture();
        let mut h = f.req_headers.clone();
        for (k, v) in [
            ("authorization", "Bearer secret"),
            ("x-cortex-token", "t"),
            ("x-cortex-agent", "a"),
            ("x-cortex-dest", "d"),
            ("x-cortex-custom", "keep"),
        ] {
            h.insert(k, v.parse().unwrap());
        }
        let mut ex = exchange("POST", "api.example.com", &f.target, &h);
        let l = json_of(&ex.on_request_body(&f.req_body));
        let hdr = l["capture"]["header"].as_object().unwrap();
        for gone in [
            "Authorization",
            "X-Cortex-Token",
            "X-Cortex-Agent",
            "X-Cortex-Dest",
            "Host",
        ] {
            assert!(!hdr.contains_key(gone), "{gone} kept");
        }
        assert_eq!(hdr["X-Cortex-Custom"], json!(["keep"]));
        assert!(hdr.contains_key("User-Agent"));
    }

    #[test]
    fn non_inference_request_has_no_capture() {
        let mut ex = exchange(
            "POST",
            "auth.example.com",
            "/oauth/token",
            &HeaderMap::new(),
        );
        let l = json_of(&ex.on_request_body(b"grant_type=refresh_token&refresh_token=x"));
        assert!(l.get("capture").is_none());
        assert!(l["event"].get("inference").is_none());
        let mut h = HeaderMap::new();
        h.insert("content-type", "application/json".parse().unwrap());
        ex.on_response_head(200, &mut h).unwrap();
        ex.on_response_chunk(br#"{"access_token":"x"}"#).unwrap();
        let r = json_of(&ex.on_response_end());
        assert!(r.get("capture").is_none());
        assert_eq!(r["event"]["bytesDown"], 20);
    }

    #[test]
    fn gzip_response_capture_is_decoded() {
        let f = fixture();
        let (_, plain, _) = run(&f.resp_body, &[], 4096);
        let plain = json_of(&plain);
        let wire = gzip(&f.resp_body);
        for chunk in [wire.len(), 4096] {
            let (_, r, _) = run(
                &wire,
                &[("content-encoding", "gzip"), ("content-length", "99")],
                chunk,
            );
            let r = json_of(&r);
            let c = &r["capture"]["response"];
            assert_eq!(c["decoded"], true);
            assert_eq!(c["ended"], true);
            assert_eq!(c["incomplete"], false);
            assert_eq!(b64(&c["body"]), f.resp_body);
            let hdr = c["header"].as_object().unwrap();
            assert!(!hdr.contains_key("Content-Encoding"));
            assert!(!hdr.contains_key("Content-Length"));
            assert_eq!(r["event"]["bytesDown"], f.resp_body.len());
            assert_eq!(r["event"]["inference"], plain["event"]["inference"]);
            assert!(r["event"]["inference"]["outputTokens"].as_i64().unwrap() > 0);
        }
    }

    #[test]
    fn identity_response_capture_is_raw() {
        let f = fixture();
        let (_, r, _) = run(&f.resp_body, &[], 4096);
        let r = json_of(&r);
        let c = &r["capture"]["response"];
        assert_eq!(c["decoded"], false);
        assert_eq!(c["status"], 200);
        assert_eq!(b64(&c["body"]), f.resp_body);
        assert_eq!(r["event"]["bytesDown"], f.resp_body.len());
        assert_eq!(r["event"]["statusCode"], 200);
        assert!(r["event"].get("error").is_none());
        assert_eq!(
            r["cost"]["headers"]["X-Litellm-Model-Name"],
            "gateway/claude-opus-5-5"
        );
        assert_eq!(
            r["event"]["inference"]["servedModel"],
            "gateway/claude-opus-5-5"
        );
    }

    #[test]
    fn br_response_capture_is_raw() {
        let f = fixture();
        let (_, r, _) = run(&f.resp_body, &[("content-encoding", "br")], 4096);
        let r = json_of(&r);
        let c = &r["capture"]["response"];
        assert_eq!(c["decoded"], false);
        assert_eq!(c["header"]["Content-Encoding"], json!(["br"]));
        assert_eq!(b64(&c["body"]), f.resp_body);
        assert_eq!(r["event"]["bytesDown"], f.resp_body.len());
    }

    fn egress(v: &str) -> (Value, HeaderMap) {
        let f = fixture();
        let mut ex = exchange("POST", "api.example.com", &f.target, &f.req_headers);
        ex.on_request_body(&f.req_body);
        let mut h = HeaderMap::new();
        h.insert("x-cortex-egress-error", v.parse().unwrap());
        h.insert("x-cortex-upstream-error", "origin".parse().unwrap());
        ex.on_response_head(502, &mut h).unwrap();
        ex.on_response_chunk(b"bad gateway").unwrap();
        (json_of(&ex.on_response_end()), h)
    }

    #[test]
    fn egress_error_header_becomes_kind_and_is_removed() {
        for kind in [
            "upstream_dns",
            "upstream_tls",
            "upstream_timeout",
            "upstream_refused",
        ] {
            let (r, h) = egress(kind);
            assert!(!h.contains_key(EGRESS_ERROR_HEADER));
            assert!(h.contains_key("x-cortex-upstream-error"));
            assert_eq!(r["event"]["statusCode"], 502);
            assert_eq!(r["event"]["error"]["kind"], kind);
            let hdr = r["capture"]["response"]["header"].as_object().unwrap();
            assert!(!hdr.contains_key("X-Cortex-Egress-Error"));
        }
    }

    #[test]
    fn unknown_egress_error_is_upstream_error() {
        let (r, _) = egress("something_else");
        assert_eq!(r["event"]["error"]["kind"], "upstream_error");
    }

    #[test]
    fn error_status_is_backend_error_with_provider_type() {
        let f = fixture();
        let mut ex = exchange("POST", "api.example.com", &f.target, &f.req_headers);
        ex.on_request_body(&f.req_body);
        let mut h = HeaderMap::new();
        h.insert("content-type", "application/json".parse().unwrap());
        ex.on_response_head(429, &mut h).unwrap();
        ex.on_response_chunk(
            br#"{"type":"error","error":{"type":"rate_limit_error","message":"q"}}"#,
        )
        .unwrap();
        let r = json_of(&ex.on_response_end());
        assert_eq!(
            r["event"]["error"],
            json!({"kind": "backend_error", "code": "429", "message": "rate_limit_error"})
        );
    }

    fn json_exchange() -> Exchange {
        let f = fixture();
        let mut ex = exchange("POST", "api.example.com", &f.target, &f.req_headers);
        ex.on_request_body(&f.req_body);
        ex
    }

    #[test]
    fn buffered_over_cap_by_content_length_aborts_at_head() {
        let mut ex = json_exchange();
        let mut h = HeaderMap::new();
        h.insert("content-type", "application/json".parse().unwrap());
        h.insert(
            "content-length",
            (MAX_BODY_SIZE + 1).to_string().parse().unwrap(),
        );
        assert_eq!(
            ex.on_response_head(200, &mut h),
            Err(Abort::BufferedOverCap)
        );
        let r = json_of(&ex.on_abort(Abort::BufferedOverCap));
        assert_eq!(r["event"]["statusCode"], 502);
        assert_eq!(
            r["event"]["error"],
            json!({"kind": "proxy_error", "code": "200",
                   "message": "response body over the 10485760-byte buffer limit"})
        );
        assert_eq!(r["capture"]["response"]["incomplete"], true);

        // A coded body's decoded size is not known from its length.
        let mut ex = json_exchange();
        h.insert("content-encoding", "gzip".parse().unwrap());
        assert_eq!(ex.on_response_head(200, &mut h), Ok(()));
        // A stream has no total cap.
        let mut ex = json_exchange();
        h.remove("content-encoding");
        h.insert("content-type", "text/event-stream".parse().unwrap());
        assert_eq!(ex.on_response_head(200, &mut h), Ok(()));
    }

    #[test]
    fn chunked_over_cap_row_is_502_proxy_error() {
        let mut ex = json_exchange();
        let mut h = HeaderMap::new();
        h.insert("content-type", "application/json".parse().unwrap());
        ex.on_response_head(200, &mut h).unwrap();
        let chunk = vec![b' '; 1 << 20];
        let mut got = Ok(());
        for _ in 0..11 {
            got = ex.on_response_chunk(&chunk);
            if got.is_err() {
                break;
            }
        }
        assert_eq!(got, Err(Abort::BufferedOverCap));
        let r = json_of(&ex.on_abort(Abort::BufferedOverCap));
        assert_eq!(r["event"]["statusCode"], 502);
        assert_eq!(r["event"]["error"]["kind"], "proxy_error");
        assert_eq!(r["event"]["error"]["code"], "200");
        let c = &r["capture"]["response"];
        assert_eq!(c["truncated"], true);
        assert_eq!(b64(&c["body"]).len(), MAX_RESPONSE_CAPTURE);
    }

    #[test]
    fn frame_over_cap_row_keeps_upstream_status() {
        let mut ex = json_exchange();
        let mut h = HeaderMap::new();
        h.insert("content-type", "text/event-stream".parse().unwrap());
        ex.on_response_head(200, &mut h).unwrap();
        let mut frame = b"data: ".to_vec();
        frame.resize(MAX_BODY_SIZE + 16, b'a');
        assert_eq!(ex.on_response_chunk(&frame), Err(Abort::FrameOverCap));
        let r = json_of(&ex.on_abort(Abort::FrameOverCap));
        assert_eq!(r["event"]["statusCode"], 200);
        assert_eq!(
            r["event"]["error"],
            json!({"kind": "proxy_error", "code": "200",
                   "message": "single SSE frame over the 10485760-byte frame cap"})
        );
    }

    #[test]
    fn deferred_413_gives_request_and_response_rows() {
        let f = fixture();
        let mut h = f.req_headers.clone();
        h.insert("content-length", "34603008".parse().unwrap());
        let mut ex = exchange("POST", "api.example.com", &f.target, &h);
        let rows: Vec<Value> = ex.deferred(413, false, false).iter().map(json_of).collect();
        assert_eq!(rows.len(), 2);
        assert_eq!(rows[0]["event"]["phase"], "request");
        assert!(rows[0].get("capture").is_none());
        assert_eq!(rows[1]["event"]["phase"], "response");
        assert_eq!(rows[1]["event"]["statusCode"], 413);
        assert_eq!(
            rows[1]["event"]["error"],
            json!({"kind": "proxy_error",
                   "message": "request body of 34603008 bytes is over the 33554432-byte buffer limit"})
        );
        let mut ex = exchange("POST", "api.example.com", &f.target, &f.req_headers);
        let rows = ex.deferred(413, false, false);
        assert_eq!(
            json_of(&rows[1])["event"]["error"]["message"],
            "request body over the 33554432-byte buffer limit"
        );
    }

    fn streaming_exchange() -> Exchange {
        let f = fixture();
        let mut ex = json_exchange();
        let mut h = f.resp_headers.clone();
        ex.on_response_head(200, &mut h).unwrap();
        ex.on_response_chunk(&f.resp_body[..100]).unwrap();
        ex
    }

    #[test]
    fn deferred_499_is_client_canceled() {
        for (status, gone) in [(200, true), (499, false)] {
            let rows = streaming_exchange().deferred(status, gone, false);
            assert_eq!(rows.len(), 1);
            let r = json_of(&rows[0]);
            assert_eq!(r["event"]["statusCode"], 499);
            assert_eq!(r["event"]["error"]["kind"], "client_canceled");
            assert_eq!(r["event"]["error"]["message"], CLIENT_GONE);
            assert_eq!(r["capture"]["response"]["incomplete"], true);
            assert_eq!(r["event"]["bytesDown"], 100);
        }
    }

    #[test]
    fn deferred_upstream_gone_is_upstream_error() {
        let rows = streaming_exchange().deferred(200, false, true);
        let r = json_of(&rows[0]);
        assert_eq!(r["event"]["statusCode"], 200);
        assert_eq!(
            r["event"]["error"],
            json!({"kind": "upstream_error", "code": "200", "message": UPSTREAM_GONE})
        );
    }

    #[test]
    fn deferred_other_is_gateway_refused() {
        let f = fixture();
        let mut ex = exchange("POST", "api.example.com", &f.target, &f.req_headers);
        let rows: Vec<Value> = ex.deferred(400, false, false).iter().map(json_of).collect();
        assert_eq!(rows.len(), 2);
        assert_eq!(rows[1]["event"]["statusCode"], 400);
        assert_eq!(
            rows[1]["event"]["error"],
            json!({"kind": "gateway_refused"})
        );
    }

    #[test]
    fn mislabeled_sse_streams_only_on_2xx() {
        let (host, path) = ("chatgpt.com", "/backend-api/codex/responses");
        assert!(is_known_mislabeled_sse(200, host, path));
        assert!(is_known_mislabeled_sse(204, host, path));
        for s in [101, 301, 401, 405, 429, 500] {
            assert!(!is_known_mislabeled_sse(s, host, path), "{s}");
        }
        assert!(!is_known_mislabeled_sse(200, "api.openai.com", path));
        assert!(!is_known_mislabeled_sse(
            200,
            host,
            "/backend-api/codex/other"
        ));

        for (status, sse) in [(200, true), (405, false)] {
            let mut ex = exchange("POST", host, path, &HeaderMap::new());
            ex.on_request_body(br#"{"model":"gpt-5","input":"hi","stream":true}"#);
            let mut h = HeaderMap::new();
            h.insert("content-type", "application/json".parse().unwrap());
            ex.on_response_head(status, &mut h).unwrap();
            assert_eq!(ex.resp.as_ref().unwrap().sse, sse, "{status}");
        }
    }

    #[test]
    fn line_with_capture_over_cap_has_dropped_row() {
        let text = "a".repeat(30 << 20);
        let body = serde_json::to_vec(&json!({
            "model": "claude-opus-5",
            "messages": [{"role": "user", "content": text}],
        }))
        .unwrap();
        let mut ex = exchange("POST", "api.example.com", "/v1/messages", &HeaderMap::new());
        let q = ex.on_request_body(&body).queued();
        assert!(q.with_capture.as_ref().unwrap().len() > MAX_LINE);
        let row: Value = serde_json::from_slice(&q.row).unwrap();
        assert_eq!(row["capture"], json!({"dropped": true}));
        assert_eq!(row["event"]["inference"]["model"], "claude-opus-5");
    }
}

//! End-to-end tests: the real `cortex-gateway` binary between a test client,
//! a mock origin and a mock ingest endpoint.

use std::io::{Read, Write};
use std::net::{Shutdown, TcpListener, TcpStream};
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex, mpsc};
use std::time::{Duration, Instant};

use base64::Engine;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use serde_json::{Value, json};

const TOKEN: &str = "test-token";
const FIXTURE: &str = concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/../../core/plugins/inferenceparser/testdata/claude-code/main-stream-thinking-tool-use"
);
const TEMPLATE: &str = include_str!("testdata/gateway.yaml.tmpl");
/// The `gateway.yaml` that `cortex -supervise` writes (Go is its source).
const SUPERVISOR_YAML: &str = include_str!(concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/../../cmd/cortex/testdata/gateway.yaml.golden"
));
const ACCESS_LOG: &str = "      - filter: access_log";
const WAIT: Duration = Duration::from_secs(10);
const QUIET: Duration = Duration::from_secs(1);

// ---------------------------------------------------------------------------
// HTTP/1.1 reading, enough for these tests
// ---------------------------------------------------------------------------

/// Reads up to the end of the head. Returns the head and the bytes after it.
fn read_head(s: &mut impl Read) -> Option<(String, Vec<u8>)> {
    let mut buf = Vec::new();
    let mut b = [0u8; 4096];
    loop {
        if let Some(i) = buf.windows(4).position(|w| w == b"\r\n\r\n") {
            let rest = buf.split_off(i + 4);
            return Some((String::from_utf8_lossy(&buf).into_owned(), rest));
        }
        let n = s.read(&mut b).ok()?;
        if n == 0 {
            return None;
        }
        buf.extend_from_slice(&b[..n]);
    }
}

fn header<'a>(head: &'a str, name: &str) -> Option<&'a str> {
    head.lines().skip(1).find_map(|l| {
        let (n, v) = l.split_once(':')?;
        n.trim().eq_ignore_ascii_case(name).then(|| v.trim())
    })
}

/// A body reader over buffered bytes and a stream, chunked or not.
struct Body<'a, S: Read> {
    s: &'a mut S,
    buf: Vec<u8>,
    chunked: bool,
    length: Option<usize>,
    out: Vec<u8>,
    done: bool,
}

impl<'a, S: Read> Body<'a, S> {
    fn new(s: &'a mut S, head: &str, rest: Vec<u8>) -> Self {
        let chunked = header(head, "transfer-encoding").is_some_and(|v| v.contains("chunked"));
        let length = header(head, "content-length").and_then(|v| v.parse().ok());
        Body {
            s,
            buf: rest,
            chunked,
            length,
            out: Vec::new(),
            done: false,
        }
    }

    fn fill(&mut self) -> bool {
        let mut b = [0u8; 65536];
        match self.s.read(&mut b) {
            Ok(0) | Err(_) => false,
            Ok(n) => {
                self.buf.extend_from_slice(&b[..n]);
                true
            }
        }
    }

    /// Moves what can be decoded from `buf` to `out`.
    fn decode(&mut self) {
        if !self.chunked {
            self.out.append(&mut self.buf);
            if self.length.is_some_and(|l| self.out.len() >= l) {
                self.done = true;
            }
            return;
        }
        loop {
            let Some(i) = self.buf.windows(2).position(|w| w == b"\r\n") else {
                return;
            };
            let line = String::from_utf8_lossy(&self.buf[..i]).into_owned();
            let size = usize::from_str_radix(line.split(';').next().unwrap_or("").trim(), 16)
                .expect("chunk size");
            if size == 0 {
                self.done = true;
                return;
            }
            if self.buf.len() < i + 2 + size + 2 {
                return;
            }
            self.out.extend_from_slice(&self.buf[i + 2..i + 2 + size]);
            self.buf.drain(..i + 2 + size + 2);
        }
    }

    /// Reads until `out` holds `n` bytes, the body ends or the stream ends.
    fn read_at_least(&mut self, n: usize) {
        self.decode();
        while !self.done && self.out.len() < n && self.fill() {
            self.decode();
        }
    }

    fn read_to_end(mut self) -> Vec<u8> {
        self.read_at_least(usize::MAX);
        self.out
    }
}

fn status(head: &str) -> u16 {
    head.split_whitespace()
        .nth(1)
        .and_then(|s| s.parse().ok())
        .expect("status")
}

// ---------------------------------------------------------------------------
// The mock origin
// ---------------------------------------------------------------------------

/// A request the origin received.
#[derive(Clone, Debug)]
struct Seen {
    head: String,
    body: Vec<u8>,
}

type Handler = Arc<dyn Fn(&Seen, &mut TcpStream) + Send + Sync>;

struct Origin {
    port: u16,
    seen: mpsc::Receiver<Seen>,
}

fn origin(handler: impl Fn(&Seen, &mut TcpStream) + Send + Sync + 'static) -> Origin {
    let l = TcpListener::bind("127.0.0.1:0").expect("bind origin");
    let port = l.local_addr().expect("addr").port();
    let (tx, seen) = mpsc::channel();
    let handler: Handler = Arc::new(handler);
    std::thread::spawn(move || {
        for s in l.incoming().flatten() {
            let (tx, handler) = (tx.clone(), handler.clone());
            std::thread::spawn(move || serve(s, &tx, &handler));
        }
    });
    Origin { port, seen }
}

fn serve(mut s: TcpStream, tx: &mpsc::Sender<Seen>, handler: &Handler) {
    let mut carry = Vec::new();
    loop {
        let mut r = std::io::Cursor::new(std::mem::take(&mut carry)).chain(&mut s);
        let Some((head, rest)) = read_head(&mut r) else {
            return;
        };
        drop(r);
        let mut body = Body::new(&mut s, &head, rest);
        if body.length.is_none() && !body.chunked {
            body.done = true;
        }
        body.read_at_least(usize::MAX);
        let seen = Seen {
            head,
            body: body.out,
        };
        carry = body.buf;
        let _ = tx.send(seen.clone());
        handler(&seen, &mut s);
    }
}

fn respond(s: &mut TcpStream, status_line: &str, headers: &[(&str, &str)], body: &[u8]) {
    let mut out = format!(
        "HTTP/1.1 {status_line}\r\nContent-Length: {}\r\n",
        body.len()
    );
    for (n, v) in headers {
        out.push_str(&format!("{n}: {v}\r\n"));
    }
    out.push_str("\r\n");
    let _ = s.write_all(out.as_bytes());
    let _ = s.write_all(body);
}

fn write_chunk(s: &mut TcpStream, b: &[u8]) -> std::io::Result<()> {
    s.write_all(format!("{:x}\r\n", b.len()).as_bytes())?;
    s.write_all(b)?;
    s.write_all(b"\r\n")
}

// ---------------------------------------------------------------------------
// The mock ingest endpoint
// ---------------------------------------------------------------------------

struct Ingest {
    port: u16,
    /// A line, or what the mock found wrong, so that the test sees it.
    lines: mpsc::Receiver<Result<Value, String>>,
}

fn ingest() -> Ingest {
    let l = TcpListener::bind("127.0.0.1:0").expect("bind ingest");
    let port = l.local_addr().expect("addr").port();
    let (tx, lines) = mpsc::channel();
    std::thread::spawn(move || {
        for mut s in l.incoming().flatten() {
            let tx = tx.clone();
            std::thread::spawn(move || {
                let Some((head, rest)) = read_head(&mut s) else {
                    return;
                };
                let auth = header(&head, "authorization");
                if auth != Some(format!("Bearer {TOKEN}").as_str()) {
                    let _ = tx.send(Err(format!("ingest Authorization is {auth:?}")));
                    return;
                }
                let mut body = Body::new(&mut s, &head, rest);
                let mut sent = 0;
                loop {
                    let before = body.out.len();
                    body.read_at_least(before + 1);
                    while let Some(i) = body.out[sent..].iter().position(|&b| b == b'\n') {
                        let line = &body.out[sent..sent + i];
                        let _ = tx.send(
                            serde_json::from_slice(line)
                                .map_err(|e| format!("ingest line is not JSON: {e}")),
                        );
                        sent += i + 1;
                    }
                    if body.done || body.out.len() == before {
                        return;
                    }
                }
            });
        }
    });
    Ingest { port, lines }
}

impl Ingest {
    /// Waits for `n` lines.
    fn take(&self, n: usize) -> Vec<Value> {
        let deadline = Instant::now() + WAIT;
        (0..n)
            .map(|i| {
                let left = deadline.saturating_duration_since(Instant::now());
                self.lines
                    .recv_timeout(left)
                    .unwrap_or_else(|_| panic!("line {i} of {n} did not arrive"))
                    .unwrap_or_else(|e| panic!("{e}"))
            })
            .collect()
    }

    /// Every line that arrives within `QUIET`.
    fn quiet(&self) -> Vec<Value> {
        let deadline = Instant::now() + QUIET;
        let mut out = Vec::new();
        while let Ok(v) = self
            .lines
            .recv_timeout(deadline.saturating_duration_since(Instant::now()))
        {
            out.push(v.unwrap_or_else(|e| panic!("{e}")));
        }
        out
    }
}

// ---------------------------------------------------------------------------
// The gateway process
// ---------------------------------------------------------------------------

struct Gateway {
    child: Child,
    port: u16,
    dir: PathBuf,
}

impl Drop for Gateway {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
        if std::thread::panicking() {
            let log = std::fs::read_to_string(self.dir.join("gateway.log")).unwrap_or_default();
            eprintln!("cortex-gateway log:\n{log}");
        }
        let _ = std::fs::remove_dir_all(&self.dir);
    }
}

/// A free port in 20000-29999. The mocks bind port 0, which the OS takes from
/// its ephemeral range (32768 and up on Linux, 49152 and up on macOS), so a
/// concurrent test's mock cannot take the gateway's port after the check.
fn free_port() -> u16 {
    static N: AtomicUsize = AtomicUsize::new(0);
    loop {
        let n = std::process::id() as usize * 37 + N.fetch_add(1, Ordering::Relaxed);
        let port = 20000 + (n % 10000) as u16;
        if TcpListener::bind(("127.0.0.1", port)).is_ok() {
            return port;
        }
    }
}

fn gateway(origin: &Origin, ingest: &Ingest, access_log: bool) -> Gateway {
    gateway_with(origin, ingest, access_log, &[])
}

/// Starts the gateway with `flags` added. Its stdin is a pipe that the
/// returned `Gateway` holds.
fn gateway_with(origin: &Origin, ingest: &Ingest, access_log: bool, flags: &[&str]) -> Gateway {
    start(flags, |token_file, port| {
        TEMPLATE
            .replace("{{LISTEN}}", &port.to_string())
            .replace("{{TOKEN_FILE}}", token_file)
            .replace("{{INGEST}}", &ingest.port.to_string())
            .replace("{{ORIGIN}}", &origin.port.to_string())
            .replace("{{ACCESS_LOG}}", if access_log { ACCESS_LOG } else { "" })
    })
}

/// Starts the gateway on the config that `config(token_file, listen_port)`
/// gives, with `flags` added.
fn start(flags: &[&str], config: impl FnOnce(&str, u16) -> String) -> Gateway {
    static N: AtomicUsize = AtomicUsize::new(0);
    let dir = std::env::temp_dir().join(format!(
        "cortex-gateway-test-{}-{}",
        std::process::id(),
        N.fetch_add(1, Ordering::Relaxed)
    ));
    std::fs::create_dir_all(&dir).expect("temp dir");
    let token_file = dir.join("token");
    std::fs::write(&token_file, format!("{TOKEN}\n")).expect("token file");
    let port = free_port();
    let yaml = config(token_file.to_str().expect("path"), port);
    let config = dir.join("gateway.yaml");
    std::fs::write(&config, yaml).expect("config");
    let log = std::fs::File::create(dir.join("gateway.log")).expect("log");
    let mut child = Command::new(env!("CARGO_BIN_EXE_cortex-gateway"))
        // The supervisor gives the gateway an empty env. Test it the same way.
        .env_clear()
        .arg("-c")
        .arg(&config)
        .args(flags)
        .stdin(Stdio::piped())
        .stdout(Stdio::null())
        .stderr(log)
        .spawn()
        .expect("start cortex-gateway");
    let deadline = Instant::now() + WAIT;
    // A connect alone is not proof: during startup the port accepts and then
    // refuses again. Only the gateway answers a tokenless request with 403.
    while !answers_403(port) {
        if let Ok(Some(st)) = child.try_wait() {
            let log = std::fs::read_to_string(dir.join("gateway.log")).unwrap_or_default();
            panic!("cortex-gateway exited with {st}:\n{log}");
        }
        assert!(Instant::now() < deadline, "cortex-gateway is not ready");
        std::thread::sleep(Duration::from_millis(20));
    }
    Gateway { child, port, dir }
}

fn answers_403(port: u16) -> bool {
    let Ok(mut s) = TcpStream::connect(("127.0.0.1", port)) else {
        return false;
    };
    let _ = s.set_read_timeout(Some(QUIET));
    s.write_all(&head("GET", "/", &[], 0)).is_ok()
        && read_head(&mut s).is_some_and(|(h, _)| h.split_whitespace().nth(1) == Some("403"))
}

impl Gateway {
    fn connect(&self) -> TcpStream {
        let s = TcpStream::connect(("127.0.0.1", self.port)).expect("connect");
        s.set_read_timeout(Some(WAIT)).expect("timeout");
        s
    }
}

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

fn head(method: &str, path: &str, headers: &[(&str, &str)], length: usize) -> Vec<u8> {
    let mut out = format!(
        "{method} {path} HTTP/1.1\r\nHost: api.example.com\r\nConnection: close\r\nContent-Length: {length}\r\n"
    );
    for (n, v) in headers {
        out.push_str(&format!("{n}: {v}\r\n"));
    }
    out.push_str("\r\n");
    out.into_bytes()
}

/// Sends one request and reads the whole response: (head, body).
fn send(
    g: &Gateway,
    method: &str,
    path: &str,
    headers: &[(&str, &str)],
    body: &[u8],
) -> (String, Vec<u8>) {
    let mut s = g.connect();
    s.write_all(&head(method, path, headers, body.len()))
        .expect("write head");
    s.write_all(body).expect("write body");
    let (h, rest) = read_head(&mut s).expect("response head");
    let body = Body::new(&mut s, &h, rest).read_to_end();
    (h, body)
}

fn attribution(v: Value) -> String {
    URL_SAFE_NO_PAD.encode(v.to_string())
}

fn fixture(name: &str) -> Vec<u8> {
    std::fs::read(format!("{FIXTURE}/{name}")).expect("fixture")
}

/// The fixture's request headers, without `Host`, plus the token.
fn fixture_headers() -> Vec<(String, String)> {
    let raw = String::from_utf8(fixture("request.headers")).expect("utf-8");
    let mut out: Vec<(String, String)> = raw
        .lines()
        .skip(1)
        .filter_map(|l| l.split_once(':'))
        .map(|(n, v)| (n.trim().to_owned(), v.trim().to_owned()))
        .filter(|(n, _)| !n.eq_ignore_ascii_case("host"))
        .collect();
    out.push(("X-Cortex-Token".into(), TOKEN.into()));
    out
}

fn as_refs(h: &[(String, String)]) -> Vec<(&str, &str)> {
    h.iter().map(|(n, v)| (n.as_str(), v.as_str())).collect()
}

fn fixture_response_headers() -> Vec<(String, String)> {
    String::from_utf8(fixture("response.headers"))
        .expect("utf-8")
        .lines()
        .filter_map(|l| l.split_once(':'))
        .map(|(n, v)| (n.trim().to_owned(), v.trim().to_owned()))
        .collect()
}

fn response_row(lines: &[Value]) -> &Value {
    lines
        .iter()
        .find(|l| l["event"]["phase"] == "response")
        .expect("a response row")
}

fn ok_origin() -> Origin {
    origin(|_, s| respond(s, "200 OK", &[("Content-Type", "text/plain")], b"ok"))
}

/// Sends half of a declared body and closes the connection.
fn abort_mid_body(g: &Gateway) {
    let mut s = g.connect();
    let mut h = fixture_headers();
    h.push(("Content-Type".into(), "application/json".into()));
    s.write_all(&head("POST", "/v1/messages", &as_refs(&h), 1000))
        .expect("head");
    s.write_all(&[b' '; 500]).expect("half body");
    s.flush().expect("flush");
    std::thread::sleep(Duration::from_millis(200));
    let _ = s.shutdown(Shutdown::Both);
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[test]
fn tokenless_request_gets_403_and_no_line() {
    let (o, i) = (ok_origin(), ingest());
    let g = gateway(&o, &i, true);
    let (h, _) = send(&g, "GET", "/v1/models", &[], b"");
    assert_eq!(status(&h), 403);
    let (h, _) = send(&g, "GET", "/v1/models", &[("X-Cortex-Token", "wrong")], b"");
    assert_eq!(status(&h), 403);
    assert!(
        o.seen.recv_timeout(QUIET).is_err(),
        "the origin saw a request"
    );
    assert_eq!(i.quiet(), Vec::<Value>::new());
}

#[test]
fn probe_request_records_nothing() {
    let (o, i) = (ok_origin(), ingest());
    let g = gateway(&o, &i, true);
    let (h, body) = send(
        &g,
        "GET",
        "/health",
        &[("X-Cortex-Token", TOKEN), ("X-Cortex-Probe", "1")],
        b"",
    );
    assert_eq!(status(&h), 200);
    assert_eq!(body, b"ok");
    let seen = o.seen.recv_timeout(WAIT).expect("the origin saw the probe");
    assert!(header(&seen.head, "x-cortex-probe").is_none());
    assert_eq!(i.quiet(), Vec::<Value>::new());
}

#[test]
fn hop_headers_removed_custom_kept() {
    let (o, i) = (ok_origin(), ingest());
    let g = gateway(&o, &i, true);
    let hop = [
        ("X-Cortex-Agent", "a"),
        ("X-Cortex-Proc", "p"),
        ("X-Cortex-Client-Proc", "cp"),
        ("X-Cortex-Session", "s"),
        ("X-Cortex-Source", "src"),
        ("X-Cortex-Session-Header", "sh"),
        ("X-Cortex-Chain-Session", "cs"),
        ("X-Cortex-Attribution", "e30"),
        ("X-Cortex-Request-Id", "rid-1"),
        ("X-Cortex-Dest", "api.example.com:443"),
        ("X-Cortex-Token", TOKEN),
        ("X-Cortex-Custom", "kept"),
    ];
    let (h, _) = send(&g, "GET", "/v1/models", &hop, b"");
    assert_eq!(status(&h), 200);
    let seen = o
        .seen
        .recv_timeout(WAIT)
        .expect("the origin saw the request");
    let mut cortex: Vec<String> = seen
        .head
        .lines()
        .skip(1)
        .filter_map(|l| l.split_once(':'))
        .map(|(n, _)| n.trim().to_ascii_lowercase())
        .filter(|n| n.starts_with("x-cortex-"))
        .collect();
    cortex.sort();
    // The token is the gateway's secret: no hop header reaches the origin.
    assert_eq!(cortex, ["x-cortex-custom"]);
    assert_eq!(header(&seen.head, "x-cortex-custom"), Some("kept"));
    for l in i.take(2) {
        assert_eq!(
            l["requestId"], "rid-1",
            "the id comes from X-Cortex-Request-Id"
        );
    }
}

#[test]
fn inference_request_gives_request_and_response_lines() {
    let sse = fixture("response.sse");
    let o = origin(move |_, s| {
        let mut h = fixture_response_headers();
        h.push(("X-Litellm-Response-Cost".into(), "0.01".into()));
        respond(s, "200 OK", &as_refs(&h), &sse);
    });
    let i = ingest();
    let g = gateway(&o, &i, true);
    let mut h = fixture_headers();
    h.push((
        "X-Cortex-Attribution".into(),
        attribution(json!({"recordSession": "s1", "proc": "claude"})),
    ));
    let (rh, body) = send(
        &g,
        "POST",
        "/v1/messages?beta=true",
        &as_refs(&h),
        &fixture("request.json"),
    );
    assert_eq!(status(&rh), 200);
    assert_eq!(body, fixture("response.sse"));
    let seen = o
        .seen
        .recv_timeout(WAIT)
        .expect("the origin saw the request");
    assert_eq!(seen.body, fixture("request.json"));

    let lines = i.take(2);
    assert_eq!(lines[0]["event"]["phase"], "request");
    assert_eq!(lines[1]["event"]["phase"], "response");
    assert_eq!(lines[0]["requestId"], lines[1]["requestId"]);
    assert_ne!(lines[0]["requestId"], "");
    for l in &lines {
        assert_eq!(l["session"], "s1");
        assert_eq!(l["event"]["proc"], "claude");
    }
    assert_eq!(lines[0]["event"]["inference"]["model"], "claude-opus-5-5");
    assert!(lines[0]["capture"]["original"].is_string());
    let resp = &lines[1];
    assert_eq!(resp["event"]["statusCode"], 200);
    assert_eq!(resp["event"]["inference"]["completionTokens"], 275);
    assert_eq!(resp["cost"]["headers"]["X-Litellm-Response-Cost"], "0.01");
    assert_eq!(resp["capture"]["response"]["ended"], true);
}

#[test]
fn gzip_sse_streams_as_frames_and_wire_bytes_unchanged() {
    use flate2::Compression;
    use flate2::write::GzEncoder;

    let sse = fixture("response.sse");
    let first_frame = sse.windows(2).position(|w| w == b"\n\n").expect("a frame") + 2;
    let mut enc = GzEncoder::new(Vec::new(), Compression::default());
    enc.write_all(&sse[..first_frame]).expect("gzip");
    enc.flush().expect("sync flush");
    let part1 = std::mem::take(enc.get_mut());
    enc.write_all(&sse[first_frame..]).expect("gzip");
    let part2 = enc.finish().expect("gzip end");
    let wire: Vec<u8> = [part1.clone(), part2.clone()].concat();
    let part1_len = part1.len();

    let (go_tx, go_rx) = mpsc::channel::<()>();
    let go_rx = Mutex::new(go_rx);
    let o = origin(move |_, s| {
        let _ = s.write_all(
            b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Encoding: gzip\r\nTransfer-Encoding: chunked\r\n\r\n",
        );
        let _ = write_chunk(s, &part1);
        let _ = s.flush();
        // The last frame goes only after the client got the first one.
        let _ = go_rx.lock().expect("lock").recv_timeout(WAIT);
        let _ = write_chunk(s, &part2);
        let _ = s.write_all(b"0\r\n\r\n");
    });
    let i = ingest();
    let g = gateway(&o, &i, true);
    let mut s = g.connect();
    let h = fixture_headers();
    let req = fixture("request.json");
    s.write_all(&head(
        "POST",
        "/v1/messages?beta=true",
        &as_refs(&h),
        req.len(),
    ))
    .expect("head");
    s.write_all(&req).expect("body");
    let (rh, rest) = read_head(&mut s).expect("response head");
    assert_eq!(status(&rh), 200);
    assert_eq!(header(&rh, "content-encoding"), Some("gzip"));
    let mut body = Body::new(&mut s, &rh, rest);
    body.read_at_least(part1_len);
    assert!(
        body.out.len() >= part1_len,
        "the first frame did not arrive before the last one was sent"
    );
    go_tx.send(()).expect("go");
    assert_eq!(body.read_to_end(), wire);

    let lines = i.take(2);
    let resp = response_row(&lines);
    assert_eq!(resp["event"]["inference"]["completionTokens"], 275);
    assert_eq!(resp["capture"]["response"]["decoded"], true);
    assert_eq!(resp["event"]["bytesDown"], sse.len());
}

#[test]
fn mid_body_abort_is_client_canceled() {
    let (o, i) = (ok_origin(), ingest());
    let g = gateway(&o, &i, true);
    abort_mid_body(&g);
    let lines = i.take(2);
    assert_eq!(lines[0]["event"]["phase"], "request");
    let resp = response_row(&lines);
    assert_eq!(resp["event"]["statusCode"], 499);
    assert_eq!(resp["event"]["error"]["kind"], "client_canceled");
}

#[test]
fn body_over_limit_is_413_proxy_error() {
    const LEN: usize = 33 << 20;
    let (o, i) = (ok_origin(), ingest());
    let g = gateway(&o, &i, true);
    let mut s = g.connect();
    let h = fixture_headers();
    s.write_all(&head("POST", "/v1/messages", &as_refs(&h), LEN))
        .expect("head");
    let mut w = s.try_clone().expect("clone");
    std::thread::spawn(move || {
        let chunk = vec![b' '; 1 << 20];
        for _ in 0..33 {
            if w.write_all(&chunk).is_err() {
                return;
            }
        }
    });
    let (rh, _) = read_head(&mut s).expect("response head");
    assert_eq!(status(&rh), 413);
    let lines = i.take(2);
    assert_eq!(lines[0]["event"]["phase"], "request");
    let resp = response_row(&lines);
    assert_eq!(resp["event"]["statusCode"], 413);
    assert_eq!(resp["event"]["error"]["kind"], "proxy_error");
    assert_eq!(
        resp["event"]["error"]["message"],
        "request body of 34603008 bytes is over the 33554432-byte buffer limit"
    );
}

#[test]
fn buffered_over_cap_with_content_length_is_502() {
    const LEN: usize = 12_000_000;
    let o = origin(|_, s| {
        let _ = s.write_all(
            format!(
                "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {LEN}\r\n\r\n"
            )
            .as_bytes(),
        );
        let chunk = vec![b' '; 1 << 20];
        let mut left = LEN;
        while left > 0 {
            let n = left.min(chunk.len());
            if s.write_all(&chunk[..n]).is_err() {
                return;
            }
            left -= n;
        }
    });
    let i = ingest();
    let g = gateway(&o, &i, true);
    let (rh, _) = send(
        &g,
        "POST",
        "/v1/messages",
        &as_refs(&fixture_headers()),
        &fixture("request.json"),
    );
    assert_eq!(status(&rh), 502);
    let lines = i.take(2);
    let resp = response_row(&lines);
    assert_eq!(resp["event"]["statusCode"], 502);
    assert_eq!(resp["event"]["error"]["kind"], "proxy_error");
    assert_eq!(resp["event"]["error"]["code"], "200");
    assert_eq!(
        resp["event"]["error"]["message"],
        "response body over the 10485760-byte buffer limit"
    );
}

#[test]
fn buffered_over_cap_after_the_head_keeps_upstream_status() {
    let o = origin(|_, s| {
        let _ = s.write_all(
            b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n",
        );
        let chunk = vec![b' '; 1 << 20];
        for _ in 0..12 {
            if write_chunk(s, &chunk).is_err() {
                return;
            }
        }
        let _ = s.write_all(b"0\r\n\r\n");
    });
    let i = ingest();
    let g = gateway(&o, &i, true);
    let mut s = g.connect();
    let h = fixture_headers();
    let req = fixture("request.json");
    s.write_all(&head("POST", "/v1/messages", &as_refs(&h), req.len()))
        .expect("head");
    s.write_all(&req).expect("body");
    let (rh, rest) = read_head(&mut s).expect("response head");
    assert_eq!(status(&rh), 200);
    let body = Body::new(&mut s, &rh, rest).read_to_end();
    assert!(body.len() < 12 << 20, "the body was not cut");
    let lines = i.take(2);
    let resp = response_row(&lines);
    assert_eq!(
        resp["event"]["statusCode"], 200,
        "the status the client got"
    );
    assert_eq!(resp["event"]["error"]["kind"], "proxy_error");
    assert_eq!(resp["event"]["error"]["code"], "200");
    assert_eq!(
        resp["event"]["error"]["message"],
        "response body over the 10485760-byte buffer limit"
    );
    assert_eq!(i.quiet(), Vec::<Value>::new(), "a second response row");
}

#[test]
fn bodyless_response_ends_its_row() {
    let o = origin(|_, s| {
        let _ = s.write_all(b"HTTP/1.1 204 No Content\r\n\r\n");
    });
    let i = ingest();
    let g = gateway(&o, &i, true);
    let (rh, _) = send(
        &g,
        "POST",
        "/v1/messages",
        &as_refs(&fixture_headers()),
        &fixture("request.json"),
    );
    assert_eq!(status(&rh), 204);
    let lines = i.take(2);
    let resp = response_row(&lines);
    assert_eq!(resp["event"]["statusCode"], 204);
    assert!(resp["event"].get("error").is_none(), "{resp}");
    assert_eq!(resp["capture"]["response"]["ended"], true);
    assert_eq!(i.quiet(), Vec::<Value>::new(), "a second response row");
}

#[test]
fn frame_over_cap_aborts_stream() {
    let o = origin(|_, s| {
        let _ = s.write_all(
            b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n",
        );
        if write_chunk(s, b"data: ").is_err() {
            return;
        }
        let chunk = vec![b'a'; 1 << 20];
        for _ in 0..12 {
            if write_chunk(s, &chunk).is_err() {
                return;
            }
        }
        let _ = write_chunk(s, b"\n\n");
        let _ = s.write_all(b"0\r\n\r\n");
    });
    let i = ingest();
    let g = gateway(&o, &i, true);
    let mut s = g.connect();
    let h = fixture_headers();
    let req = fixture("request.json");
    s.write_all(&head("POST", "/v1/messages", &as_refs(&h), req.len()))
        .expect("head");
    s.write_all(&req).expect("body");
    let (rh, rest) = read_head(&mut s).expect("response head");
    assert_eq!(status(&rh), 200);
    let body = Body::new(&mut s, &rh, rest).read_to_end();
    assert!(body.len() < 12 << 20, "the stream was not cut");
    let lines = i.take(2);
    let resp = response_row(&lines);
    assert_eq!(resp["event"]["statusCode"], 200);
    assert_eq!(resp["event"]["error"]["kind"], "proxy_error");
    assert_eq!(
        resp["event"]["error"]["message"],
        "single SSE frame over the 10485760-byte frame cap"
    );
    assert_eq!(i.quiet(), Vec::<Value>::new(), "a second response row");
}

#[test]
fn egress_error_header_becomes_kind() {
    let o = origin(|_, s| {
        respond(
            s,
            "502 Bad Gateway",
            &[
                ("X-Cortex-Egress-Error", "upstream_dns"),
                ("X-Cortex-Upstream-Error", "no such host"),
            ],
            b"",
        )
    });
    let i = ingest();
    let g = gateway(&o, &i, true);
    let (rh, _) = send(&g, "GET", "/v1/models", &[("X-Cortex-Token", TOKEN)], b"");
    assert_eq!(status(&rh), 502);
    assert!(header(&rh, "x-cortex-egress-error").is_none());
    assert_eq!(header(&rh, "x-cortex-upstream-error"), Some("no such host"));
    let lines = i.take(2);
    let resp = response_row(&lines);
    assert_eq!(resp["event"]["statusCode"], 502);
    assert_eq!(resp["event"]["error"]["kind"], "upstream_dns");
    assert_eq!(i.quiet(), Vec::<Value>::new(), "a second response row");
}

#[test]
fn without_access_log_no_deferred_row() {
    let (o, i) = (ok_origin(), ingest());
    let g = gateway(&o, &i, false);
    abort_mid_body(&g);
    let lines = i.quiet();
    assert!(
        lines.iter().all(|l| l["event"]["phase"] != "response"),
        "a response row without access_log: {lines:?}"
    );
}

/// Praxis 0.8.0 does not tell this case from a client that closes after the
/// response head, so it reads as `client_canceled` (see `emit_deferred_record`).
#[test]
fn upstream_close_mid_body_reads_as_client_canceled() {
    let o = origin(|_, s| {
        let _ = s.write_all(
            b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{\"a\":",
        );
        let _ = s.flush();
        let _ = s.shutdown(Shutdown::Both);
    });
    let i = ingest();
    let g = gateway(&o, &i, true);
    let mut s = g.connect();
    s.write_all(&head("GET", "/v1/models", &[("X-Cortex-Token", TOKEN)], 0))
        .expect("head");
    let mut sink = Vec::new();
    let _ = s.read_to_end(&mut sink);
    let lines = i.take(2);
    let resp = response_row(&lines);
    assert_eq!(resp["event"]["statusCode"], 499);
    assert_eq!(resp["event"]["error"]["kind"], "client_canceled");
}

#[test]
fn client_close_mid_sse_is_client_canceled() {
    let (closed_tx, closed_rx) = mpsc::channel::<()>();
    let closed_rx = Mutex::new(closed_rx);
    let o = origin(move |_, s| {
        let _ = s.write_all(
            b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n",
        );
        let _ = write_chunk(s, b"event: ping\ndata: {\"type\":\"ping\"}\n\n");
        let _ = s.flush();
        let _ = closed_rx.lock().expect("lock").recv_timeout(WAIT);
        // Keep the stream open, as a model still writing its answer does.
        for _ in 0..40 {
            if write_chunk(s, b"event: ping\ndata: {\"type\":\"ping\"}\n\n").is_err() {
                return;
            }
            std::thread::sleep(Duration::from_millis(50));
        }
    });
    let i = ingest();
    let g = gateway(&o, &i, true);
    let mut s = g.connect();
    let h = fixture_headers();
    let req = fixture("request.json");
    s.write_all(&head("POST", "/v1/messages", &as_refs(&h), req.len()))
        .expect("head");
    s.write_all(&req).expect("body");
    let (rh, rest) = read_head(&mut s).expect("response head");
    assert_eq!(status(&rh), 200);
    Body::new(&mut s, &rh, rest).read_at_least(1);
    let _ = s.shutdown(Shutdown::Both);
    drop(s);
    closed_tx.send(()).expect("closed");
    let lines = i.take(2);
    let resp = response_row(&lines);
    assert_eq!(resp["event"]["statusCode"], 499, "{resp}");
    assert_eq!(resp["event"]["error"]["kind"], "client_canceled");
}

#[test]
fn exits_on_stdin_eof() {
    let (o, i) = (ok_origin(), ingest());
    let mut g = gateway_with(&o, &i, true, &["--exit-on-stdin-eof"]);
    drop(g.child.stdin.take());
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        if let Some(st) = g.child.try_wait().expect("try_wait") {
            assert!(st.success(), "cortex-gateway exited with {st}");
            return;
        }
        assert!(
            Instant::now() < deadline,
            "cortex-gateway still runs 5s after stdin closed"
        );
        std::thread::sleep(Duration::from_millis(20));
    }
}

#[test]
fn keeps_running_without_flag() {
    let (o, i) = (ok_origin(), ingest());
    let mut g = gateway(&o, &i, true);
    drop(g.child.stdin.take());
    std::thread::sleep(QUIET);
    assert!(
        g.child.try_wait().expect("try_wait").is_none(),
        "cortex-gateway exited without --exit-on-stdin-eof"
    );
}

#[test]
fn supervisor_gateway_yaml_serves() {
    let (o, i) = (ok_origin(), ingest());
    let g = start(&[], |token_file, port| {
        let yaml = SUPERVISOR_YAML
            .replace("/CORTEX_DIR/gateway.token", token_file)
            .replace("127.0.0.1:47605", &format!("127.0.0.1:{port}"))
            .replace("127.0.0.1:47606", &format!("127.0.0.1:{}", o.port))
            .replace("127.0.0.1:47601", &format!("127.0.0.1:{}", i.port));
        assert!(
            !yaml.contains("CORTEX_DIR") && !yaml.contains(":4760"),
            "{yaml}"
        );
        yaml
    });
    let (h, body) = send(&g, "GET", "/v1/models", &[("X-Cortex-Token", TOKEN)], b"");
    assert_eq!(status(&h), 200);
    assert_eq!(body, b"ok");
    o.seen
        .recv_timeout(WAIT)
        .expect("the origin saw the request");
    let lines = i.take(2);
    assert_eq!(lines[0]["event"]["phase"], "request");
    assert_eq!(lines[1]["event"]["phase"], "response");
}

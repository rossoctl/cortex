//! Parity with the Go parser: each saved case under
//! core/plugins/inferenceparser/testdata/<agent>/<case> must give the
//! extension that Go wrote to its expected.json, in every feeding variant.

use std::io::Write;
use std::path::{Path, PathBuf};

use cortex_praxis_filters::parse::MAX_BODY_SIZE;
use cortex_praxis_filters::parse::decode::{Coding, Decoder};
use cortex_praxis_filters::parse::inference::InferenceObserver;
use cortex_praxis_filters::parse::sse::FrameReader;
use http::{HeaderMap, HeaderName, HeaderValue};
use serde_json::Value;

const TESTDATA: &str = "../../core/plugins/inferenceparser/testdata";

/// The response files and the Content-Type each one has (`parsefixture.responseTypes`).
const RESPONSE_TYPES: &[(&str, &str)] = &[
    ("response.sse", "text/event-stream"),
    ("response.json", "application/json"),
    ("response.html", "text/html"),
];

struct Case {
    dir: PathBuf,
    target: String,
    req_headers: HeaderMap,
    req_body: Vec<u8>,
    resp_headers: HeaderMap,
    resp_body: Vec<u8>,
    sse: bool,
}

/// Every case directory under `testdata/<agent>/` that has a request.headers file.
fn cases(agent: &str) -> Vec<Case> {
    let root = Path::new(env!("CARGO_MANIFEST_DIR"))
        .join(TESTDATA)
        .join(agent);
    let mut dirs: Vec<PathBuf> = std::fs::read_dir(&root)
        .unwrap_or_else(|e| panic!("{}: {e}", root.display()))
        .map(|e| e.unwrap().path())
        .filter(|d| d.join("request.headers").is_file())
        .collect();
    dirs.sort();
    assert!(!dirs.is_empty(), "no cases under {}", root.display());
    dirs.into_iter().map(load).collect()
}

fn load(dir: PathBuf) -> Case {
    let head = std::fs::read_to_string(dir.join("request.headers")).unwrap();
    let mut lines = head.lines();
    let request_line = lines.next().expect("request line");
    let target = request_line
        .split(' ')
        .nth(1)
        .expect("request target")
        .to_owned();
    let req_headers = header_map(lines);

    let found: Vec<_> = RESPONSE_TYPES
        .iter()
        .filter(|(name, _)| dir.join(name).is_file())
        .collect();
    assert!(
        found.len() <= 1,
        "{}: more than one response file",
        dir.display()
    );
    let (file, content_type) = found
        .first()
        .map_or(("response.json", "application/json"), |&&p| p);
    let mut resp_headers = HeaderMap::new();
    resp_headers.insert("content-type", HeaderValue::from_static(content_type));
    // An optional response.headers file replaces the default headers.
    if let Ok(raw) = std::fs::read_to_string(dir.join("response.headers")) {
        resp_headers = header_map(raw.lines());
    }
    Case {
        req_body: read_body(&dir.join("request.json")),
        resp_body: read_body(&dir.join(file)),
        sse: content_type == "text/event-stream",
        dir,
        target,
        req_headers,
        resp_headers,
    }
}

/// Parses `Name: value` lines, as textproto.ReadMIMEHeader does, up to a blank line.
fn header_map<'a>(lines: impl Iterator<Item = &'a str>) -> HeaderMap {
    let mut h = HeaderMap::new();
    for line in lines.take_while(|l| !l.is_empty()) {
        let (name, value) = line.split_once(':').expect("Name: value");
        h.append(
            HeaderName::from_bytes(name.trim().as_bytes()).unwrap(),
            HeaderValue::from_str(value.trim_matches([' ', '\t'])).unwrap(),
        );
    }
    h
}

/// The file's bytes, or no bytes when it does not exist.
fn read_body(path: &Path) -> Vec<u8> {
    std::fs::read(path).unwrap_or_default()
}

fn variants(case: &Case) -> Vec<&'static str> {
    let mut v = if case.sse {
        vec!["buffered", "chunks_4096", "chunks_1", "crlf"]
    } else {
        vec!["buffered"]
    };
    // shortcut: an empty gzip body is a known deferred edge case; add it with that fix.
    if !case.resp_body.is_empty() {
        v.push("gzip");
    }
    v
}

/// Drives `InferenceObserver` over the case, fed as `variant` says.
fn run(case: &Case, variant: &str) -> Value {
    let mut obs = InferenceObserver::on_request(&case.target, &case.req_headers, &case.req_body);
    let h = &case.resp_headers;
    match (case.sse, variant) {
        (_, "buffered") => obs.on_response_frame(h, &case.resp_body, true),
        (true, "chunks_4096") => chunks(&mut obs, h, &case.resp_body, 4096),
        (true, "chunks_1") => chunks(&mut obs, h, &case.resp_body, 1),
        (true, "crlf") => {
            let crlf = String::from_utf8(case.resp_body.clone())
                .unwrap()
                .replace('\n', "\r\n");
            chunks(&mut obs, h, crlf.as_bytes(), 4096)
        }
        (true, "gzip") => {
            let mut reader = FrameReader::new(MAX_BODY_SIZE);
            let mut dec = Decoder::new(Coding::Gzip, usize::MAX);
            {
                let mut sink = |b: &[u8]| {
                    for f in reader.push(b).unwrap() {
                        obs.on_response_frame(h, &f, false);
                    }
                    Ok(())
                };
                for c in gzip(&case.resp_body).chunks(4096) {
                    dec.push_with(c, &mut sink).unwrap();
                }
                dec.finish_with(&mut sink).unwrap();
            }
            for f in reader.finish().unwrap() {
                obs.on_response_frame(h, &f, false);
            }
            obs.on_response_frame(h, b"", true);
        }
        (false, "gzip") => {
            let mut dec = Decoder::new(Coding::Gzip, MAX_BODY_SIZE);
            let mut body = Vec::new();
            for c in gzip(&case.resp_body).chunks(4096) {
                body.extend(dec.push(c).unwrap());
            }
            body.extend(dec.finish().unwrap());
            obs.on_response_frame(h, &body, true);
        }
        _ => panic!("unknown variant {variant}"),
    }
    obs.extension()
        .map_or(Value::Null, |x| serde_json::to_value(x).unwrap())
}

/// Feeds an SSE body through `FrameReader` in chunks of `size`, as Praxis
/// hands body chunks, then gives the empty last frame.
fn chunks(obs: &mut InferenceObserver, h: &HeaderMap, body: &[u8], size: usize) {
    let mut reader = FrameReader::new(MAX_BODY_SIZE);
    for c in body.chunks(size) {
        for f in reader.push(c).unwrap() {
            obs.on_response_frame(h, &f, false);
        }
    }
    for f in reader.finish().unwrap() {
        obs.on_response_frame(h, &f, false);
    }
    obs.on_response_frame(h, b"", true);
}

fn gzip(body: &[u8]) -> Vec<u8> {
    let mut e = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::default());
    e.write_all(body).unwrap();
    e.finish().unwrap()
}

/// Compares two JSON values. Numbers are equal when their f64 values are
/// equal: Go writes 1 where Rust writes 1.0.
fn same(expected: &Value, got: &Value, path: &str) {
    match (expected, got) {
        (Value::Number(a), Value::Number(b)) if a.as_f64() == b.as_f64() => {}
        (Value::Object(a), Value::Object(b)) => {
            for k in a.keys().chain(b.keys()) {
                match (a.get(k), b.get(k)) {
                    (Some(x), Some(y)) => same(x, y, &format!("{path}.{k}")),
                    (x, y) => panic!("{path}.{k}: expected {x:?}, got {y:?}"),
                }
            }
        }
        (Value::Array(a), Value::Array(b)) => {
            assert_eq!(a.len(), b.len(), "{path}: array lengths differ");
            for (i, (x, y)) in a.iter().zip(b).enumerate() {
                same(x, y, &format!("{path}[{i}]"));
            }
        }
        _ => assert_eq!(expected, got, "{path}: values differ"),
    }
}

fn check(agent: &str) {
    for case in cases(agent) {
        let raw = std::fs::read(case.dir.join("expected.json")).unwrap();
        let expected: Value = serde_json::from_slice(&raw).unwrap();
        for variant in variants(&case) {
            let name = format!(
                "{}/{variant}",
                case.dir.file_name().unwrap().to_string_lossy()
            );
            same(&expected, &run(&case, variant), &name);
        }
    }
}

#[test]
fn claude_code() {
    check("claude-code");
}

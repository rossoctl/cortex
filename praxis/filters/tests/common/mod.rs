//! The fixture loader and the JSON comparison that the parity tests share.
#![allow(dead_code)] // each test crate uses only part of this module

use std::path::{Path, PathBuf};

use http::{HeaderMap, HeaderName, HeaderValue};
use serde_json::Value;

pub const TESTDATA: &str = "../../core/plugins/inferenceparser/testdata";

/// The response files and the Content-Type each one has (`parsefixture.responseTypes`).
pub const RESPONSE_TYPES: &[(&str, &str)] = &[
    ("response.sse", "text/event-stream"),
    ("response.json", "application/json"),
    ("response.html", "text/html"),
];

pub struct Case {
    pub dir: PathBuf,
    pub method: String,
    pub target: String,
    pub req_headers: HeaderMap,
    pub req_body: Vec<u8>,
    pub resp_headers: HeaderMap,
    pub resp_body: Vec<u8>,
    pub sse: bool,
}

/// Every case directory under `testdata/<agent>/` that has a request.headers file.
pub fn cases(agent: &str) -> Vec<Case> {
    let root = Path::new(env!("CARGO_MANIFEST_DIR"))
        .join(TESTDATA)
        .join(agent);
    case_dirs(&root).into_iter().map(load).collect()
}

/// Every subdirectory of `root` that has a request.headers file, sorted.
pub fn case_dirs(root: &Path) -> Vec<PathBuf> {
    let mut dirs: Vec<PathBuf> = std::fs::read_dir(root)
        .unwrap_or_else(|e| panic!("{}: {e}", root.display()))
        .map(|e| e.unwrap().path())
        .filter(|d| d.join("request.headers").is_file())
        .collect();
    dirs.sort();
    assert!(!dirs.is_empty(), "no cases under {}", root.display());
    dirs
}

pub fn load(dir: PathBuf) -> Case {
    let head = std::fs::read_to_string(dir.join("request.headers")).unwrap();
    let mut lines = head.lines();
    let request_line = lines.next().expect("request line");
    let mut parts = request_line.split(' ');
    let method = parts.next().expect("method").to_owned();
    let target = parts.next().expect("request target").to_owned();
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
        method,
        target,
        req_headers,
        resp_headers,
    }
}

/// Parses `Name: value` lines, as textproto.ReadMIMEHeader does, up to a blank line.
pub fn header_map<'a>(lines: impl Iterator<Item = &'a str>) -> HeaderMap {
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
pub fn read_body(path: &Path) -> Vec<u8> {
    std::fs::read(path).unwrap_or_default()
}

/// Compares two JSON values. Numbers are equal when their f64 values are
/// equal: Go writes 1 where Rust writes 1.0.
pub fn same(expected: &Value, got: &Value, path: &str) {
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

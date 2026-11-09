//! Parity with the Go parser: each saved case under
//! core/plugins/inferenceparser/testdata/<agent>/<case> must give the
//! extension that Go wrote to its expected.json, in every feeding variant.

mod common;

use std::io::Write;
use std::path::Path;

use common::{Case, TESTDATA, cases, same};

use cortex_praxis_filters::parse::MAX_BODY_SIZE;
use cortex_praxis_filters::parse::decode::{Coding, Decoder};
use cortex_praxis_filters::parse::inference::InferenceObserver;
use cortex_praxis_filters::parse::sse::FrameReader;
use http::HeaderMap;
use serde_json::Value;

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

/// Checks every case under `testdata/<agent>/` whose directory name starts with `prefix`.
fn check(agent: &str, prefix: &str) {
    let cases: Vec<_> = cases(agent)
        .into_iter()
        .filter(|c| {
            c.dir
                .file_name()
                .unwrap()
                .to_string_lossy()
                .starts_with(prefix)
        })
        .collect();
    assert!(!cases.is_empty(), "no {agent}/{prefix}* cases");
    for case in cases {
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

/// The (agent, name prefix) pair that each parity test below checks.
const CLAUDE_CODE: (&str, &str) = ("claude-code", "");
const SYNTHETIC_OPENAI_CHAT: (&str, &str) = ("synthetic", "openai-chat-");
const SYNTHETIC_RESPONSES: (&str, &str) = ("synthetic", "responses-");

#[test]
fn claude_code() {
    check(CLAUDE_CODE.0, CLAUDE_CODE.1);
}

#[test]
fn synthetic_openai_chat() {
    check(SYNTHETIC_OPENAI_CHAT.0, SYNTHETIC_OPENAI_CHAT.1);
}

#[test]
fn synthetic_responses() {
    check(SYNTHETIC_RESPONSES.0, SYNTHETIC_RESPONSES.1);
}

/// Fails when a directory under `testdata/*/*` is not checked by a parity test
/// above. A new agent directory or name prefix needs a new test.
#[test]
fn every_case_dir_is_run() {
    let suites = [CLAUDE_CODE, SYNTHETIC_OPENAI_CHAT, SYNTHETIC_RESPONSES];
    let root = Path::new(env!("CARGO_MANIFEST_DIR")).join(TESTDATA);
    let mut missed = Vec::new();
    for agent in std::fs::read_dir(&root).unwrap() {
        let agent = agent.unwrap().path();
        if !agent.is_dir() {
            continue;
        }
        let a = agent.file_name().unwrap().to_string_lossy().into_owned();
        for dir in std::fs::read_dir(&agent).unwrap() {
            let dir = dir.unwrap().path();
            if !dir.is_dir() {
                continue;
            }
            let name = dir.file_name().unwrap().to_string_lossy().into_owned();
            if !suites.iter().any(|&(s, p)| s == a && name.starts_with(p)) {
                missed.push(format!("{a}/{name}"));
            }
        }
    }
    assert!(missed.is_empty(), "no parity test runs: {missed:?}");
}

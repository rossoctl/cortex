//! Parity with the Go parser: each case under core/plugins/a2aparser/testdata/<case>
//! must give the extension that Go wrote to its expected.json, in every feeding variant.

mod common;

use std::path::Path;

use common::{Case, case_dirs, load, same};

use cortex_praxis_filters::parse::MAX_BODY_SIZE;
use cortex_praxis_filters::parse::a2a::A2aObserver;
use cortex_praxis_filters::parse::sse::FrameReader;
use serde_json::Value;

const TESTDATA: &str = "../../core/plugins/a2aparser/testdata";

/// Drives `A2aObserver` over the case, fed as `variant` says.
fn run(case: &Case, variant: &str) -> Value {
    let mut obs = A2aObserver::on_request(&case.req_headers, &case.req_body);
    match variant {
        "buffered" => obs.on_response_frame(&case.resp_body, true),
        "chunks_4096" => chunks(&mut obs, &case.resp_body, 4096),
        "chunks_1" => chunks(&mut obs, &case.resp_body, 1),
        "crlf" => {
            let crlf = String::from_utf8(case.resp_body.clone())
                .unwrap()
                .replace('\n', "\r\n");
            chunks(&mut obs, crlf.as_bytes(), 4096)
        }
        _ => panic!("unknown variant {variant}"),
    }
    obs.extension()
        .map_or(Value::Null, |x| serde_json::to_value(x).unwrap())
}

/// Feeds an SSE body through `FrameReader` in chunks of `size`, then gives the empty last frame.
fn chunks(obs: &mut A2aObserver, body: &[u8], size: usize) {
    let mut reader = FrameReader::new(MAX_BODY_SIZE);
    for c in body.chunks(size) {
        for f in reader.push(c).unwrap() {
            obs.on_response_frame(&f, false);
        }
    }
    for f in reader.finish().unwrap() {
        obs.on_response_frame(&f, false);
    }
    obs.on_response_frame(b"", true);
}

#[test]
fn a2a_parity() {
    let root = Path::new(env!("CARGO_MANIFEST_DIR")).join(TESTDATA);
    let dirs = case_dirs(&root);
    assert!(dirs.len() >= 13, "only {} cases", dirs.len());
    for case in dirs.into_iter().map(load) {
        let raw = std::fs::read(case.dir.join("expected.json")).unwrap();
        let expected: Value = serde_json::from_slice(&raw).unwrap();
        let variants: &[&str] = if case.sse {
            &["buffered", "chunks_4096", "chunks_1", "crlf"]
        } else {
            &["buffered"]
        };
        for variant in variants {
            let name = format!(
                "{}/{variant}",
                case.dir.file_name().unwrap().to_string_lossy()
            );
            same(&expected, &run(&case, variant), &name);
        }
    }
}

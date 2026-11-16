//! Row parity with direct mode: core/sessionapi/testdata/ingest/replay.ndjson holds
//! the rows that a direct-mode forward proxy recorded for each claude-code fixture
//! (TestIngestReplay_MatchesLiveRun in core/listener/forwardproxy writes it). The
//! Rust row builder must give the same event and capture for the same exchange.

mod common;

use std::path::Path;
use std::time::{Instant, SystemTime};

use common::{cases, same};
use cortex_praxis_filters::record::attribution::Attribution;
use cortex_praxis_filters::record::line::NOT_SET_IN_PRAXIS_MODE;
use cortex_praxis_filters::record::row::Exchange;
use http::HeaderValue;
use serde_json::Value;

const REPLAY: &str = "../../core/sessionapi/testdata/ingest/replay.ndjson";
/// The session header value that the live run sent with every request.
const SESSION: &str = "00000000-0000-4000-8000-000000000000";

/// The event keys left out of the comparison: timings and the request id differ
/// per run, ingestion owns `plugins`, and praxis mode does not set the rest.
fn skipped() -> Vec<&'static str> {
    // Direct mode's Go plugins record these; this run has no policy.
    let mut k = vec![
        "durationMs",
        "ttfbMs",
        "requestId",
        "plugins",
        "invocations",
    ];
    k.extend_from_slice(NOT_SET_IN_PRAXIS_MODE);
    k
}

fn text(v: &Value) -> String {
    v.as_str().unwrap_or_default().to_owned()
}

/// Compares the event and the capture of the Rust line with the Go line.
fn check(got: &Value, want: &Value, name: &str) {
    let (mut got, mut want) = (got.clone(), want.clone());
    for l in [&mut got, &mut want] {
        let ev = l["event"].as_object_mut().unwrap();
        for k in skipped() {
            ev.remove(k);
        }
        // Known difference: Go captures the agent's request headers at entry
        // (spec Section 1, "Captures", [F 2]), with the client's own headers.
        if let Some(c) = l.get_mut("capture").and_then(Value::as_object_mut) {
            c.remove("header");
        }
    }
    same(&want["event"], &got["event"], &format!("{name}.event"));
    same(
        &want["capture"],
        &got["capture"],
        &format!("{name}.capture"),
    );
}

#[test]
fn row_parity_matches_replay() {
    let path = Path::new(env!("CARGO_MANIFEST_DIR")).join(REPLAY);
    let raw = std::fs::read_to_string(&path).unwrap();
    let lines: Vec<Value> = raw
        .lines()
        .map(|l| serde_json::from_str(l).unwrap())
        .collect();
    let cases = cases("claude-code");
    assert_eq!(
        lines.len(),
        2 * cases.len(),
        "one request and one response row per case"
    );

    for (case, pair) in cases.iter().zip(lines.chunks(2)) {
        let name = case.dir.file_name().unwrap().to_string_lossy().into_owned();
        let (want_req, want_resp) = (&pair[0], &pair[1]);
        let ev = &want_req["event"];
        // What the hop carries: the proxy's attribution of the caller.
        let attribution = Attribution {
            proc: text(&ev["proc"]),
            client_proc: text(&ev["clientProc"]),
            session_header: text(&ev["sessionHeader"]),
            record_session: text(&want_req["session"]),
            ..Attribution::default()
        };
        let mut headers = case.req_headers.clone();
        headers.insert(
            "x-claude-code-session-id",
            HeaderValue::from_static(SESSION),
        );
        let host = headers["host"].to_str().unwrap().to_owned();
        let mut ex = Exchange::new(
            &case.method,
            &host,
            &case.target,
            &headers,
            attribution,
            text(&want_req["requestId"]),
            SystemTime::now(),
            Instant::now(),
        );
        let got = serde_json::to_value(ex.on_request_body(&case.req_body)).unwrap();
        check(&got, want_req, &format!("{name}/request"));

        // The live run's origin: the fixture's head, its length and the cost header it sent.
        let mut h = case.resp_headers.clone();
        h.insert("content-length", case.resp_body.len().into());
        if let Some(c) = want_resp["cost"]["headers"]["X-Litellm-Response-Cost"].as_str() {
            h.insert("x-litellm-response-cost", c.parse().unwrap());
        }
        let status = if name == "error-502-html" { 502 } else { 200 };
        ex.on_response_head(status, &mut h).unwrap();
        for c in case.resp_body.chunks(4096) {
            ex.on_response_chunk(c).unwrap();
        }
        let got = serde_json::to_value(ex.on_response_end()).unwrap();
        check(&got, want_resp, &format!("{name}/response"));
    }
}

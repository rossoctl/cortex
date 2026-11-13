//! Wire types of an ingest line: `Line`, `Event`, `Capture`, `Cost`.
//!
//! The JSON is line format v1 (`ingestLine` in core/sessionapi/ingest.go). `Event`
//! is the session event wire JSON (`sessionEventWire` in core/model/session.go)
//! with Go's omit rules.

use std::collections::BTreeMap;

use base64::Engine;
use base64::engine::general_purpose::STANDARD;
use serde::{Serialize, Serializer};

use super::client::Client;
use super::emitter::Queued;
use crate::parse::model::{A2aExtension, InferenceExtension, McpExtension};

/// The line format version, `ingestLineVersion` in Go.
pub const LINE_VERSION: u8 = 1;
/// `sessionapi.SchemaVersion`.
pub const SCHEMA_VERSION: u8 = 1;

/// The session event fields that praxis mode does not set, with the reason for each.
pub const NOT_SET_IN_PRAXIS_MODE: &[&str] = &[
    // The store sets these three when it appends the row.
    "sessionId",
    "seq",
    "at",
    // Ingestion sets the cost record; no other plugin runs.
    "plugins",
    // No workload identity on a laptop.
    "identity",
    // The TLS bridge state is in Cortex; #1721 decides if the hop carries it.
    "tls",
    // No plugins run in the gateway.
    "invocations",
    // No router in the gateway.
    "requestedHost",
    // Cortex strips upgrades before the hop.
    "upgradeStripped",
    // Cortex's own tunnel rows.
    "tunnel",
    "tunnelReason",
    "dialAddr",
    // The Go listener sets these two from its pipeline context. The gateway does not.
    "subagentKind",
    "project",
];

fn is_zero(v: &i64) -> bool {
    *v == 0
}

fn b64<S: Serializer>(b: &[u8], s: S) -> Result<S::Ok, S::Error> {
    s.serialize_str(&STANDARD.encode(b))
}

/// Header names in Go's canonical form, each with its values.
pub type WireHeader = BTreeMap<String, Vec<String>>;

/// `EventError` in core/model/session.go.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct EventError {
    pub kind: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub code: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub message: String,
}

/// One session event, as `sessionEventWire` writes it.
#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Event {
    pub direction: &'static str,
    pub phase: &'static str,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub request_id: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub a2a: Option<A2aExtension>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub mcp: Option<McpExtension>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub inference: Option<InferenceExtension>,
    #[serde(skip_serializing_if = "is_zero")]
    pub status_code: i64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<EventError>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub host: String,
    #[serde(skip_serializing_if = "is_zero")]
    pub duration_ms: i64,
    #[serde(skip_serializing_if = "is_zero")]
    pub ttfb_ms: i64,
    #[serde(skip_serializing_if = "is_zero")]
    pub bytes_up: i64,
    #[serde(skip_serializing_if = "is_zero")]
    pub bytes_down: i64,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub http_method: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub http_path: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub client: Option<Client>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub subagent: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub parent_subagent: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub proc: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub client_proc: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub session_header: String,
}

/// The response half of a capture (`ingestResponse` in Go).
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct ResponseCapture {
    pub status: u16,
    pub header: WireHeader,
    #[serde(serialize_with = "b64")]
    pub body: Vec<u8>,
    pub decoded: bool,
    pub truncated: bool,
    pub ended: bool,
    pub incomplete: bool,
}

/// A line's capture (`ingestCapture` in Go).
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(untagged)]
pub enum Capture {
    Request {
        #[serde(serialize_with = "b64")]
        original: Vec<u8>,
        header: WireHeader,
        query: String,
    },
    Response {
        response: ResponseCapture,
    },
    /// The capture did not fit; the row went without it.
    Dropped {
        dropped: bool,
    },
}

/// A response row's cost-relevant headers.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct Cost {
    pub headers: BTreeMap<String, String>,
}

/// One ingest line.
#[derive(Debug, Clone)]
pub struct Line {
    pub request_id: String,
    /// RFC 3339 with nanoseconds, UTC.
    pub started_at: String,
    pub session: String,
    pub event: Event,
    pub capture: Option<Capture>,
    pub cost: Option<Cost>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Wire<'a> {
    v: u8,
    schema_version: u8,
    op: &'static str,
    request_id: &'a str,
    started_at: &'a str,
    session: &'a str,
    event: &'a Event,
    #[serde(skip_serializing_if = "Option::is_none")]
    cost: Option<&'a Cost>,
    // Last: `Queued` puts the capture's bytes in place of `{"dropped":true}`.
    #[serde(skip_serializing_if = "Option::is_none")]
    capture: Option<&'a Capture>,
}

const DROPPED: Capture = Capture::Dropped { dropped: true };

impl Line {
    fn wire<'a>(&'a self, capture: Option<&'a Capture>) -> Wire<'a> {
        Wire {
            v: LINE_VERSION,
            schema_version: SCHEMA_VERSION,
            op: "append",
            request_id: &self.request_id,
            started_at: &self.started_at,
            session: &self.session,
            event: &self.event,
            capture,
            cost: self.cost.as_ref(),
        }
    }

    /// The line for the emitter, serialized once (see `Queued`).
    pub fn queued(&self) -> Queued {
        // shortcut: serializing these plain types cannot fail.
        let capture = self.capture.as_ref();
        let mut row =
            serde_json::to_vec(&self.wire(capture.map(|_| &DROPPED))).expect("line serializes");
        row.push(b'\n');
        Queued {
            row,
            capture: capture.map(|c| serde_json::to_vec(c).expect("capture serializes")),
        }
    }
}

impl Serialize for Line {
    fn serialize<S: Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        self.wire(self.capture.as_ref()).serialize(s)
    }
}

/// Go's `http.CanonicalHeaderKey` for a valid header name.
pub fn canonical_header_key(name: &str) -> String {
    let mut up = true;
    name.chars()
        .map(|c| {
            let out = if up {
                c.to_ascii_uppercase()
            } else {
                c.to_ascii_lowercase()
            };
            up = c == '-';
            out
        })
        .collect()
}

/// `h` as Go's `http.Header` writes it in JSON.
pub fn wire_header(h: &http::HeaderMap) -> WireHeader {
    let mut out = WireHeader::new();
    for (k, v) in h {
        out.entry(canonical_header_key(k.as_str()))
            .or_default()
            .push(String::from_utf8_lossy(v.as_bytes()).into_owned());
    }
    out
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use serde_json::Value;
    use std::collections::BTreeSet;

    /// An event with every field set.
    pub(crate) fn full_event() -> Event {
        Event {
            direction: "outbound",
            phase: "response",
            request_id: "r".into(),
            a2a: Some(A2aExtension::default()),
            mcp: Some(McpExtension::default()),
            inference: Some(InferenceExtension::default()),
            status_code: 200,
            error: Some(EventError {
                kind: "k".into(),
                ..Default::default()
            }),
            host: "h".into(),
            duration_ms: 1,
            ttfb_ms: 1,
            bytes_up: 1,
            bytes_down: 1,
            http_method: "POST".into(),
            http_path: "/".into(),
            client: Some(Client {
                name: "n".into(),
                ..Default::default()
            }),
            subagent: "s".into(),
            parent_subagent: "p".into(),
            proc: "p".into(),
            client_proc: "c".into(),
            session_header: "x".into(),
        }
    }

    fn keys(v: &Value) -> BTreeSet<String> {
        v.as_object().unwrap().keys().cloned().collect()
    }

    #[test]
    fn every_session_event_field_is_set_or_listed() {
        let path = concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../../core/pipeline/testdata/session-event.golden.json"
        );
        let golden: Value = serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
        let golden = keys(&golden);
        let rust = keys(&serde_json::to_value(full_event()).unwrap());
        let listed: BTreeSet<String> = NOT_SET_IN_PRAXIS_MODE
            .iter()
            .map(|s| s.to_string())
            .collect();
        let known = &rust | &listed;
        let missing: Vec<_> = golden.difference(&known).collect();
        assert!(
            missing.is_empty(),
            "fields neither set nor listed: {missing:?}"
        );
        let extra: Vec<_> = rust.difference(&golden).collect();
        assert!(extra.is_empty(), "fields not in the Go event: {extra:?}");
        let both: Vec<_> = rust.intersection(&listed).collect();
        assert!(both.is_empty(), "fields set and listed: {both:?}");
    }

    #[test]
    fn line_json_keys_and_dropped_row() {
        let l = Line {
            request_id: "r1".into(),
            started_at: "2026-11-09T10:00:00.000000001Z".into(),
            session: "s".into(),
            event: Event {
                direction: "outbound",
                phase: "request",
                ..Default::default()
            },
            capture: Some(Capture::Request {
                original: b"hi".to_vec(),
                header: WireHeader::new(),
                query: String::new(),
            }),
            cost: None,
        };
        let q = l.queued();
        let full: Value = serde_json::from_slice(&q.with_capture().unwrap()).unwrap();
        assert_eq!(full["v"], 1);
        assert_eq!(full["schemaVersion"], 1);
        assert_eq!(full["op"], "append");
        assert_eq!(full["requestId"], "r1");
        assert_eq!(full["capture"]["original"], "aGk=");
        assert!(full.get("cost").is_none());
        let row: Value = serde_json::from_slice(&q.row).unwrap();
        assert_eq!(row["capture"], serde_json::json!({"dropped": true}));
        assert!(q.row.ends_with(b"\n"));
    }

    #[test]
    fn canonical_keys() {
        assert_eq!(
            canonical_header_key("x-litellm-model-name"),
            "X-Litellm-Model-Name"
        );
        assert_eq!(canonical_header_key("content-type"), "Content-Type");
        assert_eq!(canonical_header_key("te"), "Te");
    }
}

//! Parity with the Go encoder: core/attribution/testdata/wire.golden is
//! attribution.Wire.Encode of the values below (TestWire_EncodeGolden).

use std::path::Path;

use cortex_praxis_filters::record::attribution::{Attribution, decode};

#[test]
fn decodes_go_golden() {
    let path =
        Path::new(env!("CARGO_MANIFEST_DIR")).join("../../core/attribution/testdata/wire.golden");
    let golden = std::fs::read_to_string(&path).expect("read wire.golden");
    let want = Attribution {
        agent: "claude-code".into(),
        proc: "p1".into(),
        client_proc: "p2".into(),
        session: "s1".into(),
        source: "header".into(),
        session_header: "X-Claude-Code-Session-Id".into(),
        chain_session: "s0".into(),
        record_session: "s1".into(),
    };
    assert_eq!(decode(Some(&golden)), want);
}

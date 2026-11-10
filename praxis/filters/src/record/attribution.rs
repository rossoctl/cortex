//! Attribution: the `X-Cortex-Attribution` header, the token check and the hop header lists.

use base64::Engine;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use serde::{Deserialize, Serialize};

/// What the forward proxy knows about the caller, sent in `X-Cortex-Attribution`
/// as base64url JSON without padding. Every key is optional.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct Attribution {
    pub agent: String,
    pub proc: String,
    pub client_proc: String,
    pub session: String,
    pub source: String,
    pub session_header: String,
    pub chain_session: String,
    /// The bucket a row is filed under. Empty: ingestion picks it, as a listener does.
    pub record_session: String,
}

pub const ATTRIBUTION_HEADER: &str = "X-Cortex-Attribution";
pub const TOKEN_HEADER: &str = "X-Cortex-Token";
pub const DEST_HEADER: &str = "X-Cortex-Dest";

/// The hop headers. The request capture gets none of them: `X-Cortex-Token` is
/// the gateway's secret. The upstream gets only those that
/// [`upstream_strip_headers`] does not give.
pub const HOP_HEADERS: [&str; 12] = [
    "X-Cortex-Agent",
    "X-Cortex-Proc",
    "X-Cortex-Client-Proc",
    "X-Cortex-Session",
    "X-Cortex-Source",
    "X-Cortex-Session-Header",
    "X-Cortex-Chain-Session",
    ATTRIBUTION_HEADER,
    "X-Cortex-Request-Id",
    "X-Cortex-Probe",
    DEST_HEADER,
    TOKEN_HEADER,
];

/// The hop headers that the gateway removes before its upstream: all but
/// `X-Cortex-Dest` and `X-Cortex-Token`. The interim upstream is always the
/// Cortex egress listener (#1721), which removes those two itself. At phase B,
/// when the gateway dials origins, it removes all of [`HOP_HEADERS`].
pub fn upstream_strip_headers() -> impl Iterator<Item = &'static str> {
    HOP_HEADERS
        .into_iter()
        .filter(|n| *n != DEST_HEADER && *n != TOKEN_HEADER)
}

/// Decodes the header. A missing header, bad base64url or bad JSON gives the default.
pub fn decode(header: Option<&str>) -> Attribution {
    header
        .and_then(|h| URL_SAFE_NO_PAD.decode(h.trim()).ok())
        .and_then(|b| serde_json::from_slice(&b).ok())
        .unwrap_or_default()
}

/// Compares the given token with `token` in constant time. An empty `token` matches nothing.
pub fn token_matches(given: Option<&[u8]>, token: &[u8]) -> bool {
    let Some(given) = given else {
        return false;
    };
    if token.is_empty() || given.len() != token.len() {
        return false;
    }
    given
        .iter()
        .zip(token)
        .fold(0u8, |acc, (a, b)| acc | (a ^ b))
        == 0
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn attribution_decode_bad_input_is_default() {
        assert_eq!(decode(None), Attribution::default());
        assert_eq!(decode(Some("%%%")), Attribution::default());
        assert_eq!(
            decode(Some(&URL_SAFE_NO_PAD.encode("not json"))),
            Attribution::default()
        );
        assert_eq!(
            decode(Some(&URL_SAFE_NO_PAD.encode(r#"{"session":7}"#))),
            Attribution::default()
        );
        // Padding is not base64url without padding.
        let padded = base64::engine::general_purpose::URL_SAFE.encode(r#"{"agent":"a1"}"#);
        assert!(padded.ends_with('='));
        assert_eq!(decode(Some(&padded)), Attribution::default());

        let good = URL_SAFE_NO_PAD.encode(
            r#"{"agent":"a","proc":"/bin/claude","clientProc":"/bin/zsh","session":"s",
               "source":"header","sessionHeader":"x-session-id","chainSession":"c",
               "recordSession":"r","unknown":1}"#,
        );
        let a = decode(Some(&good));
        assert_eq!(a.agent, "a");
        assert_eq!(a.client_proc, "/bin/zsh");
        assert_eq!(a.session_header, "x-session-id");
        assert_eq!(a.chain_session, "c");
        assert_eq!(a.record_session, "r");
        // A partial header keeps the other keys empty.
        let part = decode(Some(&URL_SAFE_NO_PAD.encode(r#"{"session":"s"}"#)));
        assert_eq!(part.session, "s");
        assert_eq!(part.proc, "");
    }

    #[test]
    fn token_matches_rules() {
        assert!(token_matches(Some(b"tok"), b"tok"));
        assert!(!token_matches(Some(b"tok"), b"tik"));
        assert!(!token_matches(Some(b"to"), b"tok"));
        assert!(!token_matches(None, b"tok"));
        assert!(!token_matches(Some(b""), b""));
        assert!(!token_matches(Some(b"x"), b""));
    }
}

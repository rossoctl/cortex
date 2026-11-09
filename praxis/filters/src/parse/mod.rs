pub mod decode;
pub mod model;
pub mod sse;

/// The cap on a buffered decoded body and on one SSE frame.
/// It is `maxBodySize` in core/listener/forwardproxy/server.go.
pub const MAX_BODY_SIZE: usize = 10 << 20;

/// Reports a response that carries SSE under a JSON content type: the Codex
/// backend does this (core/listener/forwardproxy/stream.go and server.go).
pub fn is_known_mislabeled_sse(status: u16, host: &str, path: &str) -> bool {
    status / 100 == 2 && host == "chatgpt.com" && path == "/backend-api/codex/responses"
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn mislabeled() {
        assert!(is_known_mislabeled_sse(
            200,
            "chatgpt.com",
            "/backend-api/codex/responses"
        ));
        assert!(is_known_mislabeled_sse(
            299,
            "chatgpt.com",
            "/backend-api/codex/responses"
        ));
        assert!(!is_known_mislabeled_sse(
            405,
            "chatgpt.com",
            "/backend-api/codex/responses"
        ));
        assert!(!is_known_mislabeled_sse(
            200,
            "api.openai.com",
            "/backend-api/codex/responses"
        ));
        assert!(!is_known_mislabeled_sse(
            200,
            "chatgpt.com",
            "/v1/responses"
        ));
    }
}

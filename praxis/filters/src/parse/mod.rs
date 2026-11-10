pub mod a2a;
pub mod decode;
pub mod inference;
pub(crate) mod jsonrpc;
pub mod mcp;
pub mod model;
pub mod sse;

/// The cap on a buffered decoded body and on one SSE frame.
/// It is `maxBodySize` in core/listener/forwardproxy/server.go.
pub const MAX_BODY_SIZE: usize = 10 << 20;

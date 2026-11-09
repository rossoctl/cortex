//! The emitter: one thread per process that streams NDJSON lines to Cortex.

/// One line, serialized twice: without and with its capture.
///
/// Each ends in `\n`. `row` holds `"capture":{"dropped":true}` when the line
/// has a capture, and no `capture` key otherwise.
pub struct Queued {
    pub row: Vec<u8>,
    pub with_capture: Option<Vec<u8>>,
}

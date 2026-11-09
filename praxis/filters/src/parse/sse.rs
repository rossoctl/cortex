//! SSE framing: a push-based port of `ReadFrame` in
//! core/listener/internal/sseframe/reader.go, and `NormalizeSSE` from
//! core/internal/parsercommon/sse.go.
//!
//! Praxis gives body chunks of any size. `FrameReader` gives the same frames
//! that Go's `Reader.ReadFrame` gives for the same bytes, at any chunk boundary.

use std::borrow::Cow;
use std::fmt;

const UTF8_BOM: &[u8] = b"\xef\xbb\xbf";

/// One SSE event, or one line, is larger than the per-frame cap
/// (`ErrFrameTooLarge` in Go).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FrameTooLarge {
    /// The frames that the same call completed before the error, in order.
    /// Go's `ReadFrame` gives each of them before it gives the error.
    pub completed: Vec<Vec<u8>>,
}

/// The internal form of `FrameTooLarge`, before the completed frames are attached.
struct Over;

impl fmt::Display for FrameTooLarge {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "single SSE frame over the {}-byte frame cap",
            super::MAX_BODY_SIZE
        )
    }
}

impl std::error::Error for FrameTooLarge {}

/// Gives the data payload of each SSE event that carries a `data:` line.
pub struct FrameReader {
    max: usize,
    /// The first bytes of the stream, held until there are enough to do the
    /// one BOM check (Go peeks 3 bytes at the stream start).
    head: Vec<u8>,
    bom_checked: bool,
    /// The current line, without its terminator.
    line: Vec<u8>,
    /// The last byte was CR, so a LF that comes next is part of its terminator.
    after_cr: bool,
    /// The data lines of the open event, joined with LF.
    scratch: Vec<u8>,
    has_data: bool,
    /// The bytes of the open event's non-data lines.
    non_data: usize,
}

impl FrameReader {
    pub fn new(max_frame: usize) -> Self {
        Self {
            max: max_frame,
            head: Vec::new(),
            bom_checked: false,
            line: Vec::new(),
            after_cr: false,
            scratch: Vec::new(),
            has_data: false,
            non_data: 0,
        }
    }

    /// Gives the data payloads of the events that this chunk completed.
    pub fn push(&mut self, chunk: &[u8]) -> Result<Vec<Vec<u8>>, FrameTooLarge> {
        let mut out = Vec::new();
        let r = if self.bom_checked {
            self.feed(chunk, &mut out)
        } else {
            self.head.extend_from_slice(chunk);
            if self.head.len() < UTF8_BOM.len() {
                return Ok(out);
            }
            self.bom_checked = true;
            let head = std::mem::take(&mut self.head);
            self.feed(head.strip_prefix(UTF8_BOM).unwrap_or(&head), &mut out)
        };
        match r {
            Ok(()) => Ok(out),
            Err(Over) => Err(FrameTooLarge { completed: out }),
        }
    }

    /// Ends the stream: gives the event left open at EOF, if it has data.
    /// Go dispatches it without the blank line, as the SSE spec says.
    pub fn finish(&mut self) -> Result<Vec<Vec<u8>>, FrameTooLarge> {
        let mut out = Vec::new();
        match self.finish_into(&mut out) {
            Ok(()) => Ok(out),
            Err(Over) => Err(FrameTooLarge { completed: out }),
        }
    }

    fn finish_into(&mut self, out: &mut Vec<Vec<u8>>) -> Result<(), Over> {
        if !self.bom_checked {
            // Fewer than 3 bytes: Go's Peek fails, so no BOM is removed.
            self.bom_checked = true;
            let head = std::mem::take(&mut self.head);
            self.feed(&head, out)?;
        }
        if !self.line.is_empty() {
            self.end_line(out)?;
        }
        if self.has_data {
            out.push(std::mem::take(&mut self.scratch));
        }
        self.reset_event();
        Ok(())
    }

    fn feed(&mut self, bytes: &[u8], out: &mut Vec<Vec<u8>>) -> Result<(), Over> {
        for &b in bytes {
            if std::mem::take(&mut self.after_cr) && b == b'\n' {
                continue;
            }
            match b {
                b'\n' => self.end_line(out)?,
                b'\r' => {
                    self.after_cr = true;
                    self.end_line(out)?;
                }
                _ => {
                    if self.line.len() + 1 > self.max {
                        return Err(Over);
                    }
                    self.line.push(b);
                }
            }
        }
        Ok(())
    }

    fn end_line(&mut self, out: &mut Vec<Vec<u8>>) -> Result<(), Over> {
        let line = std::mem::take(&mut self.line);
        if line.is_empty() {
            if self.has_data {
                out.push(std::mem::take(&mut self.scratch));
            }
            self.reset_event();
            return Ok(());
        }
        let (field, value) = split_field(&line);
        if field != b"data" {
            self.non_data += line.len();
            if self.non_data > self.max {
                return Err(Over);
            }
            // Comments, event:, id:, retry: and unknown fields.
            return Ok(());
        }
        let sep = usize::from(self.has_data);
        if self.scratch.len() + sep + value.len() > self.max {
            return Err(Over);
        }
        if self.has_data {
            self.scratch.push(b'\n');
        }
        self.has_data = true;
        self.scratch.extend_from_slice(value);
        Ok(())
    }

    fn reset_event(&mut self) {
        self.scratch.clear();
        self.has_data = false;
        self.non_data = 0;
    }
}

/// Splits "name:value" at the first colon, and removes one leading space
/// from the value. A line with no colon is a name with an empty value.
fn split_field(line: &[u8]) -> (&[u8], &[u8]) {
    match line.iter().position(|&b| b == b':') {
        None => (line, &[]),
        Some(i) => {
            let value = &line[i + 1..];
            (&line[..i], value.strip_prefix(b" ").unwrap_or(value))
        }
    }
}

/// Port of `parsercommon.NormalizeSSE`: removes one leading BOM and changes
/// CRLF and CR line endings to LF. A body with no CR is not copied.
pub fn normalize_sse(body: &[u8]) -> Cow<'_, [u8]> {
    let body = body.strip_prefix(UTF8_BOM).unwrap_or(body);
    if !body.contains(&b'\r') {
        return Cow::Borrowed(body);
    }
    let mut out = Vec::with_capacity(body.len());
    let mut i = 0;
    while i < body.len() {
        if body[i] == b'\r' {
            out.push(b'\n');
            if body.get(i + 1) == Some(&b'\n') {
                i += 1;
            }
        } else {
            out.push(body[i]);
        }
        i += 1;
    }
    Cow::Owned(out)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::parse::MAX_BODY_SIZE;

    const BOM: &str = "\u{feff}";

    /// All frames the reader gives, the frames completed before an error
    /// included, and whether it gave `FrameTooLarge`.
    fn frames(r: &mut FrameReader, input: &[u8], chunk: usize) -> (Vec<Vec<u8>>, bool) {
        let mut out = Vec::new();
        for c in input.chunks(chunk.max(1)) {
            match r.push(c) {
                Ok(f) => out.extend(f),
                Err(e) => {
                    out.extend(e.completed);
                    return (out, true);
                }
            }
        }
        match r.finish() {
            Ok(f) => out.extend(f),
            Err(e) => {
                out.extend(e.completed);
                return (out, true);
            }
        }
        (out, false)
    }

    /// Runs the input in one chunk, in 1-byte chunks and in 7-byte chunks,
    /// and asserts that each gives `want` and no error.
    fn check(input: &str, max: usize, want: &[&str]) {
        let want: Vec<Vec<u8>> = want.iter().map(|s| s.as_bytes().to_vec()).collect();
        for chunk in [input.len(), 1, 7] {
            let got = frames(&mut FrameReader::new(max), input.as_bytes(), chunk);
            assert_eq!(
                got,
                (want.clone(), false),
                "chunk size {chunk}, input {input:?}"
            );
        }
    }

    /// As `check`, but the reader must end with `FrameTooLarge`.
    fn check_too_large(input: &str, max: usize, want: &[&str]) {
        let want: Vec<Vec<u8>> = want.iter().map(|s| s.as_bytes().to_vec()).collect();
        for chunk in [input.len(), 1, 7] {
            let got = frames(&mut FrameReader::new(max), input.as_bytes(), chunk);
            assert_eq!(got, (want.clone(), true), "chunk size {chunk}");
        }
    }

    // Go's NewReader(r, 0) uses DefaultMaxFrameSize.
    const DEFAULT: usize = 1 << 20;

    #[test]
    fn frame_reader_single_frame() {
        check("data: hello\n\n", DEFAULT, &["hello"]);
    }

    #[test]
    fn frame_reader_multiple_frames() {
        check(
            "data: one\n\ndata: two\n\ndata: three\n\n",
            DEFAULT,
            &["one", "two", "three"],
        );
    }

    #[test]
    fn frame_reader_multiline_data() {
        check("data: line1\ndata: line2\n\n", DEFAULT, &["line1\nline2"]);
    }

    #[test]
    fn frame_reader_comment_lines() {
        check(
            ": this is a heartbeat\n: another comment\ndata: payload\n\n",
            DEFAULT,
            &["payload"],
        );
    }

    #[test]
    fn frame_reader_event_and_id_ignored() {
        check(
            "event: status-update\nid: 42\ndata: {\"k\":1}\n\n",
            DEFAULT,
            &["{\"k\":1}"],
        );
    }

    #[test]
    fn frame_reader_trailing_frame_without_blank_line() {
        check("data: only\n", DEFAULT, &["only"]);
        // No terminator at all: Go's readLine gives the tail at EOF.
        check("data: only", DEFAULT, &["only"]);
    }

    #[test]
    fn frame_reader_empty_stream() {
        check("", DEFAULT, &[]);
    }

    #[test]
    fn frame_reader_only_comments() {
        check(": ping\n\n: pong\n\n", DEFAULT, &[]);
    }

    #[test]
    fn frame_reader_crlf() {
        check("data: a\r\n\r\ndata: b\r\n\r\n", DEFAULT, &["a", "b"]);
        // CRLF is one terminator, so it does not end the event.
        check("data: a\r\ndata: b\r\n\r\n", DEFAULT, &["a\nb"]);
    }

    #[test]
    fn frame_reader_cr_only() {
        check("event: x\rdata: a\r\rdata: b\r\r", DEFAULT, &["a", "b"]);
    }

    #[test]
    fn frame_reader_frame_too_large() {
        check_too_large("data: too-long\n\n", 4, &[]);
    }

    #[test]
    fn frame_reader_field_without_space() {
        check("data:hi\n\n", DEFAULT, &["hi"]);
    }

    #[test]
    fn frame_reader_empty_data_line_is_a_frame() {
        check("data:\n\n", DEFAULT, &[""]);
    }

    #[test]
    fn frame_reader_long_data_line() {
        let payload = "x".repeat(8192);
        check(&format!("data: {payload}\n\n"), DEFAULT, &[&payload]);
    }

    // The LastEvent tests: the event type is not part of this API, so only
    // their payloads are ported.
    #[test]
    fn frame_reader_last_event_payloads() {
        check(
            "event: message_start\ndata: {\"x\":1}\n\n",
            DEFAULT,
            &["{\"x\":1}"],
        );
        check(
            "event: message_start\ndata: one\n\ndata: two\n\n",
            DEFAULT,
            &["one", "two"],
        );
    }

    const ANTHROPIC_TURN: &str = "event: message_start\n\
        data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":600}}}\n\n\
        event: message_delta\n\
        data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":50}}\n\n";
    const DATA_ONLY: &str = "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":600}}}\n\n\
        data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":50}}\n\n";
    const START: &str =
        "{\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":600}}}";
    const DELTA: &str = "{\"type\":\"message_delta\",\"usage\":{\"output_tokens\":50}}";

    #[test]
    fn frame_reader_leading_bom() {
        let max = 1 << 20;
        check(ANTHROPIC_TURN, max, &[START, DELTA]);
        check(&format!("{BOM}{ANTHROPIC_TURN}"), max, &[START, DELTA]);
        check(DATA_ONLY, max, &[START, DELTA]);
        check(&format!("{BOM}{DATA_ONLY}"), max, &[START, DELTA]);
        check(&format!("{BOM}{BOM}{ANTHROPIC_TURN}"), max, &[START, DELTA]);
        check(&format!("{BOM}{BOM}{DATA_ONLY}"), max, &[DELTA]);
    }

    #[test]
    fn frame_reader_bom_not_stripped_mid_stream() {
        check(
            &format!("data: first\n\n{BOM}data: second\n\n"),
            1 << 20,
            &["first"],
        );
    }

    #[test]
    fn frame_reader_error_keeps_completed_frames() {
        let input = format!("data: ok\n\ndata:{}\n\n", "a".repeat(20));
        // One chunk: the error carries the frame that the chunk completed first.
        let err = FrameReader::new(10).push(input.as_bytes()).unwrap_err();
        assert_eq!(err.completed, vec![b"ok".to_vec()]);
        check_too_large(&input, 10, &["ok"]);
        // The open event at EOF is over the cap.
        let mut r = FrameReader::new(10);
        assert_eq!(
            r.push(b"data: ok\n\ndata:12345\ndata:12345").unwrap(),
            vec![b"ok".to_vec()]
        );
        assert_eq!(r.finish().unwrap_err().completed, Vec::<Vec<u8>>::new());
    }

    #[test]
    fn frame_reader_cap() {
        // Go also caps one line at max_frame, so the event is two data lines:
        // a + LF + b data bytes.
        let event = |n: usize| {
            let a = n / 2;
            format!("data:{}\ndata:{}\n\n", "a".repeat(a), "b".repeat(n - a - 1))
        };
        let over = event(MAX_BODY_SIZE + 1);
        let exact = event(MAX_BODY_SIZE);
        for chunk in [usize::MAX, 4096] {
            let got = frames(&mut FrameReader::new(MAX_BODY_SIZE), over.as_bytes(), chunk);
            assert_eq!(got, (vec![], true));
            let got = frames(
                &mut FrameReader::new(MAX_BODY_SIZE),
                exact.as_bytes(),
                chunk,
            );
            assert!(!got.1);
            assert_eq!(got.0.len(), 1);
            assert_eq!(got.0[0].len(), MAX_BODY_SIZE);
        }
        // One line over the cap.
        let line = format!("data:{}\n\n", "a".repeat(MAX_BODY_SIZE));
        let got = frames(&mut FrameReader::new(MAX_BODY_SIZE), line.as_bytes(), 4096);
        assert_eq!(got, (vec![], true));
        assert_eq!(
            FrameTooLarge { completed: vec![] }.to_string(),
            "single SSE frame over the 10485760-byte frame cap"
        );
    }

    #[test]
    fn normalize_sse_cases() {
        assert!(matches!(normalize_sse(b"data: a\n\n"), Cow::Borrowed(_)));
        assert_eq!(&*normalize_sse(b"\xef\xbb\xbfdata: a\n\n"), b"data: a\n\n");
        assert_eq!(&*normalize_sse(b"data: a\r\n\r\n"), b"data: a\n\n");
        assert_eq!(
            &*normalize_sse(b"data: a\r\rdata: b\r"),
            b"data: a\n\ndata: b\n"
        );
        assert_eq!(&*normalize_sse(b"a\r\r\nb"), b"a\n\nb");
    }
}

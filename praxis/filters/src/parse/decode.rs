//! Content decoding with a cap on decoded bytes, and a port of
//! `parsercommon.DecodeRequestBody` (core/internal/parsercommon/encoding.go).

use std::borrow::Cow;
use std::fmt;
use std::io::{self, Read, Write};

use zstd::stream::raw::{InBuffer, Operation, OutBuffer};

/// The largest slice that the `_with` forms give the sink in one call.
/// It also bounds the bytes that the decoder holds between calls.
pub const SINK_CHUNK: usize = 64 << 10;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DecodeError {
    /// The decoded output is larger than the cap.
    CapExceeded,
    /// The coded bytes are corrupt or truncated.
    Corrupt,
}

impl fmt::Display for DecodeError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::CapExceeded => write!(
                f,
                "response body over the {}-byte buffer limit",
                super::MAX_BODY_SIZE
            ),
            Self::Corrupt => f.write_str("response body does not decode"),
        }
    }
}

impl std::error::Error for DecodeError {}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Coding {
    Identity,
    Gzip,
    Zstd,
}

/// The gzip decoder's output buffer. It takes at most `SINK_CHUNK` bytes, and
/// then refuses with `WouldBlock` until the `Decoder` gives them to the sink.
/// flate2 keeps the refused output and writes it again on the next call.
#[derive(Default)]
struct Held(Vec<u8>);

impl Write for Held {
    fn write(&mut self, data: &[u8]) -> io::Result<usize> {
        let room = SINK_CHUNK - self.0.len();
        if room == 0 {
            return Err(io::ErrorKind::WouldBlock.into());
        }
        let n = room.min(data.len());
        self.0.extend_from_slice(&data[..n]);
        Ok(n)
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

enum Inner {
    Identity,
    Gzip(Box<flate2::write::GzDecoder<Held>>),
    Zstd {
        d: Box<zstd::stream::raw::Decoder<'static>>,
        out: Vec<u8>,
        /// zstd's hint after the last call: 0 when the input ends on a frame end.
        hint: usize,
    },
}

/// The sink of the `_with` forms: it gets each decoded slice, at most
/// `SINK_CHUNK` bytes, in order.
pub type Sink<'a> = dyn FnMut(&[u8]) -> Result<(), DecodeError> + 'a;

/// Decodes a body pushed in chunks of any size. The cap counts decoded
/// output for the whole life of the `Decoder`.
///
/// The first error is final: each later call gives the same error and no
/// bytes. The bytes given before the error are a prefix of the decoded body.
pub struct Decoder {
    inner: Inner,
    total: usize,
    cap: usize,
    failed: Option<DecodeError>,
}

impl Decoder {
    pub fn new(coding: Coding, cap: usize) -> Self {
        let inner = match coding {
            Coding::Identity => Inner::Identity,
            Coding::Gzip => Inner::Gzip(Box::new(flate2::write::GzDecoder::new(Held::default()))),
            Coding::Zstd => Inner::Zstd {
                // shortcut: zstd's only failure here is an allocation failure.
                d: Box::new({
                    let mut d = zstd::stream::raw::Decoder::new().expect("zstd decoder");
                    // RFC 9659 limits the zstd content coding to an 8 MiB window.
                    d.set_parameter(zstd::stream::raw::DParameter::WindowLogMax(23))
                        .expect("zstd window limit");
                    d
                }),
                out: vec![0; SINK_CHUNK],
                hint: 0,
            },
        };
        Self {
            inner,
            total: 0,
            cap,
            failed: None,
        }
    }

    /// Gives the decoded bytes that this chunk made available. For the
    /// buffered path: it collects them in one `Vec`.
    pub fn push(&mut self, chunk: &[u8]) -> Result<Vec<u8>, DecodeError> {
        let mut out = Vec::new();
        self.push_with(chunk, &mut |b| {
            out.extend_from_slice(b);
            Ok(())
        })?;
        Ok(out)
    }

    /// Ends the body and gives the decoded bytes that were held back.
    pub fn finish(&mut self) -> Result<Vec<u8>, DecodeError> {
        let mut out = Vec::new();
        self.finish_with(&mut |b| {
            out.extend_from_slice(b);
            Ok(())
        })?;
        Ok(out)
    }

    /// As `push`, but gives each decoded slice to `sink` and collects nothing.
    pub fn push_with(&mut self, chunk: &[u8], sink: &mut Sink<'_>) -> Result<(), DecodeError> {
        self.run(sink, |s, emit| s.feed(chunk, emit))
    }

    /// As `finish`, but gives each decoded slice to `sink`. A truncated body
    /// gives `Corrupt` here.
    pub fn finish_with(&mut self, sink: &mut Sink<'_>) -> Result<(), DecodeError> {
        self.run(sink, |s, emit| s.end(emit))
    }

    fn run(
        &mut self,
        sink: &mut Sink<'_>,
        step: impl FnOnce(&mut Inner, &mut Sink<'_>) -> Result<(), DecodeError>,
    ) -> Result<(), DecodeError> {
        if let Some(e) = self.failed {
            return Err(e);
        }
        let (total, cap) = (&mut self.total, self.cap);
        let mut emit = |b: &[u8]| {
            if b.is_empty() {
                return Ok(());
            }
            if total.saturating_add(b.len()) > cap {
                return Err(DecodeError::CapExceeded);
            }
            *total += b.len();
            b.chunks(SINK_CHUNK).try_for_each(&mut *sink)
        };
        let r = step(&mut self.inner, &mut emit);
        self.failed = r.err();
        r
    }
}

/// Ends one gzip member and gives its held output. try_finish fails on a
/// truncated member or a bad CRC32 or ISIZE: `write` only collects the trailer.
fn finish_gzip_member(
    d: &mut flate2::write::GzDecoder<Held>,
    emit: &mut Sink<'_>,
) -> Result<(), DecodeError> {
    loop {
        let r = d.try_finish();
        emit(&std::mem::take(&mut d.get_mut().0))?;
        match r {
            Ok(()) => return Ok(()),
            Err(e) if e.kind() == io::ErrorKind::WouldBlock => {}
            Err(_) => return Err(DecodeError::Corrupt),
        }
    }
}

impl Inner {
    fn feed(&mut self, chunk: &[u8], emit: &mut Sink<'_>) -> Result<(), DecodeError> {
        match self {
            Inner::Identity => emit(chunk),
            Inner::Gzip(d) => {
                let mut input = chunk;
                loop {
                    let r = d.write(input);
                    let held = std::mem::take(&mut d.get_mut().0);
                    emit(&held)?;
                    match r {
                        // Bytes after the end of a gzip member: the next member starts
                        // here (RFC 1952 2.2). Go's gzip.Reader reads every member.
                        Ok(0) if !input.is_empty() => {
                            // Check this member's CRC32 and ISIZE before the next one.
                            finish_gzip_member(d, emit)?;
                            **d = flate2::write::GzDecoder::new(Held::default());
                        }
                        // flate2 writes its output at the start of the next
                        // call, so call it with no input until it gives nothing.
                        Ok(_) if input.is_empty() && held.is_empty() => return Ok(()),
                        Ok(n) => input = &input[n..],
                        Err(e) if e.kind() == io::ErrorKind::WouldBlock => {}
                        Err(_) => return Err(DecodeError::Corrupt),
                    }
                }
            }
            Inner::Zstd { d, out, hint } => {
                let mut inb = InBuffer::around(chunk);
                loop {
                    let mut ob = OutBuffer::around(&mut out[..]);
                    let before = inb.pos();
                    let h = d.run(&mut inb, &mut ob).map_err(|_| DecodeError::Corrupt)?;
                    // A call that moved nothing reports the hint of a next
                    // frame that has not started, so it does not count.
                    if inb.pos() != before || ob.pos() != 0 {
                        *hint = h;
                    }
                    let n = ob.pos();
                    emit(&out[..n])?;
                    if inb.pos() == chunk.len() && n < out.len() {
                        return Ok(());
                    }
                }
            }
        }
    }

    fn end(&mut self, emit: &mut Sink<'_>) -> Result<(), DecodeError> {
        match self {
            Inner::Identity => Ok(()),
            Inner::Gzip(d) => finish_gzip_member(d, emit),
            Inner::Zstd { hint, .. } => {
                // feed already gave all output that the input made. A nonzero
                // hint means the last frame wants more input.
                if *hint == 0 {
                    Ok(())
                } else {
                    Err(DecodeError::Corrupt)
                }
            }
        }
    }
}

/// `MaxDecodedRequestSize` in encoding.go.
pub(crate) const MAX_DECODED_REQUEST_SIZE: usize = 64 << 20;

/// Port of `parsercommon.DecodeRequestBody`: decodes a zstd request body. Any
/// other coding, or a body that does not decode within 64 MiB, comes back as
/// it is.
pub fn decode_request_body<'a>(headers: &http::HeaderMap, body: &'a [u8]) -> Cow<'a, [u8]> {
    let zstd = headers
        .get(http::header::CONTENT_ENCODING)
        .and_then(|v| v.to_str().ok())
        .is_some_and(|v| v.eq_ignore_ascii_case("zstd"));
    if !zstd {
        return Cow::Borrowed(body);
    }
    let mut out = Vec::new();
    // Window limit 2^26: Go's WithDecoderMaxMemory(64 MiB).
    let read = zstd::stream::read::Decoder::new(body).and_then(|mut d| {
        d.window_log_max(26)?;
        d.take(MAX_DECODED_REQUEST_SIZE as u64 + 1)
            .read_to_end(&mut out)
    });
    if read.is_err() || out.len() > MAX_DECODED_REQUEST_SIZE {
        return Cow::Borrowed(body);
    }
    Cow::Owned(out)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::parse::MAX_BODY_SIZE;

    fn gzip(data: &[u8]) -> Vec<u8> {
        let mut e = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::default());
        e.write_all(data).unwrap();
        e.finish().unwrap()
    }

    fn zstd(data: &[u8]) -> Vec<u8> {
        zstd::encode_all(data, 3).unwrap()
    }

    /// The decoded bytes, and the first error.
    fn decode_all(
        coding: Coding,
        cap: usize,
        coded: &[u8],
        chunk: usize,
    ) -> (Vec<u8>, Option<DecodeError>) {
        let mut d = Decoder::new(coding, cap);
        let mut out = Vec::new();
        for c in coded.chunks(chunk) {
            match d.push(c) {
                Ok(b) => out.extend(b),
                Err(e) => return (out, Some(e)),
            }
        }
        match d.finish() {
            Ok(b) => out.extend(b),
            Err(e) => return (out, Some(e)),
        }
        (out, None)
    }

    fn text(n: usize) -> Vec<u8> {
        let mut t = Vec::with_capacity(n);
        let mut i = 0u64;
        while t.len() < n {
            t.extend_from_slice(format!("line {i} of the round-trip text\n").as_bytes());
            i += 1;
        }
        t.truncate(n);
        t
    }

    #[test]
    fn decode_round_trip() {
        let plain = text(3 << 20);
        for (coding, coded) in [
            (Coding::Gzip, gzip(&plain)),
            (Coding::Zstd, zstd(&plain)),
            (Coding::Identity, plain.clone()),
        ] {
            for chunk in [4096, 1, coded.len()] {
                if chunk == 1 && coded.len() > 1 << 20 {
                    continue; // identity in 1-byte chunks is slow and adds nothing
                }
                let (got, err) = decode_all(coding, MAX_BODY_SIZE, &coded, chunk);
                assert_eq!(err, None, "{coding:?} chunk {chunk}");
                assert!(got == plain, "{coding:?} chunk {chunk}");
            }
        }
    }

    #[test]
    fn decode_stops_at_cap() {
        let coded = gzip(&vec![0u8; 11 << 20]);
        assert!(coded.len() < 64 << 10, "coded {} bytes", coded.len());
        let mut d = Decoder::new(Coding::Gzip, MAX_BODY_SIZE);
        let mut total = 0;
        let mut err = None;
        for c in coded.chunks(4096) {
            match d.push(c) {
                Ok(b) => {
                    assert!(b.len() <= MAX_BODY_SIZE);
                    total += b.len();
                }
                Err(e) => {
                    err = Some(e);
                    break;
                }
            }
        }
        if err.is_none() {
            err = d.finish().err();
        }
        assert_eq!(err, Some(DecodeError::CapExceeded));
        assert!(total <= MAX_BODY_SIZE);
        assert_eq!(
            DecodeError::CapExceeded.to_string(),
            "response body over the 10485760-byte buffer limit"
        );
    }

    #[test]
    fn decode_push_with_bounds_each_call() {
        let plain = vec![0u8; 32 << 20];
        for (coding, coded) in [
            (Coding::Gzip, gzip(&plain)),
            (Coding::Zstd, zstd(&plain)),
            (Coding::Identity, plain.clone()),
        ] {
            let mut d = Decoder::new(coding, usize::MAX);
            let (mut total, mut calls, mut max) = (0usize, 0usize, 0usize);
            let mut sink = |b: &[u8]| {
                total += b.len();
                calls += 1;
                max = max.max(b.len());
                Ok(())
            };
            // The whole bomb in one chunk: the worst case for one call.
            d.push_with(&coded, &mut sink).unwrap();
            d.finish_with(&mut sink).unwrap();
            assert_eq!(total, plain.len(), "{coding:?}");
            assert!(max <= SINK_CHUNK, "{coding:?}: one call gave {max} bytes");
            assert!(
                calls >= plain.len() / SINK_CHUNK,
                "{coding:?}: {calls} calls"
            );
        }
    }

    #[test]
    fn decode_push_with_gives_sse_frames_without_delay() {
        // Each chunk's output arrives in the same push, not in the next one.
        // The first part is larger than flate2's 32 KiB output buffer.
        let one = [b"data: ".as_slice(), &text(200_000), b"\n\n"].concat();
        let mut e = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::default());
        e.write_all(&one).unwrap();
        e.flush().unwrap();
        let first = e.get_ref().len();
        e.write_all(b"data: two\n\n").unwrap();
        let coded = e.finish().unwrap();
        let mut d = Decoder::new(Coding::Gzip, usize::MAX);
        assert!(d.push(&coded[..first]).unwrap() == one);
        assert_eq!(d.push(&coded[first..]).unwrap(), b"data: two\n\n");
        assert_eq!(d.finish().unwrap(), b"");
    }

    #[test]
    fn decode_corrupt_body_stops() {
        // A sync flush puts a block boundary on a byte boundary. 0xff after it
        // is a reserved block type, so the decoder fails exactly there.
        let plain = text(100_000);
        let mut e = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::default());
        e.write_all(&plain[..50_000]).unwrap();
        e.flush().unwrap();
        let mut coded = e.get_ref().clone();
        coded.extend_from_slice(&[0xff; 16]);

        let mut d = Decoder::new(Coding::Gzip, MAX_BODY_SIZE);
        let mut got = Vec::new();
        let mut err = None;
        for c in coded.chunks(4096) {
            match d.push(c) {
                Ok(b) => got.extend(b),
                Err(e) => {
                    err = Some(e);
                    break;
                }
            }
        }
        assert_eq!(err, Some(DecodeError::Corrupt));
        assert!(plain.starts_with(&got), "output is not a prefix");
        // The error is final, and a later push gives no bytes.
        assert_eq!(d.push(&coded[..10]), Err(DecodeError::Corrupt));
        assert_eq!(d.finish(), Err(DecodeError::Corrupt));
        assert!(!DecodeError::Corrupt.to_string().is_empty());
    }

    #[test]
    fn decode_truncated_gives_corrupt_at_finish() {
        let plain = text(200_000);
        for (coding, coded) in [(Coding::Gzip, gzip(&plain)), (Coding::Zstd, zstd(&plain))] {
            let cut = &coded[..coded.len() - 10];
            let mut d = Decoder::new(coding, MAX_BODY_SIZE);
            let got = d.push(cut).unwrap();
            assert!(plain.starts_with(&got), "{coding:?}");
            assert_eq!(d.finish(), Err(DecodeError::Corrupt), "{coding:?}");
        }
        // An empty zstd body has no frame to complete.
        assert_eq!(
            decode_all(Coding::Zstd, MAX_BODY_SIZE, b"", 1),
            (vec![], None)
        );
    }

    /// A zstd frame whose header asks for a 2^`log` window.
    fn zstd_window(log: u32) -> Vec<u8> {
        let mut e = zstd::stream::Encoder::new(Vec::new(), 3).unwrap();
        e.window_log(log).unwrap();
        e.long_distance_matching(true).unwrap();
        e.include_contentsize(false).unwrap();
        e.write_all(&text(1000)).unwrap();
        e.finish().unwrap()
    }

    #[test]
    fn zstd_window_limits() {
        let big = zstd_window(27);
        assert_eq!(
            decode_all(Coding::Zstd, MAX_BODY_SIZE, &big, big.len()).1,
            Some(DecodeError::Corrupt)
        );
        let ok = zstd_window(23);
        assert_eq!(
            decode_all(Coding::Zstd, MAX_BODY_SIZE, &ok, ok.len()),
            (text(1000), None)
        );
        let mut h = http::HeaderMap::new();
        h.insert(http::header::CONTENT_ENCODING, "zstd".parse().unwrap());
        assert_eq!(&*decode_request_body(&h, &big), &big[..]);
        assert_eq!(&*decode_request_body(&h, &zstd_window(26)), &text(1000)[..]);
    }

    #[test]
    fn decode_gzip_members() {
        // Two members, as a server that gzips each flush sends them. Go reads both.
        let (one, two) = (text(300_000), b"data: two\n\n".to_vec());
        let coded = [gzip(&one), gzip(&two)].concat();
        for chunk in [1, 7, 4096, 65536, coded.len()] {
            let (got, err) = decode_all(Coding::Gzip, MAX_BODY_SIZE, &coded, chunk);
            assert_eq!(err, None, "chunk {chunk}");
            assert!(got == [one.as_slice(), &two].concat(), "chunk {chunk}");
        }
        // Zero bytes after a member are not a member: Corrupt, as in Go.
        let padded = [gzip(&two), vec![0; 8]].concat();
        assert_eq!(
            decode_all(Coding::Gzip, MAX_BODY_SIZE, &padded, 4096).1,
            Some(DecodeError::Corrupt)
        );
        // A bad CRC32 or ISIZE in a member that is not the last: Corrupt, as in Go.
        for at in [8, 1] {
            let mut bad = gzip(&one);
            let n = bad.len();
            bad[n - at] ^= 0xff;
            let coded = [bad, gzip(&two)].concat();
            for chunk in [1, 7, 4096, coded.len()] {
                let (_, err) = decode_all(Coding::Gzip, MAX_BODY_SIZE, &coded, chunk);
                assert_eq!(err, Some(DecodeError::Corrupt), "byte -{at}, chunk {chunk}");
            }
        }
    }

    #[test]
    fn decode_request_body_cases() {
        let plain = br#"{"model":"gpt-5"}"#;
        let z = zstd(plain);
        let mut h = http::HeaderMap::new();
        assert_eq!(&*decode_request_body(&h, &z), &z[..]);
        h.insert(http::header::CONTENT_ENCODING, "ZSTD".parse().unwrap());
        assert_eq!(&*decode_request_body(&h, &z), plain);
        // Not zstd: the body as it is.
        assert_eq!(&*decode_request_body(&h, plain), plain);
        assert_eq!(
            &*decode_request_body(&h, &z[..z.len() - 2]),
            &z[..z.len() - 2]
        );
        h.insert(http::header::CONTENT_ENCODING, "gzip".parse().unwrap());
        let g = gzip(plain);
        assert_eq!(&*decode_request_body(&h, &g), &g[..]);
    }
}

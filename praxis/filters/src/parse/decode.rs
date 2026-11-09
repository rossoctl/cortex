//! Content decoding with a cap on decoded bytes, and a port of
//! `parsercommon.DecodeRequestBody` (core/internal/parsercommon/encoding.go).

use std::borrow::Cow;
use std::fmt;
use std::io::{self, Read, Write};

/// The decoded output is larger than the cap.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct CapExceeded;

impl fmt::Display for CapExceeded {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "response body over the {}-byte buffer limit",
            super::MAX_BODY_SIZE
        )
    }
}

impl std::error::Error for CapExceeded {}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Coding {
    Identity,
    Gzip,
    Zstd,
}

/// The coding a `Content-Encoding` value names, case-insensitive. `None`
/// for any other coding: the caller then does not parse the body.
pub fn coding_of(content_encoding: Option<&str>) -> Option<Coding> {
    let v = content_encoding.unwrap_or("");
    if v.is_empty() || v.eq_ignore_ascii_case("identity") {
        Some(Coding::Identity)
    } else if v.eq_ignore_ascii_case("gzip") {
        Some(Coding::Gzip)
    } else if v.eq_ignore_ascii_case("zstd") {
        Some(Coding::Zstd)
    } else {
        None
    }
}

/// A `Vec<u8>` that refuses to grow past the cap. `total` counts every byte
/// it took, also the bytes that `take` gave out.
struct Sink {
    buf: Vec<u8>,
    total: usize,
    cap: usize,
    over: bool,
}

impl Write for Sink {
    fn write(&mut self, data: &[u8]) -> io::Result<usize> {
        if self.total.saturating_add(data.len()) > self.cap {
            self.over = true;
            return Err(io::Error::other(CapExceeded));
        }
        self.total += data.len();
        self.buf.extend_from_slice(data);
        Ok(data.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

enum Inner {
    Identity(Sink),
    Gzip(Box<flate2::write::GzDecoder<Sink>>),
    Zstd(Box<zstd::stream::write::Decoder<'static, Sink>>),
}

/// Decodes a body pushed in chunks of any size. The cap counts decoded
/// output for the whole life of the `Decoder`.
///
/// Coded bytes that do not decode (a truncated or corrupt body) stop the
/// decoder: it keeps what it gave, and gives nothing more. Go's reader stops
/// at the same point with a read error.
pub struct Decoder {
    inner: Inner,
    broken: bool,
}

impl Decoder {
    pub fn new(coding: Coding, cap: usize) -> Self {
        let sink = Sink {
            buf: Vec::new(),
            total: 0,
            cap,
            over: false,
        };
        let inner = match coding {
            Coding::Identity => Inner::Identity(sink),
            Coding::Gzip => Inner::Gzip(Box::new(flate2::write::GzDecoder::new(sink))),
            Coding::Zstd => Inner::Zstd(Box::new(
                // shortcut: zstd's only failure here is an allocation failure.
                zstd::stream::write::Decoder::new(sink).expect("zstd decoder"),
            )),
        };
        Self {
            inner,
            broken: false,
        }
    }

    /// Gives the decoded bytes that this chunk made available.
    pub fn push(&mut self, chunk: &[u8]) -> Result<Vec<u8>, CapExceeded> {
        if !self.broken {
            let r = match &mut self.inner {
                Inner::Identity(s) => s.write_all(chunk),
                Inner::Gzip(d) => d.write_all(chunk),
                Inner::Zstd(d) => d.write_all(chunk),
            };
            self.broken = r.is_err();
        }
        self.take()
    }

    /// Ends the body and gives the decoded bytes that were held back.
    pub fn finish(&mut self) -> Result<Vec<u8>, CapExceeded> {
        if !self.broken {
            let r = match &mut self.inner {
                Inner::Identity(_) => Ok(()),
                Inner::Gzip(d) => d.try_finish(),
                Inner::Zstd(d) => d.flush(),
            };
            self.broken = r.is_err();
        }
        self.take()
    }

    fn take(&mut self) -> Result<Vec<u8>, CapExceeded> {
        let sink = match &mut self.inner {
            Inner::Identity(s) => s,
            Inner::Gzip(d) => d.get_mut(),
            Inner::Zstd(d) => d.get_mut(),
        };
        if sink.over {
            sink.buf = Vec::new();
            return Err(CapExceeded);
        }
        Ok(std::mem::take(&mut sink.buf))
    }
}

/// `MaxDecodedRequestSize` in encoding.go.
const MAX_DECODED_REQUEST_SIZE: usize = 64 << 20;

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
    let read = zstd::stream::read::Decoder::new(body).and_then(|d| {
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

    fn decode_all(
        coding: Coding,
        cap: usize,
        coded: &[u8],
        chunk: usize,
    ) -> Result<Vec<u8>, CapExceeded> {
        let mut d = Decoder::new(coding, cap);
        let mut out = Vec::new();
        for c in coded.chunks(chunk) {
            out.extend(d.push(c)?);
        }
        out.extend(d.finish()?);
        Ok(out)
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
        let z = zstd::encode_all(&plain[..], 3).unwrap();
        for (coding, coded) in [
            (Coding::Gzip, gzip(&plain)),
            (Coding::Zstd, z),
            (Coding::Identity, plain.clone()),
        ] {
            let got = decode_all(coding, MAX_BODY_SIZE, &coded, 4096).unwrap();
            assert!(got == plain, "{coding:?}");
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
        assert_eq!(err, Some(CapExceeded));
        assert!(total <= MAX_BODY_SIZE);
        assert_eq!(
            CapExceeded.to_string(),
            "response body over the 10485760-byte buffer limit"
        );
    }

    #[test]
    fn decode_corrupt_body_stops() {
        let mut coded = gzip(&text(100_000));
        coded[20] ^= 0xff;
        coded.truncate(coded.len() / 2);
        // No panic, no error: the decoder gives what it could and then nothing.
        let got = decode_all(Coding::Gzip, MAX_BODY_SIZE, &coded, 4096).unwrap();
        assert!(got.len() < 100_000);
    }

    #[test]
    fn coding_of_cases() {
        assert_eq!(coding_of(None), Some(Coding::Identity));
        assert_eq!(coding_of(Some("")), Some(Coding::Identity));
        assert_eq!(coding_of(Some("Identity")), Some(Coding::Identity));
        assert_eq!(coding_of(Some("GZIP")), Some(Coding::Gzip));
        assert_eq!(coding_of(Some("zStd")), Some(Coding::Zstd));
        assert_eq!(coding_of(Some("br")), None);
        assert_eq!(coding_of(Some("gzip, br")), None);
    }

    #[test]
    fn decode_request_body_cases() {
        let plain = br#"{"model":"gpt-5"}"#;
        let z = zstd::encode_all(&plain[..], 3).unwrap();
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

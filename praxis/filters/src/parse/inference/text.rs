//! Text helpers that keep Go's string rules.

use std::borrow::Cow;

/// A body as text, with each byte that does not start a valid UTF-8 sequence
/// replaced by U+FFFD, one per byte, as Go's decoder reads it.
pub(crate) struct LossyBody<'a> {
    pub(crate) text: Cow<'a, str>,
    /// The text offset of each U+FFFD that replaced one byte, in order.
    fixes: Vec<usize>,
}

pub(crate) fn go_lossy_utf8(body: &[u8]) -> LossyBody<'_> {
    if let Ok(s) = std::str::from_utf8(body) {
        return LossyBody {
            text: Cow::Borrowed(s),
            fixes: Vec::new(),
        };
    }
    let mut text = String::with_capacity(body.len() + 8);
    let mut fixes = Vec::new();
    let mut rest = body;
    loop {
        match std::str::from_utf8(rest) {
            Ok(s) => {
                text.push_str(s);
                break;
            }
            Err(e) => {
                let (ok, bad) = rest.split_at(e.valid_up_to());
                text.push_str(std::str::from_utf8(ok).unwrap_or_default());
                fixes.push(text.len());
                text.push('\u{FFFD}');
                // One byte only: Go's utf8.DecodeRune gives size 1 for each invalid byte.
                rest = &bad[1..];
            }
        }
    }
    LossyBody {
        text: Cow::Owned(text),
        fixes,
    }
}

impl LossyBody<'_> {
    /// The length in the original bytes of `sub`, a substring of `text`.
    pub(crate) fn raw_len(&self, sub: &str) -> usize {
        if self.fixes.is_empty() {
            return sub.len();
        }
        let start = sub.as_ptr() as usize - self.text.as_ptr() as usize;
        let end = start + sub.len();
        let n =
            self.fixes.partition_point(|&f| f < end) - self.fixes.partition_point(|&f| f < start);
        sub.len() - 2 * n
    }
}

/// Port of model.IsControlRune (core/model/client.go).
pub(crate) fn is_control_rune(r: char) -> bool {
    let u = r as u32;
    if u < 0x20 || u == 0x7f || (0x80..=0x9f).contains(&u) {
        return true;
    }
    matches!(
        r,
        '\u{202a}'
            | '\u{202b}'
            | '\u{202c}'
            | '\u{202d}'
            | '\u{202e}'
            | '\u{2066}'
            | '\u{2067}'
            | '\u{2068}'
            | '\u{2069}'
            | '\u{200e}'
            | '\u{200f}'
            | '\u{061c}'
            | '\u{200b}'
            | '\u{200c}'
            | '\u{200d}'
            | '\u{2060}'
            | '\u{feff}'
    )
}

/// Port of model.SanitizeControlRunes. The input is valid UTF-8 here, so only
/// the control-rune half of the Go rule applies.
pub(crate) fn sanitize_control_runes(s: &str) -> Cow<'_, str> {
    if !s.chars().any(is_control_rune) {
        return Cow::Borrowed(s);
    }
    Cow::Owned(
        s.chars()
            .map(|c| if is_control_rune(c) { '\u{FFFD}' } else { c })
            .collect(),
    )
}

/// bytes.TrimSpace: trims Unicode white space (unicode.IsSpace) at both ends.
/// A byte that is not valid UTF-8 is not space, and stops the trim.
pub(crate) fn go_trim_space(mut b: &[u8]) -> &[u8] {
    while let Some((c, n)) = first_rune(b) {
        if !c.is_whitespace() {
            break;
        }
        b = &b[n..];
    }
    while let Some((c, n)) = last_rune(b) {
        if !c.is_whitespace() {
            break;
        }
        b = &b[..b.len() - n];
    }
    b
}

fn first_rune(b: &[u8]) -> Option<(char, usize)> {
    let head = &b[..b.len().min(4)];
    let valid = match std::str::from_utf8(head) {
        Ok(s) => s,
        Err(e) => std::str::from_utf8(&head[..e.valid_up_to()]).ok()?,
    };
    let c = valid.chars().next()?;
    Some((c, c.len_utf8()))
}

fn last_rune(b: &[u8]) -> Option<(char, usize)> {
    for n in 1..=b.len().min(4) {
        if let Ok(s) = std::str::from_utf8(&b[b.len() - n..]) {
            let c = s.chars().next_back()?;
            return (c.len_utf8() == n).then_some((c, n));
        }
    }
    None
}

/// strings.ToLower, for the ASCII matches it is used for: Go maps U+0130 to a
/// plain 'i', where Rust's full mapping gives two runes.
pub(crate) fn go_to_lower(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for c in s.chars() {
        if c == '\u{130}' {
            out.push('i');
        } else {
            out.extend(c.to_lowercase());
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn lossy_one_rune_per_byte() {
        // A truncated 3-byte sequence is two invalid bytes: Go gives two U+FFFD.
        let b = go_lossy_utf8(b"\"a\xe2\x82b\xff\"");
        assert_eq!(b.text, "\"a\u{FFFD}\u{FFFD}b\u{FFFD}\"");
        assert_eq!(b.raw_len(&b.text), 7);
        assert_eq!(b.raw_len(&b.text[2..5]), 1);
        let ok = go_lossy_utf8("\"\u{FFFD}\"".as_bytes());
        assert_eq!(ok.raw_len(&ok.text), 5);
    }

    #[test]
    fn trim_space_is_unicode() {
        assert_eq!(go_trim_space(b" \xc2\xa0{}\xc2\xa0\t"), b"{}");
        assert_eq!(go_trim_space(b"\xff{} "), b"\xff{}");
        assert_eq!(go_trim_space(b" {}\xa0"), b"{}\xa0");
    }

    #[test]
    fn to_lower_like_go() {
        assert_eq!(go_to_lower("X-ANTHROP\u{130}C"), "x-anthropic");
    }

    #[test]
    fn sanitize() {
        assert_eq!(sanitize_control_runes("a\u{202e}b\n"), "a\u{FFFD}b\u{FFFD}");
        assert!(matches!(sanitize_control_runes("ab"), Cow::Borrowed(_)));
    }
}

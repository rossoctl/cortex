//! Decoding with the rules of Go's encoding/json, for the structs that the Go
//! parser decodes into. serde's derive differs from Go in ways that change
//! the result: Go matches keys case-insensitively, lets a later duplicate key
//! win (and merges a duplicate object into the one before it), takes `null` as
//! "no change", and replaces an unpaired surrogate escape with U+FFFD.
//!
//! Each Go struct gets a decode function that walks the object's entries in
//! order and decodes each matched value into the field, as Go does. Every
//! error is a `Bad`: the Go callers treat a syntax error and a type error the
//! same way.

use std::borrow::Cow;

use serde::Deserialize;
use serde::de::{Deserializer, MapAccess, Visitor};
use serde_json::Value;
use serde_json::value::RawValue;

/// A Go decode error.
#[derive(Debug)]
pub(crate) struct Bad;

pub(crate) type R<T = ()> = Result<T, Bad>;

/// Go's scanner refuses a body nested deeper than this (`maxNestingDepth`).
const MAX_NESTING_DEPTH: usize = 10000;

/// Checks the syntax of a whole body, as Go does before it decodes.
pub(crate) fn parse(text: &str) -> R<&RawValue> {
    let raw: &RawValue = serde_json::from_str(text).map_err(|_| Bad)?;
    // serde has no depth limit for a raw value; Go's scanner has one.
    let (mut depth, mut in_str, mut esc) = (0usize, false, false);
    for &b in raw.get().as_bytes() {
        match (in_str, esc, b) {
            (true, true, _) => esc = false,
            (true, false, b'\\') => esc = true,
            (true, false, b'"') => in_str = false,
            (true, ..) => {}
            (false, _, b'"') => in_str = true,
            (false, _, b'[' | b'{') => {
                depth += 1;
                if depth > MAX_NESTING_DEPTH {
                    return Err(Bad);
                }
            }
            (false, _, b']' | b'}') => depth -= 1,
            _ => {}
        }
    }
    Ok(raw)
}

fn first(raw: &RawValue) -> u8 {
    raw.get().as_bytes()[0]
}

pub(crate) fn is_null(raw: &RawValue) -> bool {
    raw.get() == "null"
}

struct Entries<'a>(Vec<(&'a RawValue, &'a RawValue)>);

impl<'de> Deserialize<'de> for Entries<'de> {
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        struct V;
        impl<'de> Visitor<'de> for V {
            type Value = Entries<'de>;
            fn expecting(&self, f: &mut std::fmt::Formatter) -> std::fmt::Result {
                f.write_str("an object")
            }
            fn visit_map<A: MapAccess<'de>>(self, mut m: A) -> Result<Self::Value, A::Error> {
                let mut v = Vec::new();
                while let Some(e) = m.next_entry()? {
                    v.push(e);
                }
                Ok(Entries(v))
            }
        }
        d.deserialize_map(V)
    }
}

/// The entries of an object, in order, keys unquoted as Go does. `None` for
/// null (Go leaves the target as it is). A value of another type is an error.
pub(crate) fn object(raw: &RawValue) -> R<Option<Vec<(String, &RawValue)>>> {
    match first(raw) {
        b'n' => Ok(None),
        b'{' => {
            let e: Entries = serde_json::from_str(raw.get()).map_err(|_| Bad)?;
            Ok(Some(
                e.0.into_iter()
                    .map(|(k, v)| (unquote(k.get()), v))
                    .collect(),
            ))
        }
        _ => Err(Bad),
    }
}

/// Decodes an array into a new slice. `None` for null (Go sets the slice to nil).
pub(crate) fn slice<'a, T>(
    raw: &'a RawValue,
    mut elem: impl FnMut(&'a RawValue) -> R<T>,
) -> R<Option<Vec<T>>> {
    match first(raw) {
        b'n' => Ok(None),
        b'[' => {
            let items: Vec<&RawValue> = serde_json::from_str(raw.get()).map_err(|_| Bad)?;
            items
                .into_iter()
                .map(&mut elem)
                .collect::<R<Vec<T>>>()
                .map(Some)
        }
        _ => Err(Bad),
    }
}

/// Decodes a pointer-to-struct field: null sets it to nil, an object decodes
/// into the struct it points to, allocated when nil.
pub(crate) fn ptr_into<'a, T: Default>(
    dst: &mut Option<T>,
    raw: &'a RawValue,
    decode: impl FnOnce(&'a RawValue, &mut T) -> R,
) -> R {
    if is_null(raw) {
        *dst = None;
        return Ok(());
    }
    decode(raw, dst.get_or_insert_with(T::default))
}

/// A JSON string's value. `None` for null. A value of another type is an error.
pub(crate) fn string(raw: &RawValue) -> R<Option<String>> {
    match first(raw) {
        b'n' => Ok(None),
        b'"' => Ok(Some(unquote(raw.get()))),
        _ => Err(Bad),
    }
}

pub(crate) fn string_into(dst: &mut String, raw: &RawValue) -> R {
    if let Some(s) = string(raw)? {
        *dst = s;
    }
    Ok(())
}

pub(crate) fn bool_into(dst: &mut bool, raw: &RawValue) -> R {
    match raw.get() {
        "null" => {}
        "true" => *dst = true,
        "false" => *dst = false,
        _ => return Err(Bad),
    }
    Ok(())
}

fn is_number(raw: &RawValue) -> bool {
    matches!(first(raw), b'-' | b'0'..=b'9')
}

/// A Go `*int`: null is nil, and a number must parse as an integer (Go uses
/// strconv.ParseInt, so `1.0` and `1e2` are errors).
pub(crate) fn int(raw: &RawValue) -> R<Option<i64>> {
    match raw.get() {
        "null" => Ok(None),
        s if is_number(raw) => s.parse().map(Some).map_err(|_| Bad),
        _ => Err(Bad),
    }
}

pub(crate) fn int_into(dst: &mut i64, raw: &RawValue) -> R {
    if let Some(n) = int(raw)? {
        *dst = n;
    }
    Ok(())
}

/// A Go `*float64`: null is nil. Out of range is an error, as in strconv.ParseFloat.
pub(crate) fn float(raw: &RawValue) -> R<Option<f64>> {
    match raw.get() {
        "null" => Ok(None),
        s if is_number(raw) => match s.parse::<f64>() {
            Ok(f) if f.is_finite() => Ok(Some(f)),
            _ => Err(Bad),
        },
        _ => Err(Bad),
    }
}

/// A Go `any`: numbers are float64, a duplicate key's last value wins. `None` for null.
///
/// shortcut: serde_json's own `Value` parser, so a value nested deeper than 128 levels is
/// not decoded, where Go takes it (Go's limit is 10000). For that one, `any` gives
/// `Ok(None)`, so the caller keeps the rest of the extension with this one field unset.
/// A number out of float64 range is still `Err`, as in Go. A deep `Value` also overflows
/// the stack on drop, so a port of Go's limit needs a non-recursive `Value` first.
pub(crate) fn any(raw: &RawValue) -> R<Option<Value>> {
    fn to_f64(v: &mut Value) {
        match v {
            Value::Number(n) => *v = n.as_f64().map_or(Value::Null, Value::from),
            Value::Array(a) => a.iter_mut().for_each(to_f64),
            Value::Object(m) => m.values_mut().for_each(to_f64),
            _ => {}
        }
    }
    let parse = |t: &str| serde_json::from_str::<Value>(t);
    let mut v = match parse(raw.get()).or_else(|e| match fffd_unpaired_surrogates(raw.get()) {
        Cow::Owned(t) => parse(&t),
        Cow::Borrowed(_) => Err(e),
    }) {
        Ok(v) => v,
        // Go also fails on this one; serde_json gives no error code to match.
        Err(e) if e.to_string().starts_with("number out of range") => return Err(Bad),
        Err(_) => return Ok(None),
    };
    to_f64(&mut v);
    Ok((!v.is_null()).then_some(v))
}

/// Rewrites each unpaired surrogate escape in JSON text to `\ufffd`, as Go's unquote
/// does, so that serde_json, which refuses one, decodes the value. The text is valid JSON,
/// so a backslash is always inside a string.
fn fffd_unpaired_surrogates(text: &str) -> Cow<'_, str> {
    if !text.contains("\\u") {
        return text.into();
    }
    let mut out = String::with_capacity(text.len());
    let mut rest = text;
    let mut changed = false;
    while let Some(i) = rest.find('\\') {
        out.push_str(&rest[..i]);
        rest = &rest[i..];
        let Some(r) = getu4(rest).filter(|r| (0xd800..0xe000).contains(r)) else {
            out.push_str(&rest[..2]);
            rest = &rest[2..];
            continue;
        };
        match getu4(&rest[6..]) {
            Some(r2) if r < 0xdc00 && (0xdc00..0xe000).contains(&r2) => {
                out.push_str(&rest[..12]);
                rest = &rest[12..];
            }
            _ => {
                out.push_str("\\ufffd");
                rest = &rest[6..];
                changed = true;
            }
        }
    }
    if !changed {
        return text.into();
    }
    out.push_str(rest);
    out.into()
}

/// Reports whether an object key selects the field `name`: Go compares them
/// with bytes.EqualFold. `name` is ASCII, so the only non-ASCII runes that
/// fold to one of its letters are U+017F (s) and U+212A (k).
pub(crate) fn is(key: &str, name: &str) -> bool {
    go_equal_fold(key, name)
}

/// strings.EqualFold, for an ASCII `name`.
pub(crate) fn go_equal_fold(s: &str, name: &str) -> bool {
    let mut n = name.bytes();
    for c in s.chars() {
        let Some(b) = n.next() else { return false };
        let b = b.to_ascii_lowercase();
        let ok = match c {
            '\u{17f}' => b == b's',
            '\u{212a}' => b == b'k',
            c => c.is_ascii() && (c as u8).to_ascii_lowercase() == b,
        };
        if !ok {
            return false;
        }
    }
    n.next().is_none()
}

/// The value of a JSON string literal (with its quotes), as Go's unquote
/// gives it: an unpaired surrogate escape becomes U+FFFD.
pub(crate) fn unquote(lit: &str) -> String {
    let s = &lit[1..lit.len() - 1];
    if !s.contains('\\') {
        return s.to_owned();
    }
    let mut out = String::with_capacity(s.len());
    let mut rest = s;
    while let Some(i) = rest.find('\\') {
        out.push_str(&rest[..i]);
        rest = &rest[i..];
        let c = match rest.as_bytes()[1] {
            b'u' => {
                let r = getu4(rest).unwrap_or(0xfffd);
                rest = &rest[6..];
                if (0xd800..0xe000).contains(&r) {
                    let r2 = getu4(rest);
                    match r2 {
                        Some(r2) if r < 0xdc00 && (0xdc00..0xe000).contains(&r2) => {
                            rest = &rest[6..];
                            out.push(
                                char::from_u32(0x10000 + ((r - 0xd800) << 10 | (r2 - 0xdc00)))
                                    .unwrap_or('\u{fffd}'),
                            );
                        }
                        _ => out.push('\u{fffd}'),
                    }
                    continue;
                }
                out.push(char::from_u32(r).unwrap_or('\u{fffd}'));
                continue;
            }
            b'b' => '\u{8}',
            b'f' => '\u{c}',
            b'n' => '\n',
            b'r' => '\r',
            b't' => '\t',
            c => c as char, // '"', '\\', '/': the syntax check allows no other
        };
        out.push(c);
        rest = &rest[2..];
    }
    out.push_str(rest);
    out
}

/// The code unit of a `\uXXXX` escape at the start of s.
fn getu4(s: &str) -> Option<u32> {
    let h = s.strip_prefix("\\u")?.get(..4)?;
    u32::from_str_radix(h, 16).ok()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn unquote_matches_go() {
        assert_eq!(
            unquote(r#""\ud800x\udc00😀\n\/""#),
            "\u{fffd}x\u{fffd}\u{1F600}\n/"
        );
        assert_eq!(unquote(r#""\ud800\n""#), "\u{fffd}\n");
    }

    #[test]
    fn equal_fold() {
        assert!(is("Meſſages", "messages"));
        assert!(is("\u{212a}EY", "key"));
        assert!(!is("message", "messages"));
        assert!(!is("messageş", "messages"));
    }

    #[test]
    fn nesting_depth_as_go() {
        let nest = |n: usize| format!("{}{}", "[".repeat(n), "]".repeat(n));
        assert!(parse(&nest(MAX_NESTING_DEPTH)).is_ok());
        assert!(parse(&nest(MAX_NESTING_DEPTH + 1)).is_err());
        assert!(parse(r#"["[[[", "\"{"]"#).is_ok());
    }

    #[test]
    fn any_is_float64() {
        let v = any(parse(r#"{"a":1,"a":[2,"\u00e9"]}"#).unwrap())
            .unwrap()
            .unwrap();
        assert_eq!(v, serde_json::json!({"a": [2.0, "\u{e9}"]}));
        assert!(any(parse("null").unwrap()).unwrap().is_none());
    }

    #[test]
    fn any_unpaired_surrogate_is_fffd_as_go() {
        let v = any(parse(r#"{"a":"\ud800","b":"\ud800\ud800\udc00","\udc00k":[1,"x\udc00"],"c":"\\ud800","d":"\ud83d\ude00"}"#).unwrap())
            .unwrap()
            .unwrap();
        assert_eq!(
            v,
            serde_json::json!({
                "a": "\u{fffd}",
                "b": "\u{fffd}\u{10000}",
                "\u{fffd}k": [1.0, "x\u{fffd}"],
                "c": "\\ud800",
                "d": "\u{1F600}",
            })
        );
    }
}

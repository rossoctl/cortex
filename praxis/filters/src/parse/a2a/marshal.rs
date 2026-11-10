//! Port of Go's encoding/json Marshal (encode.go) for a value that json.Unmarshal
//! decoded into an `any`: objects, arrays, strings, float64, bool and null.

use std::fmt::Write;

use serde_json::Value;

/// Go's json.Marshal of a decoded `any`. Object keys are sorted by bytes (the
/// serde_json `Map` of this workspace is a `BTreeMap`), and every number is a float64.
pub(crate) fn go_marshal(v: &Value) -> String {
    let mut out = String::new();
    encode(&mut out, v);
    out
}

fn encode(out: &mut String, v: &Value) {
    match v {
        Value::Null => out.push_str("null"),
        Value::Bool(b) => out.push_str(if *b { "true" } else { "false" }),
        Value::Number(n) => float(out, n.as_f64().unwrap_or_default()),
        Value::String(s) => string(out, s),
        Value::Array(a) => {
            out.push('[');
            for (i, e) in a.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                encode(out, e);
            }
            out.push(']');
        }
        Value::Object(m) => {
            out.push('{');
            for (i, (k, e)) in m.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                string(out, k);
                out.push(':');
                encode(out, e);
            }
            out.push('}');
        }
    }
}

/// Port of floatEncoder for float64: the shortest digits, in 'e' format when
/// abs < 1e-6 or abs >= 1e21, else in 'f' format.
fn float(out: &mut String, f: f64) {
    let abs = f.abs();
    if abs != 0.0 && !(1e-6..1e21).contains(&abs) {
        // Rust writes "1e21" and "1.5e-7". Go writes "1e+21" and, after its
        // clean-up of "e-07" to "e-7", the same negative exponent as Rust.
        let s = format!("{f:e}");
        match s.split_once("e-") {
            Some(_) => out.push_str(&s),
            None => out.push_str(&s.replacen('e', "e+", 1)),
        }
    } else {
        // Display gives the shortest digits and never an exponent, as Go's 'f' with -1.
        let _ = write!(out, "{f}");
    }
}

/// Port of appendString with escapeHTML set.
fn string(out: &mut String, s: &str) {
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\u{8}' => out.push_str("\\b"),
            '\u{c}' => out.push_str("\\f"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '<' | '>' | '&' | '\u{2028}' | '\u{2029}' | '\0'..='\u{1f}' => {
                let _ = write!(out, "\\u{:04x}", c as u32);
            }
            c => out.push(c),
        }
    }
    out.push('"');
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::parse::inference::gojson;

    fn marshal(json: &str) -> String {
        let v = gojson::any(gojson::parse(json).unwrap()).unwrap().unwrap();
        go_marshal(&v)
    }

    #[test]
    fn go_marshal_matches_go() {
        assert_eq!(
            marshal(r#"{"b":1,"a":[true,null,"<&>"]}"#),
            r#"{"a":[true,null,"\u003c\u0026\u003e"],"b":1}"#
        );
        assert_eq!(marshal("1e21"), "1e+21");
        assert_eq!(marshal("1e-7"), "1e-7");
        assert_eq!(marshal("1.5e-10"), "1.5e-10");
        assert_eq!(marshal("0.000001"), "0.000001");
        assert_eq!(marshal("123456789012345680000"), "123456789012345680000");
        assert_eq!(marshal("1.5"), "1.5");
        assert_eq!(marshal("-0.0"), "-0");
        assert_eq!(marshal("-0"), "-0");
        assert_eq!(
            marshal(r#"" \u0001\b\u2028\u2029\u007f\"\\/\f\n\r\t\u001f""#),
            "\" \\u0001\\b\\u2028\\u2029\u{7f}\\\"\\\\/\\f\\n\\r\\t\\u001f\""
        );
        assert_eq!(marshal(r#"{"\u003c":"é"}"#), r#"{"\u003c":"é"}"#);
    }
}

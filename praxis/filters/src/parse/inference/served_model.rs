//! Port of core/parse/inference/served_model.go.

use http::HeaderMap;

use super::text::{go_lossy_utf8, sanitize_control_runes};
use crate::parse::model::InferenceExtension;

/// The header a LiteLLM gateway names its deployment's model in.
const SERVED_MODEL_HEADER: &str = "x-litellm-model-name";

/// Bounds a model name read off the wire.
const MAX_SERVED_MODEL_LEN: usize = 256;

/// Keeps the response body's "model" as the served model when no header has named one.
pub(crate) fn note_body_model(ext: &mut InferenceExtension, model: &str) {
    if model.is_empty() || !ext.served_model.is_empty() {
        return;
    }
    ext.served_model = clean_served_model(model);
}

/// Decides `served_model` at the end of a response: the gateway header when
/// there is one, else what `note_body_model` kept, and nothing when the result
/// only repeats `model`.
pub fn settle_served_model(ext: &mut InferenceExtension, resp_headers: &HeaderMap) {
    // Header.Get: the first value.
    if let Some(v) = resp_headers.get(SERVED_MODEL_HEADER)
        && !v.is_empty()
    {
        ext.served_model = clean_served_model(&go_lossy_utf8(v.as_bytes()).text);
    }
    if ext.served_model == ext.model {
        ext.served_model.clear();
    }
}

/// The request's anthropic-beta header, every value joined with a comma, cleaned.
pub(crate) fn anthropic_beta(h: &HeaderMap) -> String {
    let joined: Vec<_> = h
        .get_all("anthropic-beta")
        .iter()
        .map(|v| go_lossy_utf8(v.as_bytes()).text.into_owned())
        .collect();
    clean_served_model(&joined.join(","))
}

/// Makes an upstream name safe to store and print: control runes replaced,
/// length capped on a rune boundary.
pub(crate) fn clean_served_model(s: &str) -> String {
    let s = sanitize_control_runes(s);
    // strings.ToValidUTF8(s[:256], "") drops the part of a rune that the cut split.
    s[..s.floor_char_boundary(MAX_SERVED_MODEL_LEN)].to_owned()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn clean_cuts_on_a_rune_boundary() {
        let s = format!("{}é", "a".repeat(255));
        assert_eq!(clean_served_model(&s), "a".repeat(255));
        assert_eq!(clean_served_model("m\u{1b}x"), "m\u{FFFD}x");
    }

    #[test]
    fn settle() {
        let mut h = HeaderMap::new();
        h.insert(SERVED_MODEL_HEADER, "gw/m".parse().unwrap());
        let mut ext = InferenceExtension {
            model: "m".into(),
            served_model: "m-2026".into(),
            ..Default::default()
        };
        settle_served_model(&mut ext, &h);
        assert_eq!(ext.served_model, "gw/m");
        let mut same = InferenceExtension {
            model: "m".into(),
            served_model: "m".into(),
            ..Default::default()
        };
        settle_served_model(&mut same, &HeaderMap::new());
        assert_eq!(same.served_model, "");
    }
}

//! Port of `model.ParseUserAgent` and `subagentHeader`.

use serde::Serialize;

use crate::parse::inference::text::{go_to_lower, is_control_rune, sanitize_control_runes};

/// The JSON of `model.EventClient`.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Client {
    #[serde(skip_serializing_if = "String::is_empty")]
    pub name: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub version: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub raw: String,
}

/// The header that names the Claude Code subagent of a request.
pub const CLAUDE_CODE_AGENT_ID_HEADER: &str = "X-Claude-Code-Agent-Id";
/// The header that names the parent of that subagent.
pub const CLAUDE_CODE_PARENT_AGENT_ID_HEADER: &str = "X-Claude-Code-Parent-Agent-Id";

/// `model.maxClientLen`: the byte cap on a User-Agent and on a subagent id.
const MAX_CLIENT_LEN: usize = 128;

/// `model.knownClients`: a product token, lowercased, to its canonical agent name.
fn known_client(product: &str) -> Option<&'static str> {
    Some(match go_to_lower(product).as_str() {
        "claude-cli" | "claude-code" => "claude-code",
        "bob-shell" | "bob" => "bob",
        "opencode" => "opencode",
        "codex_exec" | "codex-mcp-client" | "codex-tui" | "codex_cli_rs" => "codex",
        "pi" => "pi",
        _ => return None,
    })
}

/// `model.TruncateLabel`: at most `n` bytes, cut on a char boundary.
fn truncate_label(s: &str, n: usize) -> &str {
    if s.len() <= n {
        return s;
    }
    let mut cut = n;
    while !s.is_char_boundary(cut) {
        cut -= 1;
    }
    &s[..cut]
}

/// `model.productVersion`: OpenCode puts a release channel before its version.
fn product_version<'a>(name: &str, rest: &'a str) -> &'a str {
    if name != "opencode" {
        return rest;
    }
    rest.split('/').nth(1).unwrap_or(rest)
}

/// Port of `model.ParseUserAgent`. `None` means no User-Agent was sent.
pub fn parse_user_agent(ua: &str) -> Option<Client> {
    let ua = ua.trim();
    if ua.is_empty() {
        return None;
    }
    let ua = ua.replace('\t', " ");
    let sanitized = sanitize_control_runes(&ua);
    let ua = truncate_label(&sanitized, MAX_CLIENT_LEN);
    let mut c = Client {
        raw: ua.to_string(),
        ..Client::default()
    };
    let token = ua.split(' ').next().unwrap_or(ua);
    let (product, version) = token.split_once('/').unwrap_or((token, ""));
    if let Some(name) = known_client(product) {
        c.name = name.to_string();
        c.version = product_version(name, version).to_string();
        return Some(c);
    }
    // model.trailingKnownClient: the later tokens, last to first.
    let fields: Vec<&str> = ua.split_whitespace().collect();
    for field in fields.iter().skip(1).rev() {
        let (product, ver) = field.split_once('/').unwrap_or((field, ""));
        if let Some(name) = known_client(product) {
            c.name = name.to_string();
            c.version = product_version(name, ver).to_string();
            break;
        }
    }
    Some(c)
}

/// Port of `pipeline.subagentHeader`: trim SP and HTAB, then refuse an empty id, an id
/// over the cap, or one with a control rune.
pub fn subagent_header(v: Option<&str>) -> String {
    let id = v.unwrap_or("").trim_matches([' ', '\t']);
    if id.is_empty() || id.len() > MAX_CLIENT_LEN || id.chars().any(is_control_rune) {
        return String::new();
    }
    id.to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn user_agent_golden_matches_go() {
        let path = concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../../core/pipeline/testdata/user-agent.golden.json"
        );
        let golden: serde_json::Value =
            serde_json::from_str(&std::fs::read_to_string(path).unwrap()).unwrap();
        let uas = golden["userAgent"].as_array().unwrap();
        assert!(!uas.is_empty());
        for case in uas {
            let ua = case["ua"].as_str().unwrap();
            let got = serde_json::to_value(parse_user_agent(ua)).unwrap();
            assert_eq!(got, case["client"], "user agent {ua:?}");
        }
        let subs = golden["subagent"].as_array().unwrap();
        assert!(!subs.is_empty());
        for case in subs {
            let v = case["value"].as_str().unwrap();
            assert_eq!(subagent_header(Some(v)), case["id"], "header value {v:?}");
        }
        assert_eq!(subagent_header(None), "");
    }
}

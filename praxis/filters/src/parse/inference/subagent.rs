//! Port of core/parse/inference/subagent.go.

use super::text::go_to_lower;
use crate::parse::model::InferenceMessage;

/// What Claude Code writes as the first line of its system prompt.
const BILLING_HEADER_PREFIX: &str = "x-anthropic-billing-header:";
const SUBAGENT_MARKER: &str = "cc_is_subagent=true";

/// Which caller made a request: "main", "subagent", or "" when the request
/// says nothing. It reads the first line of the first system message only.
pub(crate) fn agent_role(msgs: &[InferenceMessage]) -> String {
    let Some(m) = msgs.iter().find(|m| m.role == "system") else {
        return String::new();
    };
    let line = m.content.split('\n').next().unwrap_or_default();
    let line = go_to_lower(line.trim());
    if !line.starts_with(BILLING_HEADER_PREFIX) {
        return String::new();
    }
    if line.contains(SUBAGENT_MARKER) {
        "subagent".into()
    } else {
        "main".into()
    }
}

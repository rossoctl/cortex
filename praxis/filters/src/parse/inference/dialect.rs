//! Port of core/parse/inference/dialect.go.

/// The wire format an inference endpoint speaks.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Dialect {
    /// Not an endpoint this parser reads.
    None,
    /// OpenAI chat completions, or legacy completions.
    OpenAI,
    /// Anthropic Messages.
    Anthropic,
    /// OpenAI Responses API.
    Responses,
}

const ANTHROPIC_MESSAGES_PATH: &str = "/v1/messages";
const COMPLETIONS_SUFFIX: &str = "/completions";
const RESPONSES_SUFFIX: &str = "/responses";

/// Which dialect a query-free path speaks, from how it ends.
pub(crate) fn dialect_for(path: &str) -> Dialect {
    if path.ends_with(ANTHROPIC_MESSAGES_PATH) {
        Dialect::Anthropic
    } else if path.ends_with(COMPLETIONS_SUFFIX) {
        Dialect::OpenAI
    } else if path.ends_with(RESPONSES_SUFFIX) {
        Dialect::Responses
    } else {
        Dialect::None
    }
}

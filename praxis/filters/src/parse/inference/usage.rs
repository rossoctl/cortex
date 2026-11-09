//! Port of core/internal/parsercommon/tokenusage.go.

use crate::parse::model::InferenceExtension;

/// A bitmask naming which sub-kinds the provider populated. A zero on a set
/// bit is "reported zero"; a zero on an unset bit is "not exposed".
pub(crate) type Kind = u8;

pub(crate) const KIND_INPUT: Kind = 1 << 0;
pub(crate) const KIND_CACHE_READ: Kind = 1 << 1;
pub(crate) const KIND_CACHE_WRITE: Kind = 1 << 2;
pub(crate) const KIND_OUTPUT: Kind = 1 << 3;
pub(crate) const KIND_REASONING: Kind = 1 << 4;

/// The provider-neutral token counts.
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub(crate) struct TokenUsage {
    pub(crate) input: i64,
    pub(crate) cache_read: i64,
    pub(crate) cache_write: i64,
    pub(crate) output: i64,
    /// Reasoning-only output, a subset of `output`.
    pub(crate) reasoning: i64,
    /// The provider's own total_tokens, else 0.
    pub(crate) reported_total: i64,
    pub(crate) present: Kind,
}

impl TokenUsage {
    pub(crate) fn prompt_total(&self) -> i64 {
        self.input + self.cache_read + self.cache_write
    }

    pub(crate) fn total(&self) -> i64 {
        self.prompt_total() + self.output
    }

    pub(crate) fn fill(&self, ext: &mut InferenceExtension) {
        ext.input_tokens = self.input;
        ext.cache_read_tokens = self.cache_read;
        ext.cache_write_tokens = self.cache_write;
        ext.output_tokens = self.output;
        ext.reasoning_tokens = self.reasoning;
        ext.present_kinds = self.present;

        ext.prompt_tokens = self.prompt_total();
        ext.completion_tokens = self.output;
        ext.total_tokens = if self.reported_total > 0 {
            self.reported_total
        } else {
            self.total()
        };
    }
}

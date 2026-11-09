//! Port of the Chat Completions half of core/parse/inference/inference.go.
//! Task 5 of the plan ports it. Until then each function claims nothing and
//! reads nothing.

use super::InferenceStreamState;
use crate::parse::model::InferenceExtension;

pub(super) fn parse_openai_request(_body: &[u8]) -> Option<InferenceExtension> {
    None
}

pub(super) fn parse_inference_json(_body: &[u8], _ext: &mut InferenceExtension) {}

pub(super) fn parse_inference_sse(_body: &[u8], _ext: &mut InferenceExtension) {}

pub(super) fn fold_openai_frame(
    _frame: &[u8],
    _state: &mut InferenceStreamState,
    _ext: &mut InferenceExtension,
) {
}

//! Port of core/parse/inference/responses.go.
//! Task 6 of the plan ports it. Until then each function claims nothing and
//! reads nothing.

use super::InferenceStreamState;
use crate::parse::model::InferenceExtension;

pub(super) fn parse_responses_request(_body: &[u8]) -> Option<InferenceExtension> {
    None
}

pub(super) fn parse_responses_json(_body: &[u8], _ext: &mut InferenceExtension) {}

pub(super) fn parse_responses_sse(_body: &[u8], _ext: &mut InferenceExtension) {}

pub(super) fn fold_responses_frame(
    _frame: &[u8],
    _state: &mut InferenceStreamState,
    _ext: &mut InferenceExtension,
) {
}

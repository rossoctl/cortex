//! The `cortex_attribution` filter.
//!
//! It refuses a request without the Cortex token, reads `X-Cortex-Attribution`
//! into [`TokenValid`], and removes the hop headers before the upstream. It runs
//! in the request-head phase, so a refused request has no body read.

use async_trait::async_trait;
use http::HeaderName;
use praxis_filter::{FilterAction, FilterError, HttpFilter, HttpFilterContext, Rejection};
use serde::Deserialize;

use crate::record::attribution::{
    ATTRIBUTION_HEADER, Attribution, TOKEN_HEADER, decode, token_matches, upstream_strip_headers,
};

/// Set on a request with a valid token, unless it is a probe. `cortex_observe`
/// records only requests that carry it.
pub(crate) struct TokenValid(pub Attribution);

pub const PROBE_HEADER: &str = "X-Cortex-Probe";

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Config {
    token_file: String,
}

/// Reads the token from `path`. Surrounding white space is not part of it.
pub(crate) fn read_token(path: &str) -> Result<String, FilterError> {
    let token = std::fs::read_to_string(path)
        .map_err(|e| format!("cannot read the token file {path}: {e}"))?;
    let token = token.trim().to_owned();
    if token.is_empty() {
        return Err(format!("the token file {path} is empty").into());
    }
    Ok(token)
}

pub struct CortexAttribution {
    token: String,
}

impl CortexAttribution {
    pub fn from_config(config: &serde_yaml::Value) -> Result<Box<dyn HttpFilter>, FilterError> {
        let cfg: Config = praxis_filter::parse_filter_config("cortex_attribution", config)?;
        Ok(Box::new(CortexAttribution {
            token: read_token(&cfg.token_file)?,
        }))
    }
}

#[async_trait]
impl HttpFilter for CortexAttribution {
    fn name(&self) -> &'static str {
        "cortex_attribution"
    }

    fn runs_request_head(&self) -> bool {
        true
    }

    async fn on_request_head(
        &self,
        ctx: &mut HttpFilterContext<'_>,
    ) -> Result<FilterAction, FilterError> {
        let headers = &ctx.request.headers;
        let given = headers.get(TOKEN_HEADER).map(|v| v.as_bytes());
        if !token_matches(given, self.token.as_bytes()) {
            return Ok(FilterAction::Reject(Rejection::status(403)));
        }
        if !headers.contains_key(PROBE_HEADER) {
            let attribution = decode(
                headers
                    .get(ATTRIBUTION_HEADER)
                    .and_then(|v| v.to_str().ok()),
            );
            ctx.extensions.insert(TokenValid(attribution));
        }
        ctx.request_headers_to_remove.extend(
            upstream_strip_headers().filter_map(|n| HeaderName::from_bytes(n.as_bytes()).ok()),
        );
        Ok(FilterAction::Continue)
    }

    async fn on_request(
        &self,
        _ctx: &mut HttpFilterContext<'_>,
    ) -> Result<FilterAction, FilterError> {
        Ok(FilterAction::Continue)
    }
}

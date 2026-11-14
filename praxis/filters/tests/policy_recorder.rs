//! The `cortex/recorder` sink, driven through the real `PolicyEngine`.
//!
//! `praxis-proxy-filter`'s request harness is `pub(crate)`, so this test loads
//! the policy document into an engine of its own and invokes the hook an
//! `llm:` route dispatches on.

use std::collections::HashMap;
use std::sync::Arc;

use cortex_praxis_filters::policy_recorder::{CortexRecorderFactory, JOIN_HEADER, KIND, take};
use praxis_policy::praxis_policy_core::cmf::enums::Role;
use praxis_policy::praxis_policy_core::cmf::{CmfHook, Message, MessagePayload};
use praxis_policy::praxis_policy_core::executor::PipelineResult;
use praxis_policy::praxis_policy_core::extensions::{HttpExtension, MetaExtension};
use praxis_policy::prelude::Extensions;
use praxis_policy::{AplOptions, PolicyEngine, register_apl};

/// `model-ok` is allowed; every other model is denied with `model_not_allowed`.
const POLICY: &str = r#"
plugins:
  - name: cortex-recorder
    kind: cortex/recorder
    capabilities: [read_headers]
routes:
  - llm: model-ok
    authorization:
      pre_invocation:
        - "llm.provider == 'never': deny('unreachable', 'never')"
  - llm: "*"
    authorization:
      pre_invocation:
        - "deny('model is not permitted', 'model_not_allowed')"
"#;

async fn engine() -> Arc<PolicyEngine> {
    let mgr = Arc::new(PolicyEngine::default());
    // As `PolicyFilter::new` does: the runtime's visitor reads `authorization:`.
    let _visitor = register_apl(&mgr, AplOptions::in_process());
    mgr.register_factory(KIND, Box::new(CortexRecorderFactory));
    mgr.load_config_yaml(POLICY).expect("the policy loads");
    mgr.initialize().await.expect("the engine initializes");
    mgr
}

fn extensions(entity_name: &str, header: Option<(&str, &str)>) -> Extensions {
    let mut request_headers = HashMap::new();
    if let Some((name, value)) = header {
        // The live path: the proxy strips the hop headers, so JOIN_HEADER is the key.
        request_headers.insert(name.to_owned(), value.to_owned());
    }
    Extensions {
        http: Some(Arc::new(HttpExtension {
            request_headers,
            ..Default::default()
        })),
        meta: Some(Arc::new(MetaExtension {
            entity_type: Some("llm".to_owned()),
            entity_name: Some(entity_name.to_owned()),
            ..Default::default()
        })),
        ..Default::default()
    }
}

/// Fire the hook an `llm:` route dispatches on, as `PolicyFilter` does.
async fn invoke(
    mgr: &Arc<PolicyEngine>,
    entity_name: &str,
    header: Option<(&str, &str)>,
) -> PipelineResult {
    let (result, _background) = mgr
        .invoke_named::<CmfHook>(
            "cmf.llm_input",
            MessagePayload {
                message: Message::text(Role::User, "hi"),
            },
            extensions(entity_name, header),
            None,
        )
        .await;
    result
}

#[tokio::test]
async fn a_deny_records_its_violation_code() {
    let mgr = engine().await;
    let result = invoke(&mgr, "model-bad", Some((JOIN_HEADER, "r1"))).await;
    assert!(!result.continue_processing, "model-bad is denied");
    let decision = take("r1").expect("the deny is recorded");
    assert_eq!(decision.action, "deny");
    assert_eq!(decision.reason, "model_not_allowed");
}

#[tokio::test]
async fn an_allow_records_an_empty_reason() {
    let mgr = engine().await;
    let result = invoke(&mgr, "model-ok", Some((JOIN_HEADER, "r2"))).await;
    assert!(result.continue_processing, "model-ok is allowed");
    let decision = take("r2").expect("the allow is recorded");
    assert_eq!(decision.action, "allow");
    assert_eq!(decision.reason, "");
}

#[tokio::test]
async fn an_absent_or_empty_id_records_nothing_and_the_name_compare_ignores_case() {
    let mgr = engine().await;
    // A sink that wrote without an id would key on the empty string.
    let _ = invoke(&mgr, "model-ok", None).await;
    let _ = invoke(&mgr, "model-ok", Some((JOIN_HEADER, ""))).await;
    assert!(take("").is_none(), "the sink wrote with no id");
    // The proxy lowercases names in production; the sink must not depend on that.
    let _ = invoke(
        &mgr,
        "model-bad",
        Some(("X-Praxis-Cortex-Request-Id", "r3")),
    )
    .await;
    assert_eq!(take("r3").expect("the sink found the name").action, "deny");
}

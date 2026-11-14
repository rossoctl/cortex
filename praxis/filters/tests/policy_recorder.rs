//! The `cortex/recorder` sink, driven through the real `PolicyEngine`.
//!
//! `praxis-proxy-filter`'s request harness is `pub(crate)`, so this test loads
//! the policy document into an engine of its own and invokes the hook an
//! `llm:` route dispatches on.

use std::collections::HashMap;
use std::sync::Arc;

use cortex_praxis_filters::policy_recorder::{
    CortexRecorderFactory, Decision, JOIN_HEADER, KIND, record, take,
};
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

fn extensions(entity_name: &str, request_id: Option<&str>) -> Extensions {
    let mut request_headers = HashMap::new();
    if let Some(id) = request_id {
        // The live path: the proxy strips the hop headers, so this is the key.
        request_headers.insert(JOIN_HEADER.to_owned(), id.to_owned());
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
    request_id: Option<&str>,
) -> PipelineResult {
    let (result, _background) = mgr
        .invoke_named::<CmfHook>(
            "cmf.llm_input",
            MessagePayload {
                message: Message::text(Role::User, "hi"),
            },
            extensions(entity_name, request_id),
            None,
        )
        .await;
    result
}

#[tokio::test]
async fn a_deny_records_its_violation_code() {
    let mgr = engine().await;
    let result = invoke(&mgr, "model-bad", Some("r1")).await;
    assert!(!result.continue_processing, "model-bad is denied");
    let decision = take("r1").expect("the deny is recorded");
    assert_eq!(decision.action, "deny");
    assert_eq!(decision.reason, "model_not_allowed");
}

#[tokio::test]
async fn an_allow_records_an_empty_reason() {
    let mgr = engine().await;
    let result = invoke(&mgr, "model-ok", Some("r2")).await;
    assert!(result.continue_processing, "model-ok is allowed");
    let decision = take("r2").expect("the allow is recorded");
    assert_eq!(decision.action, "allow");
    assert_eq!(decision.reason, "");
}

#[tokio::test]
async fn a_call_with_no_request_id_header_records_nothing() {
    let mgr = engine().await;
    // A handler that writes without the header would key on an empty id.
    record(
        "",
        Decision {
            action: "deny",
            reason: "sentinel".to_owned(),
        },
    );
    let _ = invoke(&mgr, "model-ok", None).await;
    let decision = take("").expect("the sentinel is still there");
    assert_eq!(decision.reason, "sentinel", "the sink wrote with no id");
}

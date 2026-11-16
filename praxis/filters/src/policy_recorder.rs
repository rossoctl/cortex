//! The `cortex/recorder` audit sink and the store the observe filter reads.
//!
//! The sink runs in the policy filter's engine and `cortex_observe` runs in the
//! same process, so a static store is what joins them by request id.

use std::collections::{HashMap, VecDeque};
use std::sync::{Arc, LazyLock, Mutex};

use async_trait::async_trait;
use praxis_policy::praxis_policy_core::audit::AuditHandler;
use praxis_policy::praxis_policy_core::decision::{DecisionLog, Verdict};
use praxis_policy::prelude::{
    Extensions, Plugin, PluginConfig, PluginError, PluginFactory, PluginInstance, PluginPayload,
};

use crate::record::line::{Invocation, Invocations, Line};

/// The `kind:` a policy file names to attach this sink.
pub const KIND: &str = "cortex/recorder";

/// The id in the reserved namespace. The proxy strips the hop headers before
/// the policy filter runs, so `cortex_observe` sets the id here, and the proxy
/// removes every `x-praxis-*` header before the upstream.
pub const JOIN_HEADER: &str = "x-praxis-cortex-request-id";

/// How many request ids the store keeps. A response row always takes its entry,
/// so the normal case never reaches the cap.
const CAPACITY: usize = 4096;

/// One recorded policy decision.
pub struct Decision {
    pub action: &'static str,
    pub reason: String,
}

impl Decision {
    /// The decision `log` records: a deny carries its violation code.
    fn from_log(log: &DecisionLog) -> Self {
        match log.verdict() {
            Some(Verdict::Deny(violation)) => Self {
                action: "deny",
                reason: violation.code.clone(),
            },
            _ => Self {
                action: "allow",
                reason: String::new(),
            },
        }
    }
}

#[derive(Default)]
struct Store {
    entries: HashMap<String, Decision>,
    /// Insertion order, for eviction.
    order: VecDeque<String>,
}

static STORE: LazyLock<Mutex<Store>> = LazyLock::new(|| Mutex::new(Store::default()));

fn lock() -> std::sync::MutexGuard<'static, Store> {
    STORE
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
}

/// Record `decision` for `request_id`. A later call replaces an earlier one:
/// the last verdict is the one the caller got.
///
/// shortcut: the id must be unique per request, add a per-request token if
/// clients reuse `X-Cortex-Request-Id` while two requests are open.
pub(crate) fn record(request_id: &str, decision: Decision) {
    let mut store = lock();
    if store
        .entries
        .insert(request_id.to_owned(), decision)
        .is_none()
    {
        store.order.push_back(request_id.to_owned());
    }
    while store.order.len() > CAPACITY {
        if let Some(old) = store.order.pop_front() {
            store.entries.remove(&old);
        }
    }
}

/// Take the decision for `request_id` out of the store.
pub fn take(request_id: &str) -> Option<Decision> {
    let mut store = lock();
    let decision = store.entries.remove(request_id)?;
    store.order.retain(|id| id != request_id);
    Some(decision)
}

/// Put the recorded decision for `line`'s request in its response row.
pub(crate) fn attach(line: &mut Line) {
    if line.event.phase != "response" {
        return;
    }
    if let Some(decision) = take(&line.request_id) {
        line.event.invocations = Some(Invocations {
            outbound: vec![Invocation {
                plugin: "policy".into(),
                action: decision.action.into(),
                reason: decision.reason,
            }],
        });
    }
}

/// The sink. A policy file declares it with `kind: cortex/recorder` and the
/// `read_headers` capability.
pub struct CortexRecorder {
    config: PluginConfig,
}

#[async_trait]
impl Plugin for CortexRecorder {
    fn config(&self) -> &PluginConfig {
        &self.config
    }

    fn as_audit_handler(self: Arc<Self>) -> Option<Arc<dyn AuditHandler>> {
        Some(self)
    }
}

#[async_trait]
impl AuditHandler for CortexRecorder {
    async fn handle(&self, _payload: &dyn PluginPayload, ext: &Extensions, log: &DecisionLog) {
        let Some(request_id) = request_id(ext) else {
            return;
        };
        record(request_id, Decision::from_log(log));
    }
}

/// The join header's value, found with a case-insensitive name compare.
fn request_id(ext: &Extensions) -> Option<&str> {
    let headers = &ext.http.as_ref()?.request_headers;
    headers
        .iter()
        .find(|(name, _)| name.eq_ignore_ascii_case(JOIN_HEADER))
        .map(|(_, value)| value.as_str())
        .filter(|value| !value.is_empty())
}

pub struct CortexRecorderFactory;

impl PluginFactory for CortexRecorderFactory {
    fn create(&self, config: &PluginConfig) -> Result<PluginInstance, Box<PluginError>> {
        Ok(PluginInstance {
            plugin: Arc::new(CortexRecorder {
                config: config.clone(),
            }),
            // A sink declares no hooks.
            handlers: Vec::new(),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::record::line::Event;

    /// The store is process-wide, so the cap test must not evict a sibling's id.
    static TESTS: Mutex<()> = Mutex::new(());

    fn decision(action: &'static str, reason: &str) -> Decision {
        Decision {
            action,
            reason: reason.to_owned(),
        }
    }

    fn guard() -> std::sync::MutexGuard<'static, ()> {
        TESTS
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    fn line(phase: &'static str, request_id: &str) -> Line {
        Line {
            request_id: request_id.to_owned(),
            started_at: "2026-11-13T10:00:00.000000000Z".into(),
            session: "s".into(),
            event: Event {
                direction: "outbound",
                phase,
                request_id: request_id.to_owned(),
                ..Default::default()
            },
            capture: None,
            cost: None,
        }
    }

    #[test]
    fn record_then_take_gives_the_decision_back() {
        let _guard = TESTS
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        record("t-record", decision("deny", "code"));
        let taken = take("t-record").expect("the entry is there");
        assert_eq!(taken.action, "deny");
        assert_eq!(taken.reason, "code");
    }

    #[test]
    fn a_deny_replaces_an_allow() {
        let _guard = TESTS
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        record("t-replace", decision("allow", ""));
        record("t-replace", decision("deny", "code"));
        assert_eq!(
            take("t-replace").expect("the entry is there").action,
            "deny"
        );
    }

    #[test]
    fn a_take_empties_the_entry() {
        let _guard = TESTS
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        record("t-empty", decision("allow", ""));
        assert!(take("t-empty").is_some());
        assert!(take("t-empty").is_none());
    }

    #[test]
    fn the_oldest_id_is_evicted_at_the_cap() {
        let _guard = TESTS
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        // Push past the cap with ids no other test uses.
        for i in 0..=CAPACITY {
            record(&format!("t-cap-{i}"), decision("allow", ""));
        }
        assert!(
            take("t-cap-0").is_none(),
            "the first id is evicted at the cap"
        );
        assert!(
            take(&format!("t-cap-{CAPACITY}")).is_some(),
            "the newest id is kept"
        );
        // Leave the store as this test found it.
        for i in 0..=CAPACITY {
            take(&format!("t-cap-{i}"));
        }
    }

    #[test]
    fn attach_puts_a_deny_in_the_response_row() {
        let _guard = guard();
        record("t-attach-deny", decision("deny", "model_not_allowed"));
        let mut l = line("response", "t-attach-deny");
        attach(&mut l);
        let inv = l
            .event
            .invocations
            .as_ref()
            .expect("the row carries the decision");
        assert_eq!(inv.outbound.len(), 1);
        assert_eq!(inv.outbound[0].plugin, "policy");
        assert_eq!(inv.outbound[0].action, "deny");
        assert_eq!(inv.outbound[0].reason, "model_not_allowed");
        assert!(take("t-attach-deny").is_none(), "attach takes the entry");
    }

    #[test]
    fn attach_leaves_a_response_row_alone_with_no_decision() {
        let _guard = guard();
        let mut l = line("response", "t-attach-none");
        attach(&mut l);
        assert!(l.event.invocations.is_none());
    }

    #[test]
    fn attach_leaves_a_request_row_and_its_entry_alone() {
        let _guard = guard();
        record("t-attach-request", decision("allow", ""));
        let mut l = line("request", "t-attach-request");
        attach(&mut l);
        assert!(l.event.invocations.is_none());
        assert!(
            take("t-attach-request").is_some(),
            "the entry stays for the response row"
        );
    }

    #[test]
    fn an_allow_entry_serializes_without_a_reason() {
        let _guard = guard();
        record("t-attach-allow", decision("allow", ""));
        let mut l = line("response", "t-attach-allow");
        attach(&mut l);
        let v = serde_json::to_value(&l.event).unwrap();
        assert_eq!(
            v["invocations"],
            serde_json::json!({"outbound": [{"plugin": "policy", "action": "allow"}]})
        );
    }
}

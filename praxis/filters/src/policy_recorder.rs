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

/// The `kind:` a policy file names to attach this sink.
pub const KIND: &str = "cortex/recorder";

/// The join header that `cortex_observe` sets on the hop.
const REQUEST_ID_HEADER: &str = "x-cortex-request-id";

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
pub fn record(request_id: &str, decision: Decision) {
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
        .find(|(name, _)| name.eq_ignore_ascii_case(REQUEST_ID_HEADER))
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

    /// The store is process-wide, so the cap test must not evict a sibling's id.
    static TESTS: Mutex<()> = Mutex::new(());

    fn decision(action: &'static str, reason: &str) -> Decision {
        Decision {
            action,
            reason: reason.to_owned(),
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
}

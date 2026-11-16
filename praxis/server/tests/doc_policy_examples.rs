//! Every policy example in `docs/praxis-policy.md` loads in the gateway's build.
//!
//! `PolicyFilter::from_config` is the gateway's own construction path, so this test
//! installs the same builtin plugin factories and reads the same kind of file.

use std::path::PathBuf;
use std::sync::Arc;

use cortex_praxis_filters::policy_recorder::{CortexRecorderFactory, KIND};

/// The fence that marks a load-tested policy example. Every other YAML block in the
/// page uses a plain ```yaml fence and is not loaded.
const FENCE: &str = "```yaml policy\n";

/// The number of load-tested examples the page must carry.
const EXAMPLES: usize = 5;

/// The bodies of every ```yaml policy fence in `doc`.
fn policy_examples(doc: &str) -> Vec<String> {
    let mut out = Vec::new();
    let mut rest = doc;
    while let Some(i) = rest.find(FENCE) {
        rest = &rest[i + FENCE.len()..];
        let Some(j) = rest.find("\n```") else {
            panic!("docs/praxis-policy.md: unterminated ```yaml policy fence");
        };
        out.push(rest[..j].to_owned());
        rest = &rest[j..];
    }
    out
}

#[test]
fn every_documented_policy_loads() {
    let path = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../docs/praxis-policy.md");
    let doc = std::fs::read_to_string(&path).expect("read docs/praxis-policy.md");
    let blocks = policy_examples(&doc);
    assert_eq!(
        blocks.len(),
        EXAMPLES,
        "docs/praxis-policy.md must carry {EXAMPLES} ```yaml policy examples"
    );

    // As main.rs does before the server starts: the factory must be registered first.
    praxis_filter::register_policy_plugin_factory(
        KIND,
        Arc::new(|| Box::new(CortexRecorderFactory)),
    );
    let dir = std::env::temp_dir().join(format!("cortex-doc-policy-{}", std::process::id()));
    std::fs::create_dir_all(&dir).expect("temp dir");
    for (i, block) in blocks.iter().enumerate() {
        let file = dir.join(format!("example-{i}.yaml"));
        std::fs::write(&file, block).expect("write example");
        let config: serde_yaml::Value = serde_yaml::from_str(&format!(
            "config_path: {:?}\n",
            file.to_str().expect("utf-8 path")
        ))
        .expect("filter config");
        let _filter = praxis_filter::PolicyFilter::from_config(&config)
            .unwrap_or_else(|e| panic!("example {i} does not load: {e}\n{block}"));
    }
}

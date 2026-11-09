//! `cortex-gateway`: the Praxis server with the Cortex filters registered.
//!
//! The shape follows Praxis's own `crates/server/src/main.rs` at v0.8.0.

praxis_filter::register_filters! {}

fn main() {
    // Before anything that can build a TLS config.
    praxis::install_crypto_provider();

    let explicit = config_arg(std::env::args().skip(1)).or_else(|| {
        std::env::var("PRAXIS_CONFIG")
            .ok()
            .filter(|v| !v.is_empty())
    });
    let config_path = praxis::resolve_config_path(explicit.as_deref());
    let config = praxis::load_config(config_path.as_deref().and_then(|p| p.to_str()))
        .unwrap_or_else(|e| praxis::fatal(&e));
    let tracing_guard = praxis::init_tracing(&config).unwrap_or_else(|e| praxis::fatal(&e));
    let log_level = Some(tracing_guard.log_level_state());
    let _tracing_guard = tracing_guard;
    praxis::run_server_with_registry(config, custom_registry(), config_path, log_level)
}

/// The value of `-c <path>` or `--config <path>`.
fn config_arg(mut args: impl Iterator<Item = String>) -> Option<String> {
    while let Some(arg) = args.next() {
        if arg == "-c" || arg == "--config" {
            return args.next();
        }
    }
    None
}

#[cfg(test)]
mod tests {
    use super::config_arg;

    fn args(list: &[&str]) -> impl Iterator<Item = String> {
        list.iter()
            .map(|s| (*s).to_owned())
            .collect::<Vec<_>>()
            .into_iter()
    }

    #[test]
    fn config_arg_reads_short_and_long_flags() {
        assert_eq!(
            config_arg(args(&["-c", "a.yaml"])).as_deref(),
            Some("a.yaml")
        );
        assert_eq!(
            config_arg(args(&["--config", "b.yaml"])).as_deref(),
            Some("b.yaml")
        );
        assert_eq!(config_arg(args(&[])), None);
        assert_eq!(config_arg(args(&["-c"])), None);
    }
}

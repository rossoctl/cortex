//! `cortex-gateway`: the Praxis server with the Cortex filters registered.
//!
//! The shape follows Praxis's own `crates/server/src/main.rs` at v0.8.0.

use cortex_praxis_filters::filters::attribution::CortexAttribution;
use cortex_praxis_filters::filters::observe::CortexObserve;

praxis_filter::register_filters! {
    http "cortex_attribution" => CortexAttribution::from_config,
    http "cortex_observe" => CortexObserve::from_config,
}

/// Exit when stdin closes. The supervisor holds the write end of a pipe, so
/// the gateway exits when the supervisor exits for any reason, SIGKILL included.
const EXIT_ON_STDIN_EOF: &str = "--exit-on-stdin-eof";

/// The CA file the TLS connector reads on macOS when the env names none. The
/// upstream is always the Cortex egress listener over plain HTTP, so the
/// connector needs no roots. Without it, Pingora reads the macOS trust store at
/// startup, which fails on a loaded host, and Pingora panics.
#[cfg(any(target_os = "macos", test))]
const NO_ROOTS: &str = "/dev/null";

/// The `SSL_CERT_FILE` to set, given the env's value: `NO_ROOTS` when the env
/// sets none or an empty one, else nothing, so the env's file is kept.
#[cfg(any(target_os = "macos", test))]
fn ca_file_default(env: Option<std::ffi::OsString>) -> Option<&'static str> {
    env.filter(|v| !v.is_empty()).is_none().then_some(NO_ROOTS)
}

fn main() {
    #[cfg(target_os = "macos")]
    if let Some(file) = ca_file_default(std::env::var_os("SSL_CERT_FILE")) {
        // SAFETY: the first statement of main, so no other thread exists yet.
        unsafe { std::env::set_var("SSL_CERT_FILE", file) };
    }
    // The supervisor reads this before each start. No config is loaded.
    if has_flag(std::env::args().skip(1), "--version") {
        println!("{}", version_line());
        return;
    }
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
    if has_flag(std::env::args().skip(1), EXIT_ON_STDIN_EOF) {
        std::thread::spawn(|| {
            // EOF or a read error: either way the supervisor is gone.
            let _ = std::io::copy(&mut std::io::stdin().lock(), &mut std::io::sink());
            std::process::exit(0);
        });
    }
    praxis::run_server_with_registry(config, custom_registry(), config_path, log_level)
}

/// `cortex-gateway <version>`. The release build sets `CORTEX_VERSION` to the
/// cortex version. A local build reads `dev`, as a local cortex build does.
fn version_line() -> String {
    format!(
        "cortex-gateway {}",
        option_env!("CORTEX_VERSION").unwrap_or("dev")
    )
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

/// True when `flag` is one of the arguments.
fn has_flag(mut args: impl Iterator<Item = String>, flag: &str) -> bool {
    args.any(|a| a == flag)
}

#[cfg(test)]
mod tests {
    use super::{EXIT_ON_STDIN_EOF, NO_ROOTS, ca_file_default, config_arg, has_flag, version_line};

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

    #[test]
    fn exit_flag_is_read() {
        let before = ["--exit-on-stdin-eof", "-c", "a.yaml"];
        let after = ["-c", "a.yaml", "--exit-on-stdin-eof"];
        for list in [before, after] {
            assert!(has_flag(args(&list), EXIT_ON_STDIN_EOF));
            assert_eq!(config_arg(args(&list)).as_deref(), Some("a.yaml"));
        }
        assert!(!has_flag(args(&["-c", "a.yaml"]), EXIT_ON_STDIN_EOF));
    }

    #[test]
    fn ca_file_default_fills_only_an_unset_env() {
        assert_eq!(ca_file_default(None), Some(NO_ROOTS));
        assert_eq!(ca_file_default(Some("".into())), Some(NO_ROOTS));
        assert_eq!(ca_file_default(Some("/etc/ca.pem".into())), None);
    }
    #[test]
    fn version_line_names_the_binary() {
        let line = version_line();
        assert!(line.starts_with("cortex-gateway "), "{line}");
        assert_eq!(
            line.split_whitespace().last(),
            Some(option_env!("CORTEX_VERSION").unwrap_or("dev"))
        );
    }
}

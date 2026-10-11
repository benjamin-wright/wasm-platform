/// Keeps platform diagnostics while excluding dependency events that can format
/// authentication payloads, URLs, or response bodies at verbose log levels.
pub fn is_platform_log(metadata: &tracing::Metadata<'_>) -> bool {
    matches!(
        metadata.target().split("::").next(),
        Some("execution_host" | "gateway" | "module_cache" | "platform_common")
    )
}

/// Reports panic location without formatting a potentially secret-bearing payload.
pub fn install_panic_hook() {
    std::panic::set_hook(Box::new(|info| {
        let location = info.location();
        tracing::error!(
            file = location.map(|location| location.file()),
            line = location.map(|location| location.line()),
            "process panicked"
        );
    }));
}

use crate::runtime::{HostState, message_bindings};

impl message_bindings::framework::runtime::log::Host for HostState {
    fn emit(&mut self, level: message_bindings::framework::runtime::log::Level, message: String) {
        use message_bindings::framework::runtime::log::Level;
        let app_name = &self.app_name;
        let app_namespace = &self.app_namespace;
        match level {
            Level::Debug => tracing::debug!(app_name, app_namespace, "{message}"),
            Level::Info => tracing::info!(app_name, app_namespace, "{message}"),
            Level::Warn => tracing::warn!(app_name, app_namespace, "{message}"),
            Level::Error => tracing::error!(app_name, app_namespace, "{message}"),
        }
    }
}

use crate::runtime::{HostState, message_bindings};

impl message_bindings::framework::runtime::messaging::Host for HostState {
    fn send(&mut self, topic: String, payload: Vec<u8>) -> Result<(), String> {
        let Some(ref client) = self.nats_client else {
            return Err("messaging host function unavailable: NATS not connected".to_string());
        };
        let subject = format!("fn.{topic}");
        // We are inside spawn_blocking, so a Tokio runtime handle is available.
        let result = tokio::runtime::Handle::current()
            .block_on(client.publish(subject, payload.into()))
            .map_err(|e| format!("NATS publish failed: {e}"));
        if result.is_ok() {
            self.metrics_registry
                .record_message_sent(&self.app_name, &self.app_namespace);
        }
        result
    }
}

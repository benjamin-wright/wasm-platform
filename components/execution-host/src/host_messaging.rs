use crate::runtime::{HostState, message_bindings};

impl message_bindings::framework::runtime::messaging::Host for HostState {
    async fn send(&mut self, topic: String, payload: Vec<u8>) -> Result<(), String> {
        self.invocation.check()?;
        let Some(ref client) = self.nats_client else {
            return Err("messaging host function unavailable: NATS not connected".to_string());
        };
        let subject = format!("fn.{topic}");
        let result = self
            .invocation
            .run(client.publish(subject, payload.into()))
            .await?
            .map_err(|_| "NATS publish failed".to_string());
        if result.is_ok() {
            self.metrics_registry
                .record_message_sent(&self.app_name, &self.app_namespace);
        }
        result
    }
}

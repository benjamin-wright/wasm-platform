use crate::{
    metrics::DropReason,
    runtime::{HostState, message_bindings},
};

impl message_bindings::framework::runtime::metrics::Host for HostState {
    fn counter_increment(
        &mut self,
        name: String,
        labels: Vec<(String, String)>,
    ) -> Result<(), String> {
        match self.metrics_registry.counter_increment(
            &name,
            &self.app_name,
            &self.app_namespace,
            &labels,
        ) {
            Ok(()) => Ok(()),
            Err(reason) => {
                tracing::error!(
                    app_name = %self.app_name,
                    app_namespace = %self.app_namespace,
                    function_name = %self.function_name,
                    metric_name = %name,
                    reason = reason.as_str(),
                    ?labels,
                    "guest metric call dropped",
                );
                self.metrics_registry
                    .record_dropped_metric(&self.app_name, &self.app_namespace, reason);
                // Silent drop from the guest's perspective.
                Ok(())
            }
        }
    }

    fn gauge_set(
        &mut self,
        name: String,
        value: f64,
        labels: Vec<(String, String)>,
    ) -> Result<(), String> {
        match self.metrics_registry.gauge_set(
            &name,
            value,
            &self.app_name,
            &self.app_namespace,
            &labels,
        ) {
            Ok(()) => Ok(()),
            Err(reason) => {
                tracing::error!(
                    app_name = %self.app_name,
                    app_namespace = %self.app_namespace,
                    function_name = %self.function_name,
                    metric_name = %name,
                    reason = reason.as_str(),
                    ?labels,
                    "guest metric call dropped",
                );
                self.metrics_registry
                    .record_dropped_metric(&self.app_name, &self.app_namespace, reason);
                // Silent drop from the guest's perspective.
                Ok(())
            }
        }
    }
}

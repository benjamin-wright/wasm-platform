use std::{
    collections::HashMap,
    sync::{Arc, RwLock},
};

use anyhow::Result;

pub mod configsync {
    tonic::include_proto!("configsync.v1");
}

pub use configsync::{AppUpdate, ApplicationConfig, FullConfig};

/// A flattened view of a single deployable function, combining both application-level
/// (shared env, sql, key_value, metrics) and function-level (module_ref, world_type, topic)
/// fields.  The registry maps each NATS subject (topic) to one FunctionEntry so that
/// message dispatch is a single O(1) map lookup.
#[derive(Clone, Debug)]
pub struct FunctionEntry {
    pub app_name: String,
    pub app_namespace: String,
    pub function_name: String,
    pub module_ref: String,
    pub world_type: configsync::WorldType,
    pub http_config: Option<configsync::HttpConfig>,
    /// Application-level shared config.
    pub env: HashMap<String, String>,
    pub sql: Option<configsync::SqlConfig>,
    pub key_value: Option<configsync::KeyValueConfig>,
    /// User-defined Prometheus metrics declared by the application.
    pub metrics: Vec<configsync::MetricDefinition>,
}

/// The result of applying a config update.
///
/// Load tuples: `(namespace, app_name, function_name, module_ref)` — modules to load.
/// Evict tuples: `(namespace, app_name, function_name)` — modules to remove.
pub type ConfigDiff = (
    Vec<(String, String, String, String)>,
    Vec<(String, String, String)>,
);

/// Shared, thread-safe registry of all known function entries, keyed by NATS topic.
///
/// Topic is the authoritative dispatch key: the operator enforces global uniqueness so
/// no two functions share a topic.  All lookups on the message-handling hot path are
/// therefore a single O(1) map access.
#[derive(Clone, Default)]
pub struct AppRegistry {
    inner: Arc<RwLock<HashMap<String, FunctionEntry>>>,
}

impl AppRegistry {
    pub fn new() -> Self {
        Self::default()
    }

    /// Replace the entire registry with the contents of a full-config snapshot.
    /// Returns `(to_load, to_evict)` describing the changes.
    pub fn apply_full_config(&self, full: FullConfig) -> Result<ConfigDiff> {
        let mut map = self
            .inner
            .write()
            .map_err(|_| anyhow::anyhow!("AppRegistry lock poisoned"))?;

        let mut incoming: HashMap<String, FunctionEntry> = HashMap::new();
        for app in &full.applications {
            let metric_count = app.metrics.len();
            if metric_count > 0 {
                tracing::info!(
                    app_name = %app.name,
                    namespace = %app.namespace,
                    metric_count,
                    "received metric definitions",
                );
            }
            for fn_cfg in &app.functions {
                if let Some(topic) = &fn_cfg.topic {
                    let entry = function_entry_from(app, fn_cfg);
                    incoming.insert(topic.clone(), entry);
                }
            }
        }

        let evicted: Vec<(String, String, String)> = map
            .iter()
            .filter(|(topic, _)| !incoming.contains_key(topic.as_str()))
            .map(|(_, e)| (e.app_namespace.clone(), e.app_name.clone(), e.function_name.clone()))
            .collect();

        map.clear();
        let mut to_load = Vec::with_capacity(incoming.len());
        for (topic, entry) in incoming {
            to_load.push((
                entry.app_namespace.clone(),
                entry.app_name.clone(),
                entry.function_name.clone(),
                entry.module_ref.clone(),
            ));
            map.insert(topic, entry);
        }

        Ok((to_load, evicted))
    }

    /// Apply a list of incremental updates: upsert or delete each application's functions.
    /// Returns `(to_load, to_evict)` describing the changes.
    ///
    /// On upsert, all existing entries for the application are replaced with the new
    /// function list.  This handles both additions and removals of individual functions.
    pub fn apply_incremental(&self, updates: Vec<AppUpdate>) -> Result<ConfigDiff> {
        let mut map = self
            .inner
            .write()
            .map_err(|_| anyhow::anyhow!("AppRegistry lock poisoned"))?;

        let mut to_load: Vec<(String, String, String, String)> = Vec::new();
        let mut to_evict: Vec<(String, String, String)> = Vec::new();

        for update in updates {
            let Some(app) = update.app_config else {
                if update.delete {
                    tracing::warn!("received delete update with no app_config; ignoring");
                }
                continue;
            };

            if update.delete {
                let removed: Vec<String> = map
                    .iter()
                    .filter(|(_, e)| {
                        e.app_namespace == app.namespace && e.app_name == app.name
                    })
                    .map(|(topic, _)| topic.clone())
                    .collect();
                for topic in removed {
                    if let Some(old) = map.remove(&topic) {
                        to_evict.push((old.app_namespace, old.app_name, old.function_name));
                    }
                }
            } else {
                let metric_count = app.metrics.len();
                if metric_count > 0 {
                    tracing::info!(
                        app_name = %app.name,
                        namespace = %app.namespace,
                        metric_count,
                        "received metric definitions",
                    );
                }
                let old_topics: Vec<String> = map
                    .iter()
                    .filter(|(_, e)| {
                        e.app_namespace == app.namespace && e.app_name == app.name
                    })
                    .map(|(topic, _)| topic.clone())
                    .collect();
                for topic in old_topics {
                    if let Some(old) = map.remove(&topic) {
                        to_evict.push((old.app_namespace, old.app_name, old.function_name));
                    }
                }

                for fn_cfg in &app.functions {
                    if let Some(topic) = &fn_cfg.topic {
                        if let Some(old) = map.get(topic.as_str()) {
                            if (old.app_namespace.as_str(), old.app_name.as_str())
                                != (app.namespace.as_str(), app.name.as_str())
                            {
                                to_evict.push((
                                    old.app_namespace.clone(),
                                    old.app_name.clone(),
                                    old.function_name.clone(),
                                ));
                            }
                        }
                    }
                }

                for fn_cfg in &app.functions {
                    if let Some(topic) = &fn_cfg.topic {
                        let entry = function_entry_from(&app, fn_cfg);
                        to_load.push((
                            entry.app_namespace.clone(),
                            entry.app_name.clone(),
                            entry.function_name.clone(),
                            entry.module_ref.clone(),
                        ));
                        map.insert(topic.clone(), entry);
                    }
                }
            }
        }

        Ok((to_load, to_evict))
    }

    /// Returns the NATS topic for every function currently in the registry.
    pub fn topics(&self) -> Result<Vec<String>> {
        let map = self
            .inner
            .read()
            .map_err(|_| anyhow::anyhow!("AppRegistry lock poisoned"))?;
        Ok(map.keys().cloned().collect())
    }

    /// Returns the function entry for the given NATS topic, or `None` if not found.
    pub fn get_by_topic(&self, topic: &str) -> Result<Option<FunctionEntry>> {
        let map = self
            .inner
            .read()
            .map_err(|_| anyhow::anyhow!("AppRegistry lock poisoned"))?;
        Ok(map.get(topic).cloned())
    }
}

// Constructs a FunctionEntry by combining an ApplicationConfig and a FunctionConfig.
fn function_entry_from(
    app: &ApplicationConfig,
    fn_cfg: &configsync::FunctionConfig,
) -> FunctionEntry {
    let world_type = configsync::WorldType::try_from(fn_cfg.world_type)
        .unwrap_or(configsync::WorldType::Message);
    FunctionEntry {
        app_name: app.name.clone(),
        app_namespace: app.namespace.clone(),
        function_name: fn_cfg.name.clone(),
        module_ref: fn_cfg.module_ref.clone(),
        world_type,
        http_config: fn_cfg.http_config.clone(),
        env: app.env.clone(),
        sql: app.sql.clone(),
        key_value: app.key_value.clone(),
        metrics: app.metrics.clone(),
    }
}

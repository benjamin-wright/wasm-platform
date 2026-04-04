use std::{
    collections::HashMap,
    sync::{Arc, RwLock},
};

use anyhow::Result;

pub mod configsync {
    tonic::include_proto!("configsync.v1");
}

pub use configsync::{AppUpdate, ApplicationConfig, FullConfig};

/// The result of applying a config update: apps to (re)load and keys to evict.
type ConfigDiff = (Vec<ApplicationConfig>, Vec<(String, String)>);

/// Shared, thread-safe registry of all known `ApplicationConfig` entries,
/// keyed by topic.
///
/// Topic is the authoritative identity: the operator enforces global uniqueness
/// so no two applications can share a topic. All lookups on the message-handling
/// hot path are therefore a single O(1) map access.
#[derive(Clone, Default)]
pub struct AppRegistry {
    inner: Arc<RwLock<HashMap<String, ApplicationConfig>>>,
}

impl AppRegistry {
    pub fn new() -> Self {
        Self::default()
    }

    /// Replace the entire registry with the contents of a full-config snapshot.
    /// Returns the new/changed `ApplicationConfig` entries (to trigger module
    /// loading) and the `(namespace, name)` pairs that were evicted (to remove
    /// cached modules).
    pub fn apply_full_config(&self, full: FullConfig) -> Result<ConfigDiff> {
        let mut map = self
            .inner
            .write()
            .map_err(|_| anyhow::anyhow!("AppRegistry lock poisoned"))?;

        let incoming: std::collections::HashSet<&str> =
            full.applications.iter().map(|a| a.topic.as_str()).collect();

        let deleted: Vec<(String, String)> = map
            .iter()
            .filter(|(topic, _)| !incoming.contains(topic.as_str()))
            .map(|(_, app)| (app.namespace.clone(), app.name.clone()))
            .collect();

        map.clear();
        let mut upserted = Vec::with_capacity(full.applications.len());
        for app in full.applications {
            upserted.push(app.clone());
            map.insert(app.topic.clone(), app);
        }

        Ok((upserted, deleted))
    }

    /// Apply a list of incremental updates: upsert or delete each entry.
    /// Returns the upserted `ApplicationConfig` entries and evicted
    /// `(namespace, name)` pairs.
    ///
    /// If an upsert arrives for a topic already held by a *different*
    /// `(namespace, name)`, the displaced entry is added to the eviction list —
    /// the operator is authoritative and has resolved the conflict.
    pub fn apply_incremental(&self, updates: Vec<AppUpdate>) -> Result<ConfigDiff> {
        let mut map = self
            .inner
            .write()
            .map_err(|_| anyhow::anyhow!("AppRegistry lock poisoned"))?;
        let mut upserted: Vec<ApplicationConfig> = Vec::new();
        let mut deleted: Vec<(String, String)> = Vec::new();
        for update in updates {
            if let Some(app) = update.app_config {
                if update.delete {
                    if let Some(old) = map.remove(&app.topic) {
                        deleted.push((old.namespace, old.name));
                    }
                } else {
                    if let Some(old) = map.get(&app.topic)
                        && (old.namespace.as_str(), old.name.as_str())
                            != (app.namespace.as_str(), app.name.as_str())
                    {
                        deleted.push((old.namespace.clone(), old.name.clone()));
                    }
                    upserted.push(app.clone());
                    map.insert(app.topic.clone(), app);
                }
            } else if update.delete {
                tracing::warn!("received delete update with no app_config; ignoring");
            }
        }
        Ok((upserted, deleted))
    }

    /// Returns the NATS topic for every application currently in the registry.
    pub fn topics(&self) -> Result<Vec<String>> {
        let map = self
            .inner
            .read()
            .map_err(|_| anyhow::anyhow!("AppRegistry lock poisoned"))?;
        Ok(map.keys().cloned().collect())
    }

    /// Returns the config for the application subscribed to the given topic,
    /// or `None` if no application matches.
    pub fn get_by_topic(&self, topic: &str) -> Result<Option<ApplicationConfig>> {
        let map = self
            .inner
            .read()
            .map_err(|_| anyhow::anyhow!("AppRegistry lock poisoned"))?;
        Ok(map.get(topic).cloned())
    }
}

use std::{
    collections::HashMap,
    sync::{Arc, RwLock},
};

use anyhow::Result;

pub mod configsync {
    tonic::include_proto!("configsync.v1");
}

pub use configsync::{AppUpdate, ApplicationConfig, FullConfig};

/// Shared, thread-safe registry of all known `ApplicationConfig` entries,
/// keyed by `(namespace, name)`.
#[derive(Clone, Default)]
pub struct AppRegistry {
    inner: Arc<RwLock<HashMap<(String, String), ApplicationConfig>>>,
}

impl AppRegistry {
    pub fn new() -> Self {
        Self::default()
    }

    /// Replace the entire registry with the contents of a full-config snapshot.
    pub fn apply_full_config(&self, full: FullConfig) -> Result<()> {
        let mut map = self
            .inner
            .write()
            .map_err(|_| anyhow::anyhow!("AppRegistry lock poisoned"))?;
        map.clear();
        for app in full.applications {
            map.insert((app.namespace.clone(), app.name.clone()), app);
        }
        Ok(())
    }

    /// Apply a list of incremental updates: upsert or delete each entry.
    pub fn apply_incremental(&self, updates: Vec<AppUpdate>) -> Result<()> {
        let mut map = self
            .inner
            .write()
            .map_err(|_| anyhow::anyhow!("AppRegistry lock poisoned"))?;
        for update in updates {
            if let Some(app) = update.app_config {
                let key = (app.namespace.clone(), app.name.clone());
                if update.delete {
                    map.remove(&key);
                } else {
                    map.insert(key, app);
                }
            } else if update.delete {
                tracing::warn!("received delete update with no app_config; ignoring");
            }
        }
        Ok(())
    }

    /// Returns the NATS topic for every application currently in the registry.
    pub fn topics(&self) -> Result<Vec<String>> {
        let map = self
            .inner
            .read()
            .map_err(|_| anyhow::anyhow!("AppRegistry lock poisoned"))?;
        Ok(map.values().map(|app| app.topic.clone()).collect())
    }

    /// Returns the config for the application subscribed to the given topic,
    /// or `None` if no application matches.
    #[allow(dead_code)] // used once per-message routing is wired up
    pub fn get_by_topic(&self, topic: &str) -> Result<Option<ApplicationConfig>> {
        let map = self
            .inner
            .read()
            .map_err(|_| anyhow::anyhow!("AppRegistry lock poisoned"))?;
        Ok(map.values().find(|app| app.topic == topic).cloned())
    }
}

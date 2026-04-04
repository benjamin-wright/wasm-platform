use std::{
    collections::HashMap,
    sync::{Arc, RwLock},
};

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
    pub fn apply_full_config(&self, full: FullConfig) {
        let mut map = self.inner.write().unwrap();
        map.clear();
        for app in full.applications {
            map.insert((app.namespace.clone(), app.name.clone()), app);
        }
    }

    /// Apply a list of incremental updates: upsert or delete each entry.
    pub fn apply_incremental(&self, updates: Vec<AppUpdate>) {
        let mut map = self.inner.write().unwrap();
        for update in updates {
            if let Some(app) = update.app_config {
                let key = (app.namespace.clone(), app.name.clone());
                if update.delete {
                    map.remove(&key);
                } else {
                    map.insert(key, app);
                }
            }
        }
    }

    /// Returns the NATS topic for every application currently in the registry.
    pub fn topics(&self) -> Vec<String> {
        self.inner
            .read()
            .unwrap()
            .values()
            .map(|app| app.topic.clone())
            .collect()
    }
}

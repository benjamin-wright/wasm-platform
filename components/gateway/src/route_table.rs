use std::{
    collections::HashMap,
    sync::{Arc, RwLock},
};

use anyhow::Result;

#[derive(Clone, Debug)]
pub struct RouteEntry {
    pub methods: Vec<String>,
    pub nats_subject: String,
}

/// Shared, thread-safe routing table keyed by HTTP path.
///
/// The gateway looks up every inbound request here to find the NATS subject and
/// method allow-list for the matching HTTP Application.
#[derive(Clone, Default)]
pub struct RouteTable {
    inner: Arc<RwLock<HashMap<String, RouteEntry>>>,
}

impl RouteTable {
    pub fn new() -> Self {
        Self::default()
    }

    /// Insert or replace the entry for `path`.
    pub fn upsert(&self, path: String, entry: RouteEntry) -> Result<()> {
        let mut map = self
            .inner
            .write()
            .map_err(|_| anyhow::anyhow!("RouteTable lock poisoned"))?;
        map.insert(path, entry);
        Ok(())
    }

    /// Remove the entry for `path`.  A missing key is silently ignored.
    pub fn remove(&self, path: &str) -> Result<()> {
        let mut map = self
            .inner
            .write()
            .map_err(|_| anyhow::anyhow!("RouteTable lock poisoned"))?;
        map.remove(path);
        Ok(())
    }

    /// Look up the entry for `path`.
    pub fn get(&self, path: &str) -> Result<Option<RouteEntry>> {
        let map = self
            .inner
            .read()
            .map_err(|_| anyhow::anyhow!("RouteTable lock poisoned"))?;
        Ok(map.get(path).cloned())
    }

    /// Replace the entire table with the given route set (used after a full snapshot).
    pub fn replace_all(&self, routes: Vec<(String, RouteEntry)>) -> Result<()> {
        let mut map = self
            .inner
            .write()
            .map_err(|_| anyhow::anyhow!("RouteTable lock poisoned"))?;
        map.clear();
        for (path, entry) in routes {
            map.insert(path, entry);
        }
        Ok(())
    }
}

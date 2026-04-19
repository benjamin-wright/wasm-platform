use std::sync::Arc;

use dashmap::DashMap;
use sqlx::postgres::{PgPool, PgPoolOptions};

/// Composite key identifying a single database user's pool: (namespace, app_name, username).
pub type PoolKey = (String, String, String);

/// Manages per-app, per-user PostgreSQL connection pools.
///
/// Pools are keyed by `(namespace, app_name, username)`.  `PgPool` is Arc-backed, so
/// cloning is cheap.  All mutation is lock-free via DashMap.
pub struct SqlPoolMap {
    pools: DashMap<PoolKey, PgPool>,
    max_connections: u32,
}

impl SqlPoolMap {
    pub fn new(max_connections: u32) -> Arc<Self> {
        Arc::new(Self {
            pools: DashMap::new(),
            max_connections,
        })
    }

    /// Returns a clone of the pool for `(namespace, app_name, username)`, or `None` if
    /// no pool has been created for that identity yet.
    pub fn get(&self, namespace: &str, app_name: &str, username: &str) -> Option<PgPool> {
        self.pools
            .get(&(namespace.to_owned(), app_name.to_owned(), username.to_owned()))
            .map(|r| r.value().clone())
    }

    /// Creates a pool for the given identity if one does not already exist.
    ///
    /// Errors are logged and swallowed so that a single bad credential does not abort
    /// the config sync loop.
    pub async fn ensure(
        &self,
        namespace: String,
        app_name: String,
        username: String,
        connection_url: String,
    ) {
        let key = (namespace.clone(), app_name.clone(), username.clone());
        if self.pools.contains_key(&key) {
            return;
        }
        match PgPoolOptions::new()
            .max_connections(self.max_connections)
            .connect(&connection_url)
            .await
        {
            Ok(pool) => {
                self.pools.insert(key, pool);
                tracing::info!(%namespace, %app_name, %username, "SQL pool created");
            }
            Err(e) => {
                tracing::error!(
                    %namespace,
                    %app_name,
                    %username,
                    "failed to create SQL pool: {e:#}",
                );
            }
        }
    }

    /// Closes and removes all pools whose key matches `(namespace, app_name)`.
    pub async fn evict_app(&self, namespace: &str, app_name: &str) {
        // Collect keys first so no DashMap shard lock is held when `evict` removes them.
        let keys: Vec<PoolKey> = self
            .pools
            .iter()
            .filter(|r| r.key().0.as_str() == namespace && r.key().1.as_str() == app_name)
            .map(|r| r.key().clone())
            .collect();
        self.evict(keys).await;
    }

    async fn evict(&self, keys: Vec<PoolKey>) {
        let mut to_close: Vec<PgPool> = Vec::with_capacity(keys.len());
        for key in &keys {
            if let Some((_, pool)) = self.pools.remove(key) {
                to_close.push(pool);
            }
        }
        futures_util::future::join_all(
            to_close.into_iter().map(|p| async move { p.close().await }),
        )
        .await;
    }
}

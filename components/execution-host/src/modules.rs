use std::{
    collections::HashMap,
    sync::{Arc, RwLock},
};

use anyhow::Result;
use wasmtime::{Engine, component::Component};

use crate::{module_cache::ModuleCacheClient, oci};

// ── Constants ─────────────────────────────────────────────────────────────────

/// CPU architecture string used as the `arch` segment in module-cache keys.
/// Matches the Rust target architecture at compile time.
const ARCH: &str = std::env::consts::ARCH;

/// Wasmtime version string injected by build.rs from Cargo.lock.  Used as the
/// `version` segment in module-cache keys so that cached artifacts are
/// invalidated when the runtime is upgraded.
const WASMTIME_VERSION: &str = env!("WASMTIME_VERSION");

// ── ModuleRegistry ────────────────────────────────────────────────────────────

/// Process-wide registry of AOT-compiled `Component` objects, keyed by
/// `(namespace, name)`.
///
/// All interaction with the module-cache HTTP service and OCI registry is
/// encapsulated here. Other modules depend on this type and never call
/// `module_cache` or `oci` directly.
#[derive(Clone)]
pub struct ModuleRegistry {
    #[allow(clippy::type_complexity)]
    inner: Arc<RwLock<HashMap<(String, String), Arc<Component>>>>,
    cache: Arc<ModuleCacheClient>,
    engine: Engine,
}

impl ModuleRegistry {
    pub fn new(cache_base_url: String, engine: Engine) -> Self {
        Self {
            inner: Arc::new(RwLock::new(HashMap::new())),
            cache: Arc::new(ModuleCacheClient::new(cache_base_url)),
            engine,
        }
    }

    /// Returns the compiled `Component` for `(namespace, name)`, or `None` if
    /// not yet loaded.
    pub fn get(&self, namespace: &str, name: &str) -> Result<Option<Arc<Component>>> {
        let map = self
            .inner
            .read()
            .map_err(|_| anyhow::anyhow!("ModuleRegistry lock poisoned"))?;
        Ok(map.get(&(namespace.to_string(), name.to_string())).cloned())
    }

    /// Ensures a `Component` is loaded for the given application.
    ///
    /// Flow:
    /// 1. Resolve OCI manifest digest (no layer download).
    /// 2. Query module cache for a precompiled `.cwasm` artifact.
    /// 3. If found, deserialize directly.
    /// 4. If not found, pull the raw `.wasm`, AOT-compile, push back to cache,
    ///    then store in the registry.
    pub async fn load(&self, namespace: &str, name: &str, module_ref: &str) -> Result<()> {
        let digest = oci::resolve_digest(module_ref).await?;
        // Strip any "sha256:" prefix so the cache path segment is URL-safe.
        let digest_key = digest.strip_prefix("sha256:").unwrap_or(&digest);

        let component = if let Some(artifact) =
            self.cache.get(digest_key, ARCH, WASMTIME_VERSION).await?
        {
            tracing::debug!(
                namespace,
                name,
                digest = digest_key,
                "loading precompiled module from cache"
            );
            // Safety: the artifact was produced by a process using the same
            // Engine configuration and Wasmtime version as this process (we key
            // on WASMTIME_VERSION to enforce this).
            unsafe { Component::deserialize(&self.engine, &artifact)? }
        } else {
            tracing::info!(
                namespace,
                name,
                digest = digest_key,
                "cache miss — pulling and compiling module"
            );
            let wasm = oci::pull_wasm_bytes(module_ref).await?;
            let compiled = self.engine.precompile_component(&wasm)?;

            if let Err(err) = self
                .cache
                .put(digest_key, ARCH, WASMTIME_VERSION, compiled.clone())
                .await
            {
                // A failed cache write is non-fatal; we can still run.
                tracing::warn!("failed to push compiled module to cache: {err:#}");
            }

            // Safety: same as above — just compiled with this engine.
            unsafe { Component::deserialize(&self.engine, &compiled)? }
        };

        let mut map = self
            .inner
            .write()
            .map_err(|_| anyhow::anyhow!("ModuleRegistry lock poisoned"))?;
        map.insert(
            (namespace.to_string(), name.to_string()),
            Arc::new(component),
        );
        tracing::info!(namespace, name, "module registered");
        Ok(())
    }

    /// Removes the entry for `(namespace, name)` when an application is deleted.
    pub fn remove(&self, namespace: &str, name: &str) -> Result<()> {
        let mut map = self
            .inner
            .write()
            .map_err(|_| anyhow::anyhow!("ModuleRegistry lock poisoned"))?;
        map.remove(&(namespace.to_string(), name.to_string()));
        Ok(())
    }
}

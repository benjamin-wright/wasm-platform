use anyhow::{Context, Result};

/// HTTP client for the module-cache service.
///
/// All interaction with the cache (GET to probe, PUT to store) is encapsulated
/// here so callers never depend on the HTTP protocol directly.
pub struct ModuleCacheClient {
    base_url: String,
    client: reqwest::Client,
}

impl ModuleCacheClient {
    pub fn new(base_url: String) -> Self {
        Self {
            base_url,
            client: reqwest::Client::new(),
        }
    }

    /// Returns the precompiled artifact bytes if present, or `None` on a 404.
    pub async fn get(&self, digest: &str, arch: &str, version: &str) -> Result<Option<Vec<u8>>> {
        let url = format!("{}/modules/{digest}/{arch}/{version}", self.base_url);
        let resp = self
            .client
            .get(&url)
            .send()
            .await
            .with_context(|| format!("GET {url}"))?;

        match resp.status() {
            s if s.is_success() => {
                let bytes = resp.bytes().await.with_context(|| format!("reading body from {url}"))?;
                Ok(Some(bytes.to_vec()))
            }
            reqwest::StatusCode::NOT_FOUND => Ok(None),
            s => Err(anyhow::anyhow!("unexpected status {s} from {url}")),
        }
    }

    /// Stores a precompiled artifact in the cache.
    pub async fn put(&self, digest: &str, arch: &str, version: &str, artifact: Vec<u8>) -> Result<()> {
        let url = format!("{}/modules/{digest}/{arch}/{version}", self.base_url);
        let resp = self
            .client
            .put(&url)
            .body(artifact)
            .send()
            .await
            .with_context(|| format!("PUT {url}"))?;

        if !resp.status().is_success() {
            return Err(anyhow::anyhow!(
                "module-cache PUT returned {} for {url}",
                resp.status()
            ));
        }
        Ok(())
    }
}

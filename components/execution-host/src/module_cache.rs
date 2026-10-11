use anyhow::Result;

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
        let resp = self.client.get(&url).send().await.map_err(|error| {
            anyhow::anyhow!("module-cache GET failed ({})", http_error_kind(&error))
        })?;

        match resp.status() {
            s if s.is_success() => {
                let bytes = resp.bytes().await.map_err(|error| {
                    anyhow::anyhow!(
                        "module-cache response body failed ({})",
                        http_error_kind(&error)
                    )
                })?;
                Ok(Some(bytes.to_vec()))
            }
            reqwest::StatusCode::NOT_FOUND => Ok(None),
            s => Err(anyhow::anyhow!(
                "module-cache GET returned status {}",
                s.as_u16()
            )),
        }
    }

    /// Stores a precompiled artifact in the cache.
    pub async fn put(
        &self,
        digest: &str,
        arch: &str,
        version: &str,
        artifact: Vec<u8>,
    ) -> Result<()> {
        let url = format!("{}/modules/{digest}/{arch}/{version}", self.base_url);
        let resp = self
            .client
            .put(&url)
            .body(artifact)
            .send()
            .await
            .map_err(|error| {
                anyhow::anyhow!("module-cache PUT failed ({})", http_error_kind(&error))
            })?;

        if !resp.status().is_success() {
            return Err(anyhow::anyhow!(
                "module-cache PUT returned status {}",
                resp.status().as_u16()
            ));
        }
        Ok(())
    }
}

fn http_error_kind(error: &reqwest::Error) -> &'static str {
    if error.is_builder() {
        "configuration"
    } else if error.is_timeout() {
        "timeout"
    } else if error.is_connect() {
        "connection"
    } else if error.is_redirect() {
        "redirect"
    } else if error.is_body() || error.is_decode() {
        "body"
    } else {
        "request"
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Duration;

    fn assert_safe_error(error: &anyhow::Error) {
        let diagnostic = format!("{error:#}\n{error:?}");
        assert_eq!(error.chain().count(), 1, "{diagnostic}");
        assert!(!diagnostic.contains("CACHE_PASSWORD"), "{diagnostic}");
        assert!(!diagnostic.contains("CACHE_TOKEN"), "{diagnostic}");
        assert!(!diagnostic.contains("http://"), "{diagnostic}");
    }

    #[tokio::test]
    async fn invalid_cache_url_errors_exclude_credentials_and_tokens() {
        let cache = ModuleCacheClient::new(
            "http://user:CACHE_PASSWORD@[invalid?token=CACHE_TOKEN".to_string(),
        );
        let error = tokio::time::timeout(
            Duration::from_secs(3),
            cache.get("digest", "arch", "version"),
        )
        .await
        .expect("cache GET exceeded test deadline")
        .unwrap_err();
        assert_safe_error(&error);
        assert!(error.to_string().contains("configuration"));
        let error = tokio::time::timeout(
            Duration::from_secs(3),
            cache.put("digest", "arch", "version", vec![]),
        )
        .await
        .expect("cache PUT exceeded test deadline")
        .unwrap_err();
        assert_safe_error(&error);
        assert!(error.to_string().contains("configuration"));
    }

    #[tokio::test]
    async fn cache_status_errors_exclude_credentials_and_tokens() {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let server = tokio::spawn(async move {
            axum::serve(
                listener,
                axum::Router::new().fallback(|| async {
                    (
                        axum::http::StatusCode::UNAUTHORIZED,
                        [
                            ("www-authenticate", "Bearer CACHE_TOKEN"),
                            ("x-diagnostic", "CACHE_PASSWORD"),
                        ],
                        "credentials CACHE_PASSWORD token CACHE_TOKEN",
                    )
                }),
            )
            .await
            .unwrap();
        });
        let cache = ModuleCacheClient::new(format!(
            "http://user:CACHE_PASSWORD@{addr}?token=CACHE_TOKEN"
        ));
        let error = tokio::time::timeout(
            Duration::from_secs(3),
            cache.get("digest", "arch", "version"),
        )
        .await
        .expect("cache GET exceeded test deadline")
        .unwrap_err();
        assert_safe_error(&error);
        assert!(error.to_string().contains("401"));
        let error = tokio::time::timeout(
            Duration::from_secs(3),
            cache.put("digest", "arch", "version", vec![]),
        )
        .await
        .expect("cache PUT exceeded test deadline")
        .unwrap_err();
        assert_safe_error(&error);
        assert!(error.to_string().contains("401"));
        server.abort();
        let shutdown = tokio::time::timeout(Duration::from_secs(3), server)
            .await
            .expect("cache test server exceeded shutdown deadline");
        assert!(shutdown.unwrap_err().is_cancelled());
    }
}

use std::time::Duration;

use anyhow::{Result, bail};
use platform_common::artifacts::{MAX_WASM_BYTES, Sha256Digest};
use platform_common::identity::ProjectedTokenReader;

const CACHE_TIMEOUT: Duration = Duration::from_secs(2);

/// Bounded HTTP transport for raw Wasm addressed by its SHA-256 digest.
pub struct ModuleCacheClient {
    base_url: reqwest::Url,
    client: reqwest::Client,
    token: ProjectedTokenReader,
}

impl ModuleCacheClient {
    #[cfg(test)]
    pub(crate) fn set_token_reader(&mut self, token: ProjectedTokenReader) {
        self.token = token;
    }

    pub fn new(base_url: String) -> Result<Self> {
        let mut base_url = reqwest::Url::parse(&base_url)
            .map_err(|_| anyhow::anyhow!("invalid module-cache URL configuration"))?;
        if !matches!(base_url.scheme(), "http" | "https")
            || base_url.host_str().is_none()
            || !base_url.username().is_empty()
            || base_url.password().is_some()
            || base_url.query().is_some()
            || base_url.fragment().is_some()
        {
            bail!("invalid module-cache URL configuration");
        }
        if !base_url.path().ends_with('/') {
            base_url.set_path(&format!("{}/", base_url.path()));
        }
        let client = reqwest::Client::builder()
            .timeout(CACHE_TIMEOUT)
            .redirect(reqwest::redirect::Policy::none())
            .no_proxy()
            .build()
            .map_err(|_| anyhow::anyhow!("module-cache client configuration failed"))?;
        Ok(Self {
            base_url,
            client,
            token: ProjectedTokenReader::platform(),
        })
    }

    fn url(&self, digest: &Sha256Digest) -> Result<reqwest::Url> {
        self.base_url
            .join(&format!("modules/{digest}"))
            .map_err(|_| anyhow::anyhow!("invalid module-cache URL configuration"))
    }

    /// Returns at most 16 MiB; the caller checks integrity against the OCI descriptor.
    pub async fn get(&self, digest: &Sha256Digest) -> Result<Option<Vec<u8>>> {
        tokio::time::timeout(CACHE_TIMEOUT, async {
            let response = self
                .token
                .http_request(self.client.get(self.url(digest)?))
                .await?
                .send()
                .await
                .map_err(|error| {
                    anyhow::anyhow!("module-cache GET failed ({})", http_error_kind(&error))
                })?;
            match response.status() {
                reqwest::StatusCode::NOT_FOUND => return Ok(None),
                reqwest::StatusCode::OK => {}
                status => bail!("module-cache GET returned status {}", status.as_u16()),
            }
            crate::oci::read_response(response, MAX_WASM_BYTES)
                .await
                .map(Some)
        })
        .await
        .map_err(|_| anyhow::anyhow!("module-cache GET failed (timeout)"))?
    }

    pub async fn put(&self, digest: &Sha256Digest, wasm: Vec<u8>) -> Result<()> {
        if wasm.len() > MAX_WASM_BYTES {
            bail!("module-cache PUT exceeds Wasm size limit");
        }
        tokio::time::timeout(CACHE_TIMEOUT, async {
            let response = self
                .token
                .http_request(
                    self.client
                        .put(self.url(digest)?)
                        .header(reqwest::header::CONTENT_TYPE, "application/wasm")
                        .body(wasm),
                )
                .await?
                .send()
                .await
                .map_err(|error| {
                    anyhow::anyhow!("module-cache PUT failed ({})", http_error_kind(&error))
                })?;
            if !response.status().is_success() {
                bail!(
                    "module-cache PUT returned status {}",
                    response.status().as_u16()
                );
            }
            Ok(())
        })
        .await
        .map_err(|_| anyhow::anyhow!("module-cache PUT failed (timeout)"))?
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
    use axum::{
        Router,
        body::Body,
        http::{Response, StatusCode, header},
        routing::get,
    };

    struct TestTokenFile(std::path::PathBuf);

    impl Drop for TestTokenFile {
        fn drop(&mut self) {
            let _ = std::fs::remove_file(&self.0);
        }
    }

    fn assert_safe_error(error: &anyhow::Error) {
        let diagnostic = format!("{error:#}\n{error:?}");
        assert_eq!(error.chain().count(), 1, "{diagnostic}");
        for secret in ["CACHE_PASSWORD", "CACHE_TOKEN", "http://"] {
            assert!(!diagnostic.contains(secret), "{diagnostic}");
        }
    }

    #[test]
    fn invalid_cache_url_errors_exclude_credentials_and_tokens() {
        for url in [
            "http://user:CACHE_PASSWORD@[invalid?token=CACHE_TOKEN",
            "http://user:CACHE_PASSWORD@localhost/?token=CACHE_TOKEN",
            "file:///tmp/modules",
        ] {
            let error = ModuleCacheClient::new(url.into()).err().unwrap();
            assert_safe_error(&error);
            assert!(error.to_string().contains("configuration"));
        }
    }

    async fn server(router: Router) -> (ModuleCacheClient, tokio::task::JoinHandle<()>) {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let mut client = ModuleCacheClient::new(format!("http://{addr}")).unwrap();
        let path = std::env::temp_dir().join(format!(
            "cache-client-{}-{}.token",
            std::process::id(),
            addr.port()
        ));
        tokio::fs::write(&path, "HOST_TOKEN").await.unwrap();
        client.token = ProjectedTokenReader::new(&path);
        let server = tokio::spawn(async move {
            let _token_file = TestTokenFile(path);
            axum::serve(listener, router).await.unwrap();
        });
        (client, server)
    }

    fn digest() -> Sha256Digest {
        format!("sha256:{}", "a".repeat(64)).parse().unwrap()
    }

    #[tokio::test]
    async fn cache_status_errors_exclude_response_secrets() {
        let router = Router::new().fallback(|| async {
            (
                StatusCode::UNAUTHORIZED,
                [("www-authenticate", "Bearer CACHE_TOKEN")],
                "credentials CACHE_PASSWORD token CACHE_TOKEN",
            )
        });
        let (cache, server) = server(router).await;
        for error in [
            cache.get(&digest()).await.unwrap_err(),
            cache.put(&digest(), vec![]).await.unwrap_err(),
        ] {
            assert_safe_error(&error);
            assert!(error.to_string().contains("401"));
        }
        server.abort();
        let _ = server.await;
    }

    #[tokio::test]
    async fn redirects_do_not_follow_another_cache_origin() {
        let router = Router::new().fallback(|| async {
            (
                StatusCode::TEMPORARY_REDIRECT,
                [(
                    header::LOCATION,
                    "http://CACHE_PASSWORD.invalid/?token=CACHE_TOKEN",
                )],
            )
        });
        let (cache, server) = server(router).await;
        let error = cache.get(&digest()).await.unwrap_err();
        assert_safe_error(&error);
        assert!(error.to_string().contains("307"));
        server.abort();
        let _ = server.await;
    }

    #[tokio::test]
    async fn advertised_oversized_body_is_rejected_before_reading() {
        let router = Router::new().fallback(|| async {
            Response::builder()
                .header(header::CONTENT_LENGTH, MAX_WASM_BYTES + 1)
                .body(Body::from_stream(futures_util::stream::pending::<
                    Result<Vec<u8>, std::io::Error>,
                >()))
                .unwrap()
        });
        let (cache, server) = server(router).await;
        let error = cache.get(&digest()).await.unwrap_err();
        assert!(error.to_string().contains("size limit"));
        server.abort();
        let _ = server.await;
    }

    #[tokio::test]
    async fn streamed_body_cannot_exceed_the_artifact_limit() {
        let router = Router::new().route(
            "/modules/{digest}",
            get(|| async {
                let stream = futures_util::stream::iter(
                    (0..=16).map(|_| Ok::<_, std::io::Error>(vec![0; 1024 * 1024])),
                );
                Body::from_stream(stream)
            }),
        );
        let (cache, server) = server(router).await;
        let error = cache.get(&digest()).await.unwrap_err();
        assert!(error.to_string().contains("size limit"));
        server.abort();
        let _ = server.await;
    }

    #[tokio::test]
    async fn stalled_body_uses_one_whole_response_deadline() {
        let router = Router::new().fallback(|| async {
            let stream = futures_util::stream::pending::<Result<Vec<u8>, std::io::Error>>();
            Body::from_stream(stream)
        });
        let (cache, server) = server(router).await;
        let error = tokio::time::timeout(Duration::from_secs(3), cache.get(&digest()))
            .await
            .unwrap()
            .unwrap_err();
        assert!(error.to_string().contains("timeout"));
        server.abort();
        let _ = server.await;
    }
}

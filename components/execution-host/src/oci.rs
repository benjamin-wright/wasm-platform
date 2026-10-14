use std::{collections::HashSet, net::IpAddr, time::Duration};

use anyhow::{Result, ensure};
use oci_distribution::{Reference, manifest::OciImageManifest};
use platform_common::artifacts::{MAX_MANIFEST_BYTES, MAX_WASM_BYTES, Sha256Digest, verify_wasm};

#[derive(Clone)]
pub struct OciSource {
    approved: HashSet<String>,
    plaintext: HashSet<String>,
    client: reqwest::Client,
}

pub struct WasmArtifact {
    pub reference: String,
    pub digest: Sha256Digest,
    pub size: usize,
}

impl OciSource {
    pub fn from_env() -> Result<Self> {
        let approved = std::env::var("APPROVED_REGISTRIES")
            .map_err(|_| anyhow::anyhow!("APPROVED_REGISTRIES is required"))?;
        let plaintext = std::env::var("LOCAL_PLAINTEXT_REGISTRIES").unwrap_or_default();
        Self::new(&approved, &plaintext)
    }

    pub fn new(approved: &str, plaintext: &str) -> Result<Self> {
        let approved = registry_list(approved)?;
        ensure!(
            !approved.is_empty(),
            "approved registry list must not be empty"
        );
        let plaintext = registry_list(plaintext)?;
        for registry in &plaintext {
            ensure!(
                approved.contains(registry),
                "plaintext registry must be approved"
            );
            let url = registry_url(registry)?;
            let host = url.host_str().unwrap_or_default().trim_matches(['[', ']']);
            ensure!(
                host == "localhost"
                    || host.ends_with(".localhost")
                    || host.parse::<IpAddr>().is_ok_and(|ip| ip.is_loopback()),
                "plaintext registry must be a local development origin"
            );
        }
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(10))
            .connect_timeout(Duration::from_secs(3))
            .redirect(reqwest::redirect::Policy::none())
            .build()
            .map_err(|_| anyhow::anyhow!("OCI client configuration failed"))?;
        Ok(Self {
            approved,
            plaintext,
            client,
        })
    }

    fn parse(&self, image_ref: &str) -> Result<Reference> {
        let bare = image_ref.strip_prefix("oci://").unwrap_or(image_ref);
        ensure!(bare.len() <= 2048, "invalid OCI reference");
        let (registry, path) = bare
            .split_once('/')
            .ok_or_else(|| anyhow::anyhow!("invalid OCI reference"))?;
        registry_url(registry)?;
        ensure!(
            self.approved.contains(registry),
            "OCI registry is not approved"
        );
        let (repository, digest) = path
            .split_once('@')
            .ok_or_else(|| anyhow::anyhow!("immutable OCI reference required"))?;
        Sha256Digest::parse(digest)?;
        let parsed: Reference = format!("registry.example/{repository}@{digest}")
            .parse()
            .map_err(|_| anyhow::anyhow!("invalid OCI reference"))?;
        Ok(Reference::with_digest(
            registry.to_string(),
            parsed.repository().to_string(),
            digest.to_string(),
        ))
    }

    fn endpoint(&self, reference: &Reference, kind: &str, identity: &str) -> String {
        let scheme = if self.plaintext.contains(reference.registry()) {
            "http"
        } else {
            "https"
        };
        format!(
            "{scheme}://{}/v2/{}/{kind}/{identity}",
            reference.registry(),
            reference.repository()
        )
    }

    pub async fn manifest(&self, image_ref: &str) -> Result<WasmArtifact> {
        let reference = self.parse(image_ref)?;
        let digest = Sha256Digest::parse(
            reference
                .digest()
                .ok_or_else(|| anyhow::anyhow!("immutable OCI reference required"))?,
        )?;
        let response = self.client.get(self.endpoint(&reference, "manifests", digest.as_str()))
            .header(reqwest::header::ACCEPT, "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")
            .send().await.map_err(|_| anyhow::anyhow!("OCI manifest request failed"))?;
        ensure!(
            response.status() == reqwest::StatusCode::OK,
            "OCI manifest request rejected"
        );
        verify_digest_header(&response, &digest)?;
        let bytes = read_response(response, MAX_MANIFEST_BYTES).await?;
        digest.verify(&bytes)?;
        let manifest: OciImageManifest =
            serde_json::from_slice(&bytes).map_err(|_| anyhow::anyhow!("invalid OCI manifest"))?;
        ensure!(manifest.schema_version == 2, "unsupported OCI schema");
        ensure!(
            matches!(
                manifest.media_type.as_deref(),
                Some("application/vnd.oci.image.manifest.v1+json")
                    | Some("application/vnd.docker.distribution.manifest.v2+json")
            ),
            "unsupported OCI manifest type"
        );
        ensure!(
            manifest.layers.len() == 1,
            "Wasm artifact requires exactly one layer"
        );
        let layer = &manifest.layers[0];
        ensure!(
            matches!(
                layer.media_type.as_str(),
                "application/octet-stream"
                    | "application/wasm"
                    | "application/vnd.wasm.content.layer.v1+wasm"
            ) || (layer.media_type == "application/vnd.oci.image.layer.v1.tar"
                && manifest.artifact_type.as_deref()
                    == Some("application/vnd.wasm.content.layer.v1+wasm")),
            "unsupported Wasm layer type"
        );
        ensure!(
            layer.urls.as_ref().is_none_or(|urls| urls.is_empty()),
            "external OCI layer URLs are unsupported"
        );
        let size =
            usize::try_from(layer.size).map_err(|_| anyhow::anyhow!("invalid Wasm layer size"))?;
        ensure!(
            (8..=MAX_WASM_BYTES).contains(&size),
            "Wasm layer exceeds size bounds"
        );
        Ok(WasmArtifact {
            reference: image_ref.to_string(),
            digest: Sha256Digest::parse(&layer.digest)?,
            size,
        })
    }

    pub async fn pull(&self, artifact: &WasmArtifact) -> Result<Vec<u8>> {
        let reference = self.parse(&artifact.reference)?;
        let response = self
            .client
            .get(self.endpoint(&reference, "blobs", artifact.digest.as_str()))
            .send()
            .await
            .map_err(|_| anyhow::anyhow!("OCI blob request failed"))?;
        ensure!(
            response.status() == reqwest::StatusCode::OK,
            "OCI blob request rejected"
        );
        verify_digest_header(&response, &artifact.digest)?;
        let bytes = read_response(response, artifact.size).await?;
        ensure!(bytes.len() == artifact.size, "OCI layer size mismatch");
        verify_wasm(&artifact.digest, &bytes)?;
        Ok(bytes)
    }
}

fn registry_url(registry: &str) -> Result<reqwest::Url> {
    let url = reqwest::Url::parse(&format!("https://{registry}"))
        .map_err(|_| anyhow::anyhow!("invalid registry origin"))?;
    ensure!(
        url.host_str().is_some()
            && url.username().is_empty()
            && url.password().is_none()
            && url.path() == "/"
            && url.query().is_none()
            && url.fragment().is_none()
            && !registry.ends_with('/')
            && (registry == url.authority()
                || registry.strip_suffix(":443") == Some(url.authority())),
        "invalid registry origin"
    );
    Ok(url)
}

fn registry_list(value: &str) -> Result<HashSet<String>> {
    if value.is_empty() {
        return Ok(HashSet::new());
    }
    value
        .split(',')
        .map(|origin| {
            registry_url(origin)?;
            Ok(origin.to_string())
        })
        .collect()
}

fn verify_digest_header(response: &reqwest::Response, expected: &Sha256Digest) -> Result<()> {
    if let Some(value) = response.headers().get("docker-content-digest") {
        let digest = value
            .to_str()
            .map_err(|_| anyhow::anyhow!("invalid OCI digest header"))?;
        ensure!(digest == expected.as_str(), "OCI digest header mismatch");
    }
    Ok(())
}

pub async fn read_response(mut response: reqwest::Response, limit: usize) -> Result<Vec<u8>> {
    ensure!(
        response
            .content_length()
            .is_none_or(|size| size <= limit as u64),
        "artifact response exceeds size limit"
    );
    let mut bytes = Vec::new();
    while let Some(chunk) = response.chunk().await.map_err(|error| {
        if error.is_timeout() {
            anyhow::anyhow!("artifact response timeout")
        } else {
            anyhow::anyhow!("artifact response read failed")
        }
    })? {
        ensure!(
            chunk.len() <= limit.saturating_sub(bytes.len()),
            "artifact response exceeds size limit"
        );
        bytes.extend_from_slice(&chunk);
    }
    Ok(bytes)
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::{
        Router,
        body::Body,
        http::{Response, StatusCode},
        routing::get,
    };
    use std::sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    };

    #[test]
    fn origins_are_exact_https_by_default_and_plaintext_is_local_only() {
        let source = OciSource::new("registry.example:443", "").unwrap();
        let pinned = format!(
            "registry.example:443/app@{}",
            Sha256Digest::from_bytes(b"manifest").as_str()
        );
        let reference = source.parse(&pinned).unwrap();
        assert!(
            source
                .endpoint(&reference, "manifests", "digest")
                .starts_with("https://")
        );
        assert!(source.parse(&pinned.replace(":443", ":444")).is_err());
        assert!(source.parse("registry.example:443/app:latest").is_err());
        let ipv6 = OciSource::new("[::1]:5001", "[::1]:5001").unwrap();
        let reference = ipv6
            .parse(&format!(
                "[::1]:5001/app@{}",
                Sha256Digest::from_bytes(b"manifest").as_str()
            ))
            .unwrap();
        assert_eq!(reference.registry(), "[::1]:5001");
        assert!(
            ipv6.endpoint(&reference, "manifests", "digest")
                .starts_with("http://[::1]:5001/")
        );
        for (approved, plaintext) in [
            ("", ""),
            ("registry.example", "registry.example"),
            ("localhost:1", "localhost:2"),
            ("user:secret@localhost:1", ""),
            ("localhost:1/path", ""),
            ("localhost:1?token=secret", ""),
        ] {
            assert!(OciSource::new(approved, plaintext).is_err());
        }
    }

    #[tokio::test]
    async fn immutable_manifest_and_layer_are_verified_before_use() {
        let wasm = b"\0asm\x0d\0\x01\0".to_vec();
        let digest = Sha256Digest::from_bytes(&wasm);
        let manifest = serde_json::to_vec(&serde_json::json!({"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","artifactType":"application/vnd.wasm.content.layer.v1+wasm","config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":Sha256Digest::from_bytes(b"{}").as_str(),"size":2},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":digest.as_str(),"size":wasm.len()}]})).unwrap();
        let manifest_digest = Sha256Digest::from_bytes(&manifest);
        let requests = Arc::new(AtomicUsize::new(0));
        let count = requests.clone();
        let manifest_for_route = manifest.clone();
        let wasm_for_route = wasm.clone();
        let app = Router::new()
            .route(
                &format!("/v2/app/manifests/{}", manifest_digest.as_str()),
                get(move || {
                    let data = manifest_for_route.clone();
                    let count = count.clone();
                    async move {
                        count.fetch_add(1, Ordering::SeqCst);
                        data
                    }
                }),
            )
            .route(
                &format!("/v2/app/blobs/{}", digest.as_str()),
                get(move || {
                    let data = wasm_for_route.clone();
                    async move { data }
                }),
            )
            .fallback(|| async { StatusCode::NOT_FOUND });
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let origin = listener.local_addr().unwrap().to_string();
        let server = tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });
        let source = OciSource::new(&origin, &origin).unwrap();
        let pinned = format!("{origin}/app@{}", manifest_digest.as_str());
        let artifact = source.manifest(&pinned).await.unwrap();
        assert_eq!(source.pull(&artifact).await.unwrap(), wasm);
        assert_eq!(requests.load(Ordering::SeqCst), 1);
        assert!(
            source
                .manifest(&format!("{origin}/app:latest"))
                .await
                .is_err()
        );
        server.abort();
        for body in [b"tampered".to_vec(), vec![0; MAX_MANIFEST_BYTES + 1]] {
            let app = Router::new().fallback(move || {
                let body = body.clone();
                async move { Response::new(Body::from(body)) }
            });
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
            let origin = listener.local_addr().unwrap().to_string();
            let server = tokio::spawn(async move {
                axum::serve(listener, app).await.unwrap();
            });
            let source = OciSource::new(&origin, &origin).unwrap();
            assert!(
                source
                    .manifest(&format!("{origin}/app@{}", manifest_digest.as_str()))
                    .await
                    .is_err()
            );
            assert!(
                source
                    .pull(&WasmArtifact {
                        reference: format!("{origin}/app@{}", manifest_digest.as_str()),
                        digest: digest.clone(),
                        size: wasm.len()
                    })
                    .await
                    .is_err()
            );
            server.abort();
        }
    }
}

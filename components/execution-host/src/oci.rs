use anyhow::{Context, Result};
use oci_distribution::{
    Reference,
    client::{Client, ClientConfig, ClientProtocol},
    secrets::RegistryAuth,
};

/// Pulls the first layer of an OCI image and returns its raw bytes.
///
/// `image_ref` is a full OCI reference such as `oci://registry:5000/namespace/app@sha256:<digest>`
/// or the bare form `registry:5000/namespace/app@sha256:<digest>`. The `oci://` scheme prefix is
/// stripped before parsing. The registry is assumed to be unauthenticated (internal).
pub async fn pull_wasm_bytes(image_ref: &str) -> Result<Vec<u8>> {
    let bare = image_ref.strip_prefix("oci://").unwrap_or(image_ref);
    let reference: Reference = bare
        .parse()
        .with_context(|| format!("parsing OCI reference: {image_ref}"))?;

    let config = ClientConfig {
        protocol: ClientProtocol::HttpsExcept(vec![
            reference.registry().to_string(),
        ]),
        ..Default::default()
    };
    let client = Client::new(config);

    let (manifest, _) = client
        .pull_manifest(&reference, &RegistryAuth::Anonymous)
        .await
        .with_context(|| format!("pulling manifest for {image_ref}"))?;

    use oci_distribution::manifest::OciManifest;
    let layers = match &manifest {
        OciManifest::Image(img) => &img.layers,
        OciManifest::ImageIndex(_) => {
            return Err(anyhow::anyhow!(
                "OCI image index (multi-arch manifest) not supported for {image_ref}"
            ))
        }
    };

    let layer = layers
        .first()
        .ok_or_else(|| anyhow::anyhow!("OCI image {image_ref} has no layers"))?;

    let mut bytes = Vec::new();
    client
        .pull_blob(&reference, layer, &mut bytes)
        .await
        .with_context(|| format!("pulling layer {} from {image_ref}", layer.digest))?;

    Ok(bytes)
}

/// Returns the OCI manifest digest for an image reference without downloading
/// the layer, for use as the module-cache key.
///
/// Accepts the `oci://` scheme prefix used in `Application.spec.functions[].module`;
/// strips it before parsing.
pub async fn resolve_digest(image_ref: &str) -> Result<String> {
    let bare = image_ref.strip_prefix("oci://").unwrap_or(image_ref);
    let reference: Reference = bare
        .parse()
        .with_context(|| format!("parsing OCI reference: {image_ref}"))?;

    let config = ClientConfig {
        protocol: ClientProtocol::HttpsExcept(vec![
            reference.registry().to_string(),
        ]),
        ..Default::default()
    };
    let client = Client::new(config);

    let (_, digest) = client
        .pull_manifest(&reference, &RegistryAuth::Anonymous)
        .await
        .with_context(|| format!("resolving digest for {image_ref}"))?;

    Ok(digest)
}

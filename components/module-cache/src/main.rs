use anyhow::Result;
use axum::{
    Router,
    body::Bytes,
    extract::{Path, State},
    http::StatusCode,
    response::IntoResponse,
    routing::get,
};
use platform_common::health;
use std::{collections::HashMap, sync::Arc};
use tokio::sync::RwLock;

// ── Cache state ───────────────────────────────────────────────────────────────
// Keyed by (digest, architecture, wasmtime_version).  The value is the raw bytes
// of an AOT-compiled .cwasm artifact produced by an execution host.

type Cache = Arc<RwLock<HashMap<(String, String, String), Bytes>>>;

// ── Entry point ───────────────────────────────────────────────────────────────

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
        .init();

    let cache: Cache = Arc::new(RwLock::new(HashMap::new()));

    tracing::info!("module-cache starting");

    let app = Router::new()
        .route(
            "/modules/{digest}/{arch}/{version}",
            get(get_module).put(put_module),
        )
        .route("/healthz", get(health::healthz_handler))
        .with_state(cache);

    let listener = tokio::net::TcpListener::bind("0.0.0.0:3000").await?;
    axum::serve(listener, app).await?;
    Ok(())
}

// ── Handlers ──────────────────────────────────────────────────────────────────

async fn get_module(
    State(cache): State<Cache>,
    Path((digest, arch, version)): Path<(String, String, String)>,
) -> impl IntoResponse {
    let key = (digest, arch, version);
    let store = cache.read().await;
    match store.get(&key) {
        Some(artifact) => {
            tracing::info!(digest = key.0, arch = key.1, version = key.2, "cache hit");
            (StatusCode::OK, artifact.clone()).into_response()
        }
        None => {
            tracing::info!(digest = key.0, arch = key.1, version = key.2, "cache miss");
            StatusCode::NOT_FOUND.into_response()
        }
    }
}

async fn put_module(
    State(cache): State<Cache>,
    Path((digest, arch, version)): Path<(String, String, String)>,
    body: Bytes,
) -> StatusCode {
    let size = body.len();
    tracing::info!(digest, arch, version, bytes = size, "module stored");
    let mut store = cache.write().await;
    store.insert((digest, arch, version), body);
    StatusCode::NO_CONTENT
}


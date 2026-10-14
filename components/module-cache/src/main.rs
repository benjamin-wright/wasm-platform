mod cache;

use anyhow::Result;
use std::{future::IntoFuture, sync::atomic::Ordering, time::Duration};
use tracing_subscriber::prelude::*;

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::registry()
        .with(tracing_subscriber::EnvFilter::from_default_env())
        .with(
            tracing_subscriber::fmt::layer().with_filter(tracing_subscriber::filter::filter_fn(
                platform_common::logging::is_platform_log,
            )),
        )
        .init();
    platform_common::logging::install_panic_hook();

    let state = cache::CacheState::new();
    let listener = tokio::net::TcpListener::bind("0.0.0.0:3000").await?;
    let (shutdown_tx, shutdown_rx) = tokio::sync::oneshot::channel();
    let server = axum::serve(listener, cache::router(state.clone()))
        .with_graceful_shutdown(async {
            let _ = shutdown_rx.await;
        })
        .into_future();
    tokio::pin!(server);
    tracing::info!("module-cache starting");

    tokio::select! {
        result = &mut server => return result.map_err(Into::into),
        result = shutdown_signal() => result?,
    }
    state.ready.store(false, Ordering::Release);
    let _ = shutdown_tx.send(());
    match tokio::time::timeout(Duration::from_secs(10), &mut server).await {
        Ok(result) => result?,
        Err(_) => tracing::warn!("module-cache shutdown drain deadline reached"),
    }
    Ok(())
}

async fn shutdown_signal() -> Result<()> {
    #[cfg(unix)]
    {
        let mut terminate =
            tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())?;
        tokio::select! {
            result = tokio::signal::ctrl_c() => result?,
            _ = terminate.recv() => {},
        }
    }
    #[cfg(not(unix))]
    tokio::signal::ctrl_c().await?;
    Ok(())
}

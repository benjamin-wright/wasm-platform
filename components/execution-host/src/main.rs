use axum::{
    Router,
    routing::{get, post},
};

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
        .init();

    tracing::info!("execution-host starting");

    let app = Router::new()
        .route("/execute", post(execute_handler))
        .route("/healthz", get(healthz_handler));

    let listener = tokio::net::TcpListener::bind("0.0.0.0:3000").await.unwrap();
    axum::serve(listener, app).await.unwrap();
    Ok(())
}

async fn execute_handler() -> &'static str {
    tracing::info!("execute_handler called");
    "Hello, World!"
}

async fn healthz_handler() -> &'static str {
    tracing::info!("healthz_handler called");
    "OK"
}

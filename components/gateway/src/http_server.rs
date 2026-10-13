use std::{sync::Arc, time::Duration};

use axum::{
    Router,
    body::Body,
    extract::{Request, State},
    response::Response,
    routing::any,
    serve::Listener,
};
use hyper::{server::conn::http1, service::service_fn};
use hyper_util::rt::{TokioIo, TokioTimer};
use tokio::task::JoinSet;
use tower::ServiceExt;

use crate::{listener::BoundedListener, supervisor::RequestSupervisor};

/// Serves HTTP/1 connections with bounded parsing buffers and owned connection tasks.
pub async fn serve_http(mut listener: BoundedListener, app: Router, header_timeout: Duration) {
    let mut connections = JoinSet::new();
    loop {
        tokio::select! {
            biased;
            Some(result) = connections.join_next(), if !connections.is_empty() => {
                if result.is_err() {
                    tracing::warn!("HTTP connection task failed");
                }
            }
            (connection, _) = listener.accept() => {
                let app = app.clone();
                connections.spawn(async move {
                    let service = service_fn(move |request: hyper::Request<hyper::body::Incoming>| {
                        app.clone().oneshot(request.map(Body::new))
                    });
                    let mut server = http1::Builder::new();
                    server
                        .timer(TokioTimer::new())
                        .header_read_timeout(header_timeout)
                        .max_buf_size(32 * 1024)
                        .max_headers(128);
                    if let Err(error) = server.serve_connection(TokioIo::new(connection), service).await {
                        tracing::debug!(is_timeout = error.is_timeout(), "HTTP connection closed");
                    }
                });
            }
        }
    }
}

pub fn build_router(supervisor: Arc<RequestSupervisor>) -> Router {
    Router::new()
        .fallback(any(handle_request))
        .with_state(supervisor)
}

async fn handle_request(
    State(supervisor): State<Arc<RequestSupervisor>>,
    req: Request,
) -> Response {
    supervisor.handle(req).await
}

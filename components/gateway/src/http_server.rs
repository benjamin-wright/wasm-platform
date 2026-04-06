use std::{sync::Arc, time::Duration};

use axum::{
    Router,
    extract::{Request, State},
    http::{HeaderMap, HeaderName, HeaderValue, StatusCode},
    response::{IntoResponse, Response},
    routing::any,
};
use serde::{Deserialize, Serialize};

use crate::route_table::RouteTable;

// ── Platform-private HTTP payload types ───────────────────────────────────────
// These must match the types defined in execution-host/src/runtime.rs exactly.
// They are independently defined in each binary; both sides serialize/deserialize
// the same JSON shape so the module never sees raw JSON.

#[derive(Debug, Serialize)]
struct HttpRequestPayload {
    method: String,
    path: String,
    query: String,
    headers: Vec<(String, String)>,
    body: Option<Vec<u8>>,
}

#[derive(Debug, Deserialize)]
struct HttpResponsePayload {
    status: u16,
    headers: Vec<(String, String)>,
    body: Option<Vec<u8>>,
}

// ── Shared gateway state ──────────────────────────────────────────────────────

pub struct GatewayState {
    pub table: RouteTable,
    pub nats: tokio::sync::watch::Receiver<Option<async_nats::Client>>,
    pub timeout: Duration,
}

// ── Router ────────────────────────────────────────────────────────────────────

pub fn build_router(state: Arc<GatewayState>) -> Router {
    Router::new()
        .fallback(any(handle_request))
        .with_state(state)
}

// ── Request handler ───────────────────────────────────────────────────────────

async fn handle_request(
    State(state): State<Arc<GatewayState>>,
    req: Request,
) -> Response {
    let method = req.method().to_string();
    let uri = req.uri().clone();
    let path = uri.path().to_string();
    let query = uri.query().unwrap_or("").to_string();

    // Extract headers — skip any whose values are not valid UTF-8.
    let headers: Vec<(String, String)> = req
        .headers()
        .iter()
        .filter_map(|(k, v)| v.to_str().ok().map(|v_str| (k.to_string(), v_str.to_string())))
        .collect();

    // Consume and buffer the request body.
    let body_bytes = match axum::body::to_bytes(req.into_body(), usize::MAX).await {
        Ok(b) => b,
        Err(_) => return StatusCode::BAD_REQUEST.into_response(),
    };
    let body = if body_bytes.is_empty() {
        None
    } else {
        Some(body_bytes.to_vec())
    };

    // Route lookup.
    let route_entry = match state.table.get(&path) {
        Ok(Some(e)) => e,
        Ok(None) => return StatusCode::NOT_FOUND.into_response(),
        Err(err) => {
            tracing::error!(?err, "route table error");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };

    // Method enforcement: if the allow-list is non-empty and the request method
    // is not in it, return 405 with an Allow header.
    if !route_entry.methods.is_empty()
        && !route_entry
            .methods
            .iter()
            .any(|m| m.eq_ignore_ascii_case(&method))
    {
        let allow = route_entry.methods.join(", ");
        return (
            StatusCode::METHOD_NOT_ALLOWED,
            [(axum::http::header::ALLOW, allow)],
        )
            .into_response();
    }

    // Snapshot the current NATS client.  If the manager is mid-reconnect,
    // return 503 — the caller should retry.
    let nats = state.nats.borrow().clone();
    let nats = match nats {
        Some(c) => c,
        None => {
            tracing::warn!("NATS unavailable; returning 503");
            return StatusCode::SERVICE_UNAVAILABLE.into_response();
        }
    };

    // Serialise the request as a platform-private JSON payload.
    let payload = HttpRequestPayload {
        method,
        path,
        query,
        headers,
        body,
    };
    let payload_bytes = match serde_json::to_vec(&payload) {
        Ok(b) => b,
        Err(err) => {
            tracing::error!("failed to serialise request payload: {err:#}");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };

    // Publish via NATS request-reply, respecting the configured timeout.
    let response_msg = match tokio::time::timeout(
        state.timeout,
        nats.request(route_entry.nats_subject.clone(), payload_bytes.into()),
    )
    .await
    {
        Ok(Ok(msg)) => msg,
        Ok(Err(err)) => {
            tracing::error!(%route_entry.nats_subject, "NATS request failed: {err:#}");
            return StatusCode::BAD_GATEWAY.into_response();
        }
        Err(_) => {
            tracing::warn!(%route_entry.nats_subject, "NATS request timed out");
            return StatusCode::GATEWAY_TIMEOUT.into_response();
        }
    };

    // Deserialise the platform-private JSON response from the execution host.
    let http_response: HttpResponsePayload = match serde_json::from_slice(&response_msg.payload) {
        Ok(r) => r,
        Err(err) => {
            tracing::error!("failed to deserialise response payload: {err:#}");
            return StatusCode::BAD_GATEWAY.into_response();
        }
    };

    // Build the HTTP response.
    let status = StatusCode::from_u16(http_response.status)
        .unwrap_or(StatusCode::INTERNAL_SERVER_ERROR);

    let mut header_map = HeaderMap::new();
    for (k, v) in &http_response.headers {
        if let (Ok(name), Ok(value)) = (
            HeaderName::from_bytes(k.as_bytes()),
            HeaderValue::from_str(v),
        ) {
            header_map.insert(name, value);
        }
    }

    let body = http_response.body.unwrap_or_default();
    (status, header_map, body).into_response()
}

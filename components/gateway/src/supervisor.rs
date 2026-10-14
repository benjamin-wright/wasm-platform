use std::{
    io::{self, Write},
    sync::{
        Arc,
        atomic::{AtomicU64, AtomicUsize, Ordering},
    },
};

use axum::{
    body::Body,
    extract::Request,
    http::{HeaderMap, HeaderName, HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
};
use futures_util::{StreamExt, stream};
use platform_common::http_types::{HttpRequestPayload, HttpResponsePayload};
use tokio::{
    sync::{OwnedSemaphorePermit, Semaphore, watch},
    time::Instant,
};

use crate::{
    config::RequestLimits,
    nats::{RequestBroker, RequestError},
    route_table::RouteTable,
};

#[derive(Default)]
struct Counts {
    high_water: AtomicUsize,
    rejected: AtomicU64,
    completed: AtomicU64,
    cancelled: AtomicU64,
}

pub struct Snapshot {
    pub in_flight: usize,
    pub in_flight_limit: usize,
    pub high_water: usize,
    pub rejected: u64,
    pub completed: u64,
    pub cancelled: u64,
    pub connection_count: usize,
    pub connection_limit: usize,
}

/// Owns request admission through body intake, dispatch and buffered reply consumption.
/// Requests execute in the HTTP service future; no detached tasks or waiting queue exist.
pub struct RequestSupervisor {
    table: RouteTable,
    broker: Arc<dyn RequestBroker>,
    limits: RequestLimits,
    requests: Arc<Semaphore>,
    connections: Arc<Semaphore>,
    counts: Arc<Counts>,
    authority: watch::Receiver<bool>,
}

impl RequestSupervisor {
    pub fn new(
        table: RouteTable,
        broker: Arc<dyn RequestBroker>,
        limits: RequestLimits,
        authority: watch::Receiver<bool>,
    ) -> Self {
        Self {
            table,
            broker,
            requests: Arc::new(Semaphore::new(limits.in_flight)),
            connections: Arc::new(Semaphore::new(limits.connections)),
            counts: Arc::new(Counts::default()),
            limits,
            authority,
        }
    }

    pub fn connection_capacity(&self) -> Arc<Semaphore> {
        Arc::clone(&self.connections)
    }

    pub fn snapshot(&self) -> Snapshot {
        Snapshot {
            in_flight: self.limits.in_flight - self.requests.available_permits(),
            in_flight_limit: self.limits.in_flight,
            high_water: self.counts.high_water.load(Ordering::Relaxed),
            rejected: self.counts.rejected.load(Ordering::Relaxed),
            completed: self.counts.completed.load(Ordering::Relaxed),
            cancelled: self.counts.cancelled.load(Ordering::Relaxed),
            connection_count: self.limits.connections - self.connections.available_permits(),
            connection_limit: self.limits.connections,
        }
    }

    pub async fn handle(&self, req: Request) -> Response {
        let mut authority = self.authority.clone();
        let authorized = *authority.borrow_and_update();
        if !authorized || authority.has_changed().is_err() {
            return StatusCode::SERVICE_UNAVAILABLE.into_response();
        }
        let route = match self.table.get(req.uri().path()) {
            Ok(Some(route)) => route,
            Ok(None) => return StatusCode::NOT_FOUND.into_response(),
            Err(_) => return StatusCode::INTERNAL_SERVER_ERROR.into_response(),
        };
        if !route.methods.is_empty()
            && !route
                .methods
                .iter()
                .any(|method| method.eq_ignore_ascii_case(req.method().as_str()))
        {
            return (
                StatusCode::METHOD_NOT_ALLOWED,
                [(header::ALLOW, route.methods.join(", "))],
            )
                .into_response();
        }
        let permit = match Arc::clone(&self.requests).try_acquire_owned() {
            Ok(permit) => permit,
            Err(_) => {
                self.counts.rejected.fetch_add(1, Ordering::Relaxed);
                return StatusCode::SERVICE_UNAVAILABLE.into_response();
            }
        };
        self.counts.high_water.fetch_max(
            self.limits.in_flight - self.requests.available_permits(),
            Ordering::Relaxed,
        );
        let lease = RequestLease {
            _permit: permit,
            counts: Arc::clone(&self.counts),
            completed: false,
        };
        let deadline = Instant::now() + self.limits.timeout;
        let dispatch_authority = authority.clone();
        tokio::select! {
            biased;
            _ = authority.changed() => {
                drop(lease);
                StatusCode::SERVICE_UNAVAILABLE.into_response()
            }
            response = self.dispatch(req, route.nats_subject, deadline, dispatch_authority) => {
                lease.reply(response, authority)
            }
        }
    }

    async fn dispatch(
        &self,
        req: Request,
        subject: String,
        deadline: Instant,
        authority: watch::Receiver<bool>,
    ) -> Response {
        let metadata_bytes = req.uri().to_string().len()
            + req.method().as_str().len()
            + req
                .headers()
                .iter()
                .map(|(name, value)| name.as_str().len() + value.as_bytes().len())
                .sum::<usize>();
        if metadata_bytes > self.limits.metadata_bytes {
            return StatusCode::REQUEST_HEADER_FIELDS_TOO_LARGE.into_response();
        }
        if req
            .headers()
            .get(header::CONTENT_LENGTH)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.parse::<u64>().ok())
            .is_some_and(|length| length > self.limits.body_bytes as u64)
        {
            return StatusCode::PAYLOAD_TOO_LARGE.into_response();
        }
        if !self.broker.is_available() {
            return StatusCode::SERVICE_UNAVAILABLE.into_response();
        }
        let (parts, body) = req.into_parts();
        let body_deadline = deadline.min(Instant::now() + self.limits.body_read_timeout);
        let body = match tokio::time::timeout_at(body_deadline, self.read_body(body)).await {
            Ok(Ok(body)) => body,
            Ok(Err(status)) => return status.into_response(),
            Err(_) => return StatusCode::REQUEST_TIMEOUT.into_response(),
        };
        let payload = HttpRequestPayload {
            method: parts.method.to_string(),
            path: parts.uri.path().to_string(),
            query: parts.uri.query().unwrap_or("").to_string(),
            headers: parts
                .headers
                .iter()
                .filter_map(|(name, value)| {
                    value
                        .to_str()
                        .ok()
                        .map(|value| (name.to_string(), value.to_string()))
                })
                .collect(),
            body: if body.is_empty() { None } else { Some(body) },
        };
        let mut serialized = BoundedPayload {
            bytes: Vec::with_capacity(self.limits.payload_bytes),
            limit: self.limits.payload_bytes,
        };
        if serde_json::to_writer(&mut serialized, &payload).is_err() {
            return StatusCode::PAYLOAD_TOO_LARGE.into_response();
        }
        if Instant::now() >= deadline {
            return StatusCode::GATEWAY_TIMEOUT.into_response();
        }
        if !*authority.borrow() {
            return StatusCode::SERVICE_UNAVAILABLE.into_response();
        }
        let reply = match self
            .broker
            .request(subject, serialized.bytes, deadline)
            .await
        {
            Ok(reply) => reply,
            Err(RequestError::Unavailable) => {
                return StatusCode::SERVICE_UNAVAILABLE.into_response();
            }
            Err(RequestError::PayloadTooLarge) => {
                return StatusCode::PAYLOAD_TOO_LARGE.into_response();
            }
            Err(RequestError::Failed) => return StatusCode::BAD_GATEWAY.into_response(),
            Err(RequestError::Timeout) => return StatusCode::GATEWAY_TIMEOUT.into_response(),
        };
        decode_response(&reply)
    }

    async fn read_body(&self, body: Body) -> Result<Vec<u8>, StatusCode> {
        let mut stream = body.into_data_stream();
        let mut bytes = Vec::new();
        while let Some(chunk) = stream.next().await {
            let chunk = chunk.map_err(|_| StatusCode::BAD_REQUEST)?;
            if chunk.len() > self.limits.body_bytes - bytes.len() {
                return Err(StatusCode::PAYLOAD_TOO_LARGE);
            }
            bytes.extend_from_slice(&chunk);
        }
        Ok(bytes)
    }
}

struct RequestLease {
    _permit: OwnedSemaphorePermit,
    counts: Arc<Counts>,
    completed: bool,
}

impl RequestLease {
    fn reply(mut self, response: Response, authority: watch::Receiver<bool>) -> Response {
        self.completed = true;
        let (parts, body) = response.into_parts();
        let body = stream::unfold(
            Some((body.into_data_stream(), self, authority)),
            |state| async {
                let (mut body, mut lease, mut authority) = state?;
                tokio::select! {
                    biased;
                    _ = authority.changed() => {
                        lease.completed = false;
                        Some((Err(axum::Error::new(io::Error::other("configuration authority lost"))), None))
                    }
                    chunk = body.next() => {
                        chunk.map(|chunk| (chunk, Some((body, lease, authority))))
                    }
                }
            },
        );
        Response::from_parts(parts, Body::from_stream(body))
    }
}

impl Drop for RequestLease {
    fn drop(&mut self) {
        let counter = if self.completed {
            &self.counts.completed
        } else {
            &self.counts.cancelled
        };
        counter.fetch_add(1, Ordering::Relaxed);
    }
}

struct BoundedPayload {
    bytes: Vec<u8>,
    limit: usize,
}

impl Write for BoundedPayload {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        if bytes.len() > self.limit - self.bytes.len() {
            return Err(io::Error::other("request payload exceeds byte limit"));
        }
        self.bytes.extend_from_slice(bytes);
        Ok(bytes.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

fn decode_response(reply: &[u8]) -> Response {
    let response: HttpResponsePayload = match serde_json::from_slice(reply) {
        Ok(response) => response,
        Err(_) => return StatusCode::BAD_GATEWAY.into_response(),
    };
    let status = StatusCode::from_u16(response.status).unwrap_or(StatusCode::INTERNAL_SERVER_ERROR);
    let mut headers = HeaderMap::new();
    for (name, value) in response.headers {
        if let (Ok(name), Ok(value)) = (
            HeaderName::from_bytes(name.as_bytes()),
            HeaderValue::from_str(&value),
        ) {
            headers.insert(name, value);
        }
    }
    (status, headers, response.body.unwrap_or_default()).into_response()
}

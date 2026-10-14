use axum::{
    Router,
    body::Bytes,
    extract::{DefaultBodyLimit, FromRequest, Path, Request, State},
    http::{StatusCode, header::CONTENT_TYPE},
    response::{IntoResponse, Response},
    routing::get,
};
use platform_common::{
    artifacts::{CACHE_MAX_BYTES, CACHE_MAX_ENTRIES, MAX_WASM_BYTES, Sha256Digest, verify_wasm},
    health,
};
use std::{
    collections::{HashMap, VecDeque},
    sync::{
        Arc,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
    time::Duration,
};
use tokio::sync::{Mutex, OwnedSemaphorePermit, Semaphore};

const MAX_UPLOADS: usize = 2;
const MAX_RESPONSES: usize = 2;
const BODY_TIMEOUT: Duration = Duration::from_secs(5);

struct OwnedResponse {
    bytes: Bytes,
    _permit: OwnedSemaphorePermit,
}

impl AsRef<[u8]> for OwnedResponse {
    fn as_ref(&self) -> &[u8] {
        &self.bytes
    }
}

#[derive(Default)]
struct CacheMetrics {
    hits: AtomicU64,
    misses: AtomicU64,
    stores: AtomicU64,
    rejected: AtomicU64,
    evictions: AtomicU64,
}

struct Store {
    entries: HashMap<Sha256Digest, Bytes>,
    recency: VecDeque<Sha256Digest>,
    bytes: usize,
    max_bytes: usize,
    max_entries: usize,
}

impl Store {
    fn touch(&mut self, digest: &Sha256Digest) {
        if let Some(index) = self.recency.iter().position(|entry| entry == digest) {
            self.recency.remove(index);
        }
        self.recency.push_back(digest.clone());
    }

    fn get(&mut self, digest: &Sha256Digest) -> Option<Bytes> {
        let bytes = self.entries.get(digest)?.clone();
        self.touch(digest);
        Some(bytes)
    }

    fn insert(&mut self, digest: Sha256Digest, bytes: Bytes) -> Result<usize, StatusCode> {
        if let Some(existing) = self.entries.get(&digest) {
            if existing != &bytes {
                return Err(StatusCode::CONFLICT);
            }
            self.touch(&digest);
            return Ok(0);
        }
        if bytes.len() > self.max_bytes || self.max_entries == 0 {
            return Err(StatusCode::PAYLOAD_TOO_LARGE);
        }
        let mut evictions = 0;
        while self.bytes + bytes.len() > self.max_bytes || self.entries.len() >= self.max_entries {
            let Some(oldest) = self.recency.pop_front() else {
                return Err(StatusCode::INTERNAL_SERVER_ERROR);
            };
            if let Some(old) = self.entries.remove(&oldest) {
                self.bytes -= old.len();
                evictions += 1;
            }
        }
        self.bytes += bytes.len();
        self.entries.insert(digest.clone(), bytes);
        self.touch(&digest);
        Ok(evictions)
    }
}

#[derive(Clone)]
pub(crate) struct CacheState {
    store: Arc<Mutex<Store>>,
    uploads: Arc<Semaphore>,
    responses: Arc<Semaphore>,
    metrics: Arc<CacheMetrics>,
    pub(crate) ready: Arc<AtomicBool>,
}

impl CacheState {
    pub(crate) fn new() -> Self {
        Self::with_limits(CACHE_MAX_BYTES, CACHE_MAX_ENTRIES)
    }

    fn with_limits(max_bytes: usize, max_entries: usize) -> Self {
        Self {
            store: Arc::new(Mutex::new(Store {
                entries: HashMap::new(),
                recency: VecDeque::new(),
                bytes: 0,
                max_bytes,
                max_entries,
            })),
            uploads: Arc::new(Semaphore::new(MAX_UPLOADS)),
            responses: Arc::new(Semaphore::new(MAX_RESPONSES)),
            metrics: Arc::new(CacheMetrics::default()),
            ready: Arc::new(AtomicBool::new(true)),
        }
    }

    fn reject(&self, status: StatusCode) -> Response {
        self.metrics.rejected.fetch_add(1, Ordering::Relaxed);
        status.into_response()
    }
}

pub(crate) fn router(state: CacheState) -> Router {
    Router::new()
        .route("/modules/{digest}", get(get_module).put(put_module))
        .route("/healthz", get(health::healthz_handler))
        .route("/readyz", get(readyz))
        .route("/metrics", get(metrics))
        .layer(DefaultBodyLimit::max(MAX_WASM_BYTES))
        .with_state(state)
}

async fn get_module(State(state): State<CacheState>, Path(digest): Path<String>) -> Response {
    if !state.ready.load(Ordering::Acquire) {
        return state.reject(StatusCode::SERVICE_UNAVAILABLE);
    }
    let Ok(digest) = Sha256Digest::parse(&digest) else {
        return state.reject(StatusCode::BAD_REQUEST);
    };
    match state.store.lock().await.get(&digest) {
        Some(bytes) => {
            let Ok(permit) = state.responses.clone().try_acquire_owned() else {
                return state.reject(StatusCode::TOO_MANY_REQUESTS);
            };
            state.metrics.hits.fetch_add(1, Ordering::Relaxed);
            (
                [(CONTENT_TYPE, "application/wasm")],
                Bytes::from_owner(OwnedResponse {
                    bytes,
                    _permit: permit,
                }),
            )
                .into_response()
        }
        None => {
            state.metrics.misses.fetch_add(1, Ordering::Relaxed);
            StatusCode::NOT_FOUND.into_response()
        }
    }
}

async fn put_module(
    State(state): State<CacheState>,
    Path(digest): Path<String>,
    request: Request,
) -> Response {
    if !state.ready.load(Ordering::Acquire) {
        return state.reject(StatusCode::SERVICE_UNAVAILABLE);
    }
    let Ok(digest) = Sha256Digest::parse(&digest) else {
        return state.reject(StatusCode::BAD_REQUEST);
    };
    let Ok(_permit) = state.uploads.try_acquire() else {
        return state.reject(StatusCode::TOO_MANY_REQUESTS);
    };
    let bytes = match tokio::time::timeout(BODY_TIMEOUT, Bytes::from_request(request, &state)).await
    {
        Ok(Ok(bytes)) => bytes,
        Ok(Err(rejection)) => return state.reject(rejection.status()),
        Err(_) => return state.reject(StatusCode::REQUEST_TIMEOUT),
    };
    if verify_wasm(&digest, &bytes).is_err() {
        return state.reject(StatusCode::UNPROCESSABLE_ENTITY);
    }
    let size = bytes.len();
    match state.store.lock().await.insert(digest.clone(), bytes) {
        Ok(evictions) => {
            state.metrics.stores.fetch_add(1, Ordering::Relaxed);
            state
                .metrics
                .evictions
                .fetch_add(evictions as u64, Ordering::Relaxed);
            tracing::info!(%digest, bytes = size, evictions, "raw Wasm stored");
            StatusCode::NO_CONTENT.into_response()
        }
        Err(status) => state.reject(status),
    }
}

async fn readyz(State(state): State<CacheState>) -> StatusCode {
    if state.ready.load(Ordering::Acquire) {
        StatusCode::OK
    } else {
        StatusCode::SERVICE_UNAVAILABLE
    }
}

async fn metrics(State(state): State<CacheState>) -> Response {
    let store = state.store.lock().await;
    let mut output = String::new();
    for (name, kind, value) in [
        ("module_cache_bytes", "gauge", store.bytes as u64),
        ("module_cache_entries", "gauge", store.entries.len() as u64),
        (
            "module_cache_uploads",
            "gauge",
            (MAX_UPLOADS - state.uploads.available_permits()) as u64,
        ),
        (
            "module_cache_responses",
            "gauge",
            (MAX_RESPONSES - state.responses.available_permits()) as u64,
        ),
        (
            "module_cache_hits_total",
            "counter",
            state.metrics.hits.load(Ordering::Relaxed),
        ),
        (
            "module_cache_misses_total",
            "counter",
            state.metrics.misses.load(Ordering::Relaxed),
        ),
        (
            "module_cache_stores_total",
            "counter",
            state.metrics.stores.load(Ordering::Relaxed),
        ),
        (
            "module_cache_rejections_total",
            "counter",
            state.metrics.rejected.load(Ordering::Relaxed),
        ),
        (
            "module_cache_evictions_total",
            "counter",
            state.metrics.evictions.load(Ordering::Relaxed),
        ),
    ] {
        use std::fmt::Write;
        let _ = writeln!(output, "# TYPE {name} {kind}\n{name} {value}");
    }
    ([(CONTENT_TYPE, "text/plain; version=0.0.4")], output).into_response()
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::body::{Body, to_bytes};
    use std::{convert::Infallible, task::Poll};
    use tower::ServiceExt;

    fn wasm(marker: u8) -> Bytes {
        Bytes::from(vec![0, b'a', b's', b'm', 13, 0, 1, 0, 0, 2, 1, marker])
    }

    fn request(method: &str, path: &str, body: Body) -> Request {
        Request::builder()
            .method(method)
            .uri(path)
            .body(body)
            .unwrap()
    }

    fn path(bytes: &[u8]) -> String {
        format!("/modules/{}", Sha256Digest::from_bytes(bytes))
    }

    async fn put(app: &Router, bytes: Bytes) -> StatusCode {
        app.clone()
            .oneshot(request("PUT", &path(&bytes), Body::from(bytes)))
            .await
            .unwrap()
            .status()
    }

    async fn get(app: &Router, bytes: &[u8]) -> Response {
        app.clone()
            .oneshot(request("GET", &path(bytes), Body::empty()))
            .await
            .unwrap()
    }

    #[tokio::test]
    async fn raw_wasm_round_trips_and_cannot_be_replaced_under_another_digest() {
        let state = CacheState::new();
        let app = router(state.clone());
        let first = wasm(b'a');
        assert_eq!(put(&app, first.clone()).await, StatusCode::NO_CONTENT);
        assert_eq!(put(&app, first.clone()).await, StatusCode::NO_CONTENT);
        let response = app
            .clone()
            .oneshot(request("PUT", &path(&first), Body::from(wasm(b'b'))))
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::UNPROCESSABLE_ENTITY);
        let response = get(&app, &first).await;
        assert_eq!(response.status(), StatusCode::OK);
        assert_eq!(response.headers()[CONTENT_TYPE], "application/wasm");
        assert_eq!(
            to_bytes(response.into_body(), MAX_WASM_BYTES)
                .await
                .unwrap(),
            first
        );
        assert_eq!(state.store.lock().await.entries.len(), 1);
    }

    #[tokio::test]
    async fn native_payloads_malformed_keys_and_legacy_routes_are_rejected() {
        let state = CacheState::new();
        let app = router(state.clone());
        assert_eq!(
            put(&app, Bytes::from_static(b"\x7fELFnative artifact")).await,
            StatusCode::UNPROCESSABLE_ENTITY
        );
        let response = app
            .clone()
            .oneshot(request(
                "PUT",
                "/modules/not-a-digest",
                Body::from(wasm(b'a')),
            ))
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::BAD_REQUEST);
        let response = app
            .oneshot(request(
                "GET",
                "/modules/digest/arch/version",
                Body::empty(),
            ))
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::NOT_FOUND);
        assert!(state.store.lock().await.entries.is_empty());
    }

    #[tokio::test]
    async fn raw_wasm_above_the_default_extractor_limit_is_supported_and_oversize_is_rejected() {
        let state = CacheState::new();
        let app = router(state.clone());
        let mut large = vec![0; 3 * 1024 * 1024];
        large[..8].copy_from_slice(b"\0asm\x0d\0\x01\0");
        let large = Bytes::from(large);
        assert_eq!(put(&app, large.clone()).await, StatusCode::NO_CONTENT);
        assert_eq!(
            to_bytes(get(&app, &large).await.into_body(), MAX_WASM_BYTES)
                .await
                .unwrap(),
            large
        );
        let response = app
            .clone()
            .oneshot(request(
                "PUT",
                &path(&wasm(b'a')),
                Body::from(vec![0; MAX_WASM_BYTES + 1]),
            ))
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::PAYLOAD_TOO_LARGE);
        assert_eq!(state.store.lock().await.entries.len(), 1);
    }

    #[tokio::test]
    async fn least_recently_used_entries_are_evicted_at_byte_and_entry_limits() {
        for state in [
            CacheState::with_limits(24, 256),
            CacheState::with_limits(CACHE_MAX_BYTES, 2),
        ] {
            let app = router(state.clone());
            let first = wasm(b'a');
            let second = wasm(b'b');
            let third = wasm(b'c');
            assert_eq!(put(&app, first.clone()).await, StatusCode::NO_CONTENT);
            assert_eq!(put(&app, second.clone()).await, StatusCode::NO_CONTENT);
            assert_eq!(get(&app, &first).await.status(), StatusCode::OK);
            assert_eq!(put(&app, third.clone()).await, StatusCode::NO_CONTENT);
            assert_eq!(get(&app, &second).await.status(), StatusCode::NOT_FOUND);
            assert_eq!(get(&app, &first).await.status(), StatusCode::OK);
            assert_eq!(get(&app, &third).await.status(), StatusCode::OK);
            let store = state.store.lock().await;
            assert_eq!(store.bytes, 24);
            assert_eq!(store.entries.len(), 2);
            assert_eq!(state.metrics.evictions.load(Ordering::Relaxed), 1);
        }
    }

    #[tokio::test]
    async fn response_permits_survive_eviction_and_transport_buffer_clones() {
        let state = CacheState::with_limits(12, 1);
        let app = router(state.clone());
        let first = wasm(b'a');
        let second = wasm(b'b');
        assert_eq!(put(&app, first.clone()).await, StatusCode::NO_CONTENT);
        let held_first = get(&app, &first).await;
        assert_eq!(held_first.status(), StatusCode::OK);
        assert_eq!(put(&app, second.clone()).await, StatusCode::NO_CONTENT);
        let held_second = get(&app, &second).await;
        assert_eq!(held_second.status(), StatusCode::OK);
        assert_eq!(
            get(&app, &second).await.status(),
            StatusCode::TOO_MANY_REQUESTS
        );
        assert_eq!(state.responses.available_permits(), 0);
        let downloaded = to_bytes(held_first.into_body(), MAX_WASM_BYTES)
            .await
            .unwrap();
        let buffered = downloaded.clone();
        assert_eq!(downloaded, first);
        assert_eq!(state.responses.available_permits(), 0);
        drop(downloaded);
        assert_eq!(state.responses.available_permits(), 0);
        drop(buffered);
        assert_eq!(state.responses.available_permits(), 1);
        drop(held_second);
        assert_eq!(state.responses.available_permits(), MAX_RESPONSES);
        assert_eq!(get(&app, &second).await.status(), StatusCode::OK);
    }

    #[tokio::test(start_paused = true)]
    async fn stalled_uploads_bound_intake_keep_probes_available_and_release_capacity_on_deadline() {
        let state = CacheState::new();
        let app = router(state.clone());
        let mut stalled = Vec::new();
        for marker in [b'a', b'b'] {
            let (started_tx, started_rx) = tokio::sync::oneshot::channel();
            let mut started_tx = Some(started_tx);
            let body = Body::from_stream(futures_util::stream::poll_fn(move |_| {
                if let Some(started_tx) = started_tx.take() {
                    let _ = started_tx.send(());
                }
                Poll::Pending::<Option<Result<Bytes, Infallible>>>
            }));
            let app = app.clone();
            stalled.push(tokio::spawn(async move {
                app.oneshot(request("PUT", &path(&wasm(marker)), body))
                    .await
                    .unwrap()
            }));
            started_rx.await.unwrap();
        }
        assert_eq!(state.uploads.available_permits(), 0);
        assert_eq!(put(&app, wasm(b'c')).await, StatusCode::TOO_MANY_REQUESTS);
        for route in ["/healthz", "/readyz", "/metrics"] {
            assert_eq!(
                app.clone()
                    .oneshot(request("GET", route, Body::empty()))
                    .await
                    .unwrap()
                    .status(),
                StatusCode::OK
            );
        }
        tokio::time::advance(BODY_TIMEOUT).await;
        for task in stalled {
            assert_eq!(task.await.unwrap().status(), StatusCode::REQUEST_TIMEOUT);
        }
        assert_eq!(put(&app, wasm(b'c')).await, StatusCode::NO_CONTENT);
        assert_eq!(state.store.lock().await.entries.len(), 1);
    }

    #[tokio::test]
    async fn stopping_rejects_new_work_and_metrics_report_storage_bounds() {
        let state = CacheState::new();
        let app = router(state.clone());
        assert_eq!(put(&app, wasm(b'a')).await, StatusCode::NO_CONTENT);
        let response = app
            .clone()
            .oneshot(request("GET", "/metrics", Body::empty()))
            .await
            .unwrap();
        let body = to_bytes(response.into_body(), 4096).await.unwrap();
        let text = std::str::from_utf8(&body).unwrap();
        assert!(text.contains("module_cache_bytes 12\n"));
        assert!(text.contains("module_cache_entries 1\n"));
        state.ready.store(false, Ordering::Release);
        assert_eq!(put(&app, wasm(b'b')).await, StatusCode::SERVICE_UNAVAILABLE);
        assert_eq!(
            get(&app, &wasm(b'a')).await.status(),
            StatusCode::SERVICE_UNAVAILABLE
        );
        assert_eq!(
            app.clone()
                .oneshot(request("GET", "/readyz", Body::empty()))
                .await
                .unwrap()
                .status(),
            StatusCode::SERVICE_UNAVAILABLE
        );
        assert_eq!(
            app.oneshot(request("GET", "/healthz", Body::empty()))
                .await
                .unwrap()
                .status(),
            StatusCode::OK
        );
    }
}

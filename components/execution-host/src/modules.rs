use std::{
    collections::HashMap,
    sync::{Arc, RwLock},
    time::Duration,
};

use anyhow::{Result, bail, ensure};
use platform_common::artifacts::{Sha256Digest, verify_wasm};
use tokio::sync::{OwnedSemaphorePermit, Semaphore};
use tokio::time::Instant;
use wasmtime::{Engine, component::Component};

use crate::{
    metrics::MetricsRegistry,
    module_cache::ModuleCacheClient,
    oci::{OciSource, WasmArtifact},
    preparation_metrics::{
        PreparationControl, PreparationMetrics, PreparationOutcome, PreparationWork,
    },
};

const MAX_QUEUED_PREPARATIONS: usize = 16;
const MAX_RETAINED_COMPONENTS: usize = 32;
const MAX_RETAINED_NATIVE_BYTES: usize = 128 * 1024 * 1024;
const PREPARATION_TIMEOUT: Duration = Duration::from_secs(30);

type ModuleKey = (String, String, String);

struct RegisteredComponent {
    component: Arc<Component>,
    native_bytes: usize,
}

#[derive(Default)]
struct RegistryState {
    components: HashMap<ModuleKey, RegisteredComponent>,
    native_bytes: usize,
}

struct PreparationPermits {
    // Drop accounting before releasing capacity so a new owner cannot overlap it.
    work: PreparationWork,
    _active: Option<OwnedSemaphorePermit>,
    _record: OwnedSemaphorePermit,
}

struct PreparationCancellation {
    control: PreparationControl,
    armed: bool,
}

impl PreparationCancellation {
    fn disarm(&mut self) {
        self.armed = false;
    }
}

impl Drop for PreparationCancellation {
    fn drop(&mut self) {
        if self.armed {
            self.control.cancelled();
        }
    }
}

struct PreparedComponent {
    component: Component,
    native_bytes: usize,
    digest: Sha256Digest,
    cache_write: Option<Vec<u8>>,
    _permits: PreparationPermits,
}

#[cfg(test)]
struct CompilationGate {
    entered: tokio::sync::Notify,
    release: std::sync::Mutex<std::sync::mpsc::Receiver<()>>,
}

/// Locally compiled components and bounded preparation admission.
#[derive(Clone)]
pub struct ModuleRegistry {
    inner: Arc<RwLock<RegistryState>>,
    cache: Arc<ModuleCacheClient>,
    source: OciSource,
    engine: Engine,
    metrics: MetricsRegistry,
    preparation_metrics: PreparationMetrics,
    records: Arc<Semaphore>,
    active: Arc<Semaphore>,
    #[cfg(test)]
    compilation_gate: Option<Arc<CompilationGate>>,
}

impl ModuleRegistry {
    pub fn new(
        cache_base_url: String,
        engine: Engine,
        metrics: MetricsRegistry,
        source: OciSource,
    ) -> Result<Self> {
        let preparation_metrics = metrics.preparation(MAX_QUEUED_PREPARATIONS + 1, 1)?;
        Ok(Self {
            inner: Arc::new(RwLock::new(RegistryState::default())),
            cache: Arc::new(ModuleCacheClient::new(cache_base_url)?),
            source,
            engine,
            metrics,
            preparation_metrics,
            records: Arc::new(Semaphore::new(MAX_QUEUED_PREPARATIONS + 1)),
            active: Arc::new(Semaphore::new(1)),
            #[cfg(test)]
            compilation_gate: None,
        })
    }

    pub fn get(
        &self,
        namespace: &str,
        app_name: &str,
        fn_name: &str,
    ) -> Result<Option<Arc<Component>>> {
        let state = self
            .inner
            .read()
            .map_err(|_| anyhow::anyhow!("module registry lock poisoned"))?;
        Ok(state
            .components
            .get(&module_key(namespace, app_name, fn_name))
            .map(|entry| Arc::clone(&entry.component)))
    }

    /// The config-sync caller waits for bounded capacity before creating a preparation task.
    pub async fn submit_load(
        &self,
        namespace: String,
        app_name: String,
        fn_name: String,
        module_ref: String,
    ) -> Result<()> {
        let submission = self.preparation_metrics.submission();
        let record = match tokio::time::timeout(
            PREPARATION_TIMEOUT,
            Arc::clone(&self.records).acquire_owned(),
        )
        .await
        {
            Ok(Ok(record)) => record,
            Ok(Err(_)) => {
                submission.closed();
                return Err(anyhow::anyhow!("module preparation admission closed"));
            }
            Err(_) => {
                submission.timed_out();
                return Err(anyhow::anyhow!(
                    "module submission capacity deadline expired"
                ));
            }
        };
        let permits = PreparationPermits {
            work: submission.admitted(),
            _active: None,
            _record: record,
        };
        let deadline = Instant::now() + PREPARATION_TIMEOUT;
        let registry = self.clone();
        tokio::spawn(async move {
            if let Err(error) = registry
                .load_admitted(
                    &namespace,
                    &app_name,
                    &fn_name,
                    &module_ref,
                    permits,
                    deadline,
                )
                .await
            {
                tracing::error!(namespace, app_name, function_name = fn_name,
                    error_kind = "module_preparation", reason = %error,
                    "failed to prepare module");
            }
        });
        Ok(())
    }

    #[cfg(test)]
    fn admit(&self) -> Result<OwnedSemaphorePermit> {
        Arc::clone(&self.records)
            .try_acquire_owned()
            .map_err(|_| anyhow::anyhow!("module preparation queue is full"))
    }

    #[cfg(test)]
    async fn load(
        &self,
        namespace: &str,
        app_name: &str,
        fn_name: &str,
        module_ref: &str,
    ) -> Result<()> {
        let record = self.admit()?;
        let permits = PreparationPermits {
            work: self.preparation_metrics.submission().admitted(),
            _active: None,
            _record: record,
        };
        self.load_admitted(
            namespace,
            app_name,
            fn_name,
            module_ref,
            permits,
            Instant::now() + PREPARATION_TIMEOUT,
        )
        .await
    }

    async fn load_admitted(
        &self,
        namespace: &str,
        app_name: &str,
        fn_name: &str,
        module_ref: &str,
        permits: PreparationPermits,
        deadline: Instant,
    ) -> Result<()> {
        let control = permits.work.control();
        let preparation = self.prepare(
            module_ref,
            permits,
            deadline,
            module_key(namespace, app_name, fn_name),
        );
        tokio::pin!(preparation);
        let mut cancellation = PreparationCancellation {
            control: control.clone(),
            armed: true,
        };
        let prepared = tokio::select! {
            biased;
            _ = tokio::time::sleep_until(deadline) => {
                control.timed_out_at(deadline.into_std());
                return Err(anyhow::anyhow!("module preparation deadline expired"));
            }
            prepared = &mut preparation => prepared,
        };
        let mut prepared = match prepared {
            Ok(prepared) => prepared,
            Err(error) => {
                cancellation.disarm();
                return Err(error);
            }
        };
        let registration = (|| -> Result<()> {
            let mut state = self
                .inner
                .write()
                .map_err(|_| anyhow::anyhow!("module registry lock poisoned"))?;
            ensure!(
                Instant::now() < deadline,
                "module preparation deadline expired"
            );
            let key = module_key(namespace, app_name, fn_name);
            let replaced_bytes = state
                .components
                .get(&key)
                .map_or(0, |entry| entry.native_bytes);
            let retained_bytes = state.native_bytes - replaced_bytes + prepared.native_bytes;
            ensure!(
                state.components.contains_key(&key)
                    || state.components.len() < MAX_RETAINED_COMPONENTS,
                "module registry component count limit exceeded"
            );
            ensure!(
                retained_bytes <= MAX_RETAINED_NATIVE_BYTES,
                "module registry native byte budget exceeded"
            );
            state.components.insert(
                key,
                RegisteredComponent {
                    component: Arc::new(prepared.component),
                    native_bytes: prepared.native_bytes,
                },
            );
            state.native_bytes = retained_bytes;
            Ok(())
        })();
        if let Err(error) = registration {
            if Instant::now() >= deadline {
                control.timed_out_at(deadline.into_std());
            }
            prepared._permits.work.finish(PreparationOutcome::Error);
            cancellation.disarm();
            return Err(error);
        }
        tracing::info!(namespace, app_name, fn_name, "module registered");
        if let Some(wasm) = prepared.cache_write.take() {
            match tokio::time::timeout_at(deadline, self.cache.put(&prepared.digest, wasm)).await {
                Ok(Ok(())) => {}
                Ok(Err(error)) => {
                    tracing::warn!(error_kind = "cache_put", reason = %error, "raw module cache write failed")
                }
                Err(_) => tracing::warn!(
                    error_kind = "cache_put_timeout",
                    "raw module cache write deadline expired"
                ),
            }
        }
        prepared._permits.work.finish(PreparationOutcome::Success);
        cancellation.disarm();
        Ok(())
    }

    async fn prepare(
        &self,
        module_ref: &str,
        mut permits: PreparationPermits,
        deadline: Instant,
        identity: ModuleKey,
    ) -> Result<PreparedComponent> {
        permits.work.finish(PreparationOutcome::Error);
        let active = Arc::clone(&self.active)
            .acquire_owned()
            .await
            .map_err(|_| anyhow::anyhow!("module preparation admission closed"))?;
        permits._active = Some(active);
        permits.work.activate();
        let artifact = self.source.manifest(module_ref).await?;
        let cached = match self.cache.get(&artifact.digest).await {
            Ok(Some(wasm)) => match validate_bytes(&artifact, &wasm) {
                Ok(()) => Some(wasm),
                Err(error) => {
                    tracing::warn!(error_kind = "cache_integrity", reason = %error, "raw module cache hit rejected");
                    None
                }
            },
            Ok(None) => None,
            Err(error) => {
                tracing::warn!(error_kind = "cache_get", reason = %error, "raw module cache lookup failed");
                None
            }
        };
        let cache_miss = cached.is_none();
        let wasm = match cached {
            Some(wasm) => wasm,
            None => self.source.pull(&artifact).await?,
        };
        validate_bytes(&artifact, &wasm)?;
        let engine = self.engine.clone();
        let metrics = self.metrics.clone();
        #[cfg(test)]
        let compilation_gate = self.compilation_gate.clone();
        tokio::task::spawn_blocking(move || {
            if Instant::now() >= deadline {
                permits.work.control().timed_out_at(deadline.into_std());
            }
            permits.work.compiling();
            #[cfg(test)]
            if let Some(gate) = compilation_gate {
                gate.entered.notify_one();
                let _ = gate.release.lock().unwrap().recv();
            }
            let compiled = (|| -> Result<(Component, usize)> {
                let component = Component::new(&engine, &wasm)
                    .map_err(|_| anyhow::anyhow!("Wasm component compilation failed"))?;
                let native_bytes = component
                    .serialize()
                    .map_err(|_| anyhow::anyhow!("Wasm component size measurement failed"))?
                    .len();
                ensure!(
                    native_bytes <= MAX_RETAINED_NATIVE_BYTES,
                    "compiled component exceeds native byte budget"
                );
                Ok((component, native_bytes))
            })();
            let (outcome, result) = if compiled.is_ok() {
                (PreparationOutcome::Success, "ok")
            } else {
                (PreparationOutcome::Error, "err")
            };
            metrics.record_compilation(&identity.1, &identity.0, result);
            if Instant::now() >= deadline {
                permits.work.control().timed_out_at(deadline.into_std());
            }
            permits.work.compilation_finished(outcome);
            let (component, native_bytes) = compiled?;
            permits.work.finish(PreparationOutcome::Cancelled);
            Ok(PreparedComponent {
                component,
                native_bytes,
                digest: artifact.digest,
                cache_write: cache_miss.then_some(wasm),
                _permits: permits,
            })
        })
        .await
        .map_err(|_| anyhow::anyhow!("Wasm component compilation task failed"))?
    }

    pub fn remove(&self, namespace: &str, app_name: &str, fn_name: &str) -> Result<()> {
        let mut state = self
            .inner
            .write()
            .map_err(|_| anyhow::anyhow!("module registry lock poisoned"))?;
        if let Some(entry) = state
            .components
            .remove(&module_key(namespace, app_name, fn_name))
        {
            state.native_bytes -= entry.native_bytes;
        }
        Ok(())
    }
}

fn module_key(namespace: &str, app_name: &str, fn_name: &str) -> ModuleKey {
    (
        namespace.to_string(),
        app_name.to_string(),
        fn_name.to_string(),
    )
}

fn validate_bytes(artifact: &WasmArtifact, wasm: &[u8]) -> Result<()> {
    if wasm.len() != artifact.size {
        bail!("Wasm artifact size mismatch");
    }
    verify_wasm(&artifact.digest, wasm)
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::{
        Router,
        body::{Body, Bytes},
        extract::Path,
        http::{Response, StatusCode},
        routing::get,
    };
    use std::sync::{
        Mutex,
        atomic::{AtomicUsize, Ordering},
    };

    const EMPTY_COMPONENT: &[u8] = b"\0asm\x0d\0\x01\0";

    #[derive(Clone)]
    enum CacheResponse {
        Hit(Vec<u8>),
        Miss,
        Unavailable,
    }

    #[derive(Debug, Eq, PartialEq)]
    struct CacheWrite {
        digest: String,
        wasm: Vec<u8>,
    }

    struct Fixture {
        registry: ModuleRegistry,
        pinned: String,
        digest: Sha256Digest,
        pulls: Arc<AtomicUsize>,
        puts: Arc<Mutex<Vec<CacheWrite>>>,
        servers: Vec<tokio::task::JoinHandle<()>>,
    }

    impl Drop for Fixture {
        fn drop(&mut self) {
            for server in &self.servers {
                server.abort();
            }
        }
    }

    impl Fixture {
        async fn new(
            cache_response: CacheResponse,
            expected: Vec<u8>,
            blob: Vec<u8>,
            stall_put: bool,
        ) -> Self {
            let digest = Sha256Digest::from_bytes(&expected);
            let manifest = serde_json::to_vec(&serde_json::json!({
                "schemaVersion": 2,
                "mediaType": "application/vnd.oci.image.manifest.v1+json",
                "config": {
                    "mediaType": "application/vnd.oci.empty.v1+json",
                    "digest": Sha256Digest::from_bytes(b"{}").as_str(), "size": 2,
                },
                "layers": [{ "mediaType": "application/wasm", "digest": digest.as_str(), "size": expected.len() }],
            })).unwrap();
            let manifest_digest = Sha256Digest::from_bytes(&manifest);
            let pulls = Arc::new(AtomicUsize::new(0));
            let pulls_for_route = Arc::clone(&pulls);
            let source_router = Router::new()
                .route(
                    &format!("/v2/app/manifests/{manifest_digest}"),
                    get(move || {
                        let manifest = manifest.clone();
                        async move { manifest }
                    }),
                )
                .route(
                    &format!("/v2/app/blobs/{digest}"),
                    get(move || {
                        let pulls = Arc::clone(&pulls_for_route);
                        let blob = blob.clone();
                        async move {
                            pulls.fetch_add(1, Ordering::SeqCst);
                            blob
                        }
                    }),
                );
            let (source_origin, source_server) = serve(source_router).await;
            let puts = Arc::new(Mutex::new(Vec::new()));
            let puts_for_route = Arc::clone(&puts);
            let cache_router = Router::new().route(
                "/modules/{digest}",
                get(move || {
                    let response = cache_response.clone();
                    async move {
                        let (status, bytes) = match response {
                            CacheResponse::Hit(bytes) => (StatusCode::OK, bytes),
                            CacheResponse::Miss => (StatusCode::NOT_FOUND, vec![]),
                            CacheResponse::Unavailable => (StatusCode::SERVICE_UNAVAILABLE, vec![]),
                        };
                        Response::builder()
                            .status(status)
                            .body(Body::from(bytes))
                            .unwrap()
                    }
                })
                .put(move |Path(identity): Path<String>, bytes: Bytes| {
                    let puts = Arc::clone(&puts_for_route);
                    async move {
                        puts.lock().unwrap().push(CacheWrite {
                            digest: identity,
                            wasm: bytes.to_vec(),
                        });
                        if stall_put {
                            std::future::pending::<()>().await;
                        }
                        StatusCode::SERVICE_UNAVAILABLE
                    }
                }),
            );
            let (cache_origin, cache_server) = serve(cache_router).await;
            let registry = ModuleRegistry::new(
                format!("http://{cache_origin}"),
                Engine::default(),
                MetricsRegistry::new().unwrap(),
                OciSource::new(&source_origin, &source_origin).unwrap(),
            )
            .unwrap();
            Self {
                registry,
                pinned: format!("oci://{source_origin}/app@{manifest_digest}"),
                digest,
                pulls,
                puts,
                servers: vec![source_server, cache_server],
            }
        }

        async fn load(&self, function: &str) -> Result<()> {
            tokio::time::timeout(
                Duration::from_secs(5),
                self.registry.load("test", "app", function, &self.pinned),
            )
            .await
            .expect("module load exceeded test deadline")
        }

        fn preparation_sample(&self, suffix: &str) -> f64 {
            let name = format!("wasm_host_preparation_{suffix}");
            let rendered = self.registry.metrics.render().unwrap();
            let sample = rendered
                .lines()
                .find_map(|line| {
                    let (metric, value) = line.split_once(' ')?;
                    (metric == name).then_some(value)
                })
                .unwrap_or_else(|| panic!("missing preparation sample {name}"));
            sample.parse().unwrap()
        }

        fn assert_preparation_idle(&self) {
            for state in [
                "admission_wait",
                "queued",
                "preparing",
                "compiling",
                "terminating",
            ] {
                assert_eq!(
                    self.preparation_sample(&format!("work{{state=\"{state}\"}}")),
                    0.0
                );
                assert_eq!(
                    self.preparation_sample(&format!(
                        "accounting_errors_total{{state=\"{state}\"}}"
                    )),
                    0.0
                );
            }
            for kind in ["queued", "active", "tracked", "compiling"] {
                assert_eq!(
                    self.preparation_sample(&format!("occupancy{{kind=\"{kind}\"}}")),
                    0.0
                );
                assert_eq!(
                    self.preparation_sample(&format!("capacity_breaches_total{{kind=\"{kind}\"}}")),
                    0.0
                );
            }
        }

        fn gate_compilation(&mut self) -> (Arc<CompilationGate>, std::sync::mpsc::Sender<()>) {
            let (release_tx, release_rx) = std::sync::mpsc::channel();
            let gate = Arc::new(CompilationGate {
                entered: tokio::sync::Notify::new(),
                release: Mutex::new(release_rx),
            });
            self.registry.compilation_gate = Some(Arc::clone(&gate));
            (gate, release_tx)
        }
    }

    async fn serve(router: Router) -> (String, tokio::task::JoinHandle<()>) {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let origin = listener.local_addr().unwrap().to_string();
        let server = tokio::spawn(async move { axum::serve(listener, router).await.unwrap() });
        (origin, server)
    }

    #[tokio::test]
    async fn verified_raw_cache_hit_is_compiled_locally_without_blob_download() {
        let fixture = Fixture::new(
            CacheResponse::Hit(EMPTY_COMPONENT.to_vec()),
            EMPTY_COMPONENT.to_vec(),
            vec![],
            false,
        )
        .await;
        fixture.load("handler").await.unwrap();
        assert!(
            fixture
                .registry
                .get("test", "app", "handler")
                .unwrap()
                .is_some()
        );
        assert_eq!(fixture.pulls.load(Ordering::SeqCst), 0);
        assert!(fixture.puts.lock().unwrap().is_empty());
    }

    #[tokio::test]
    async fn native_or_corrupted_cache_hits_fall_back_to_verified_raw_source() {
        let native = Component::new(&Engine::default(), EMPTY_COMPONENT)
            .unwrap()
            .serialize()
            .unwrap();
        for invalid in [native, b"\0asm\x0d\0\x01\x01".to_vec()] {
            let fixture = Fixture::new(
                CacheResponse::Hit(invalid),
                EMPTY_COMPONENT.to_vec(),
                EMPTY_COMPONENT.to_vec(),
                false,
            )
            .await;
            fixture.load("handler").await.unwrap();
            assert!(
                fixture
                    .registry
                    .get("test", "app", "handler")
                    .unwrap()
                    .is_some()
            );
            assert_eq!(fixture.pulls.load(Ordering::SeqCst), 1);
            assert_eq!(
                fixture.puts.lock().unwrap().as_slice(),
                &[CacheWrite {
                    digest: fixture.digest.to_string(),
                    wasm: EMPTY_COMPONENT.to_vec(),
                }]
            );
        }
    }

    #[tokio::test]
    async fn cache_outage_and_stalled_put_do_not_prevent_registration() {
        for (cache_response, stall_put) in [
            (CacheResponse::Unavailable, false),
            (CacheResponse::Miss, true),
        ] {
            let fixture = Fixture::new(
                cache_response,
                EMPTY_COMPONENT.to_vec(),
                EMPTY_COMPONENT.to_vec(),
                stall_put,
            )
            .await;
            fixture.load("handler").await.unwrap();
            assert!(
                fixture
                    .registry
                    .get("test", "app", "handler")
                    .unwrap()
                    .is_some()
            );
            assert_eq!(fixture.pulls.load(Ordering::SeqCst), 1);
            assert_eq!(fixture.puts.lock().unwrap().len(), 1);
        }
    }

    #[tokio::test]
    async fn corrupted_fallback_or_invalid_component_is_never_registered_or_cached() {
        let invalid_component = b"\0asm\x0d\0\x01\0\x01\xff".to_vec();
        for (expected, blob, compilation_errors) in [
            (EMPTY_COMPONENT.to_vec(), b"tampered".to_vec(), 0.0),
            (invalid_component.clone(), invalid_component, 1.0),
        ] {
            let fixture = Fixture::new(CacheResponse::Miss, expected, blob, false).await;
            assert!(fixture.load("handler").await.is_err());
            assert!(
                fixture
                    .registry
                    .get("test", "app", "handler")
                    .unwrap()
                    .is_none()
            );
            assert!(fixture.puts.lock().unwrap().is_empty());
            fixture.assert_preparation_idle();
            assert_eq!(
                fixture.preparation_sample("finished_total{outcome=\"error\"}"),
                1.0
            );
            assert_eq!(
                fixture.preparation_sample("compilation_finished_total{outcome=\"error\"}"),
                compilation_errors
            );
        }
    }

    #[tokio::test]
    async fn retained_count_rejects_growth_and_removal_releases_capacity() {
        let fixture = Fixture::new(
            CacheResponse::Hit(EMPTY_COMPONENT.to_vec()),
            EMPTY_COMPONENT.to_vec(),
            vec![],
            false,
        )
        .await;
        for function in 0..MAX_RETAINED_COMPONENTS {
            fixture.load(&function.to_string()).await.unwrap();
        }
        let before = fixture.registry.inner.read().unwrap().native_bytes;
        assert!(
            fixture
                .load("overflow")
                .await
                .unwrap_err()
                .to_string()
                .contains("count limit")
        );
        assert_eq!(fixture.registry.inner.read().unwrap().native_bytes, before);
        fixture.load("0").await.unwrap();
        assert_eq!(fixture.registry.inner.read().unwrap().native_bytes, before);
        fixture.registry.remove("test", "app", "0").unwrap();
        assert!(fixture.registry.inner.read().unwrap().native_bytes < before);
        fixture.load("overflow").await.unwrap();
        assert_eq!(
            fixture.registry.inner.read().unwrap().components.len(),
            MAX_RETAINED_COMPONENTS
        );
    }

    #[tokio::test]
    async fn retained_native_byte_budget_rejects_component_without_publication() {
        let fixture = Fixture::new(
            CacheResponse::Hit(EMPTY_COMPONENT.to_vec()),
            EMPTY_COMPONENT.to_vec(),
            vec![],
            false,
        )
        .await;
        fixture.registry.inner.write().unwrap().native_bytes = MAX_RETAINED_NATIVE_BYTES;
        assert!(
            fixture
                .load("handler")
                .await
                .unwrap_err()
                .to_string()
                .contains("byte budget")
        );
        assert!(
            fixture
                .registry
                .get("test", "app", "handler")
                .unwrap()
                .is_none()
        );
    }

    #[tokio::test]
    async fn config_submission_waits_for_capacity_and_prepares_more_than_one_queue() {
        let fixture = Fixture::new(
            CacheResponse::Hit(EMPTY_COMPONENT.to_vec()),
            EMPTY_COMPONENT.to_vec(),
            vec![],
            false,
        )
        .await;
        for function in 0..=MAX_QUEUED_PREPARATIONS {
            fixture
                .registry
                .submit_load(
                    "test".into(),
                    "app".into(),
                    function.to_string(),
                    fixture.pinned.clone(),
                )
                .await
                .unwrap();
        }
        assert_eq!(fixture.registry.records.available_permits(), 0);
        tokio::time::timeout(
            Duration::from_secs(5),
            fixture.registry.submit_load(
                "test".into(),
                "app".into(),
                "overflow".into(),
                fixture.pinned.clone(),
            ),
        )
        .await
        .unwrap()
        .unwrap();
        tokio::time::timeout(Duration::from_secs(5), async {
            while fixture.registry.records.available_permits() != MAX_QUEUED_PREPARATIONS + 1
                || fixture.registry.inner.read().unwrap().components.len()
                    != MAX_QUEUED_PREPARATIONS + 2
            {
                tokio::time::sleep(Duration::from_millis(1)).await;
            }
        })
        .await
        .unwrap();
        assert_eq!(
            fixture.registry.records.available_permits(),
            MAX_QUEUED_PREPARATIONS + 1
        );
        assert_eq!(fixture.pulls.load(Ordering::SeqCst), 0);
        fixture.assert_preparation_idle();
        assert_eq!(
            fixture.preparation_sample("finished_total{outcome=\"success\"}"),
            (MAX_QUEUED_PREPARATIONS + 2) as f64
        );
        assert_eq!(
            fixture.preparation_sample("submissions_total{outcome=\"admitted\"}"),
            (MAX_QUEUED_PREPARATIONS + 2) as f64
        );
        assert_eq!(
            fixture.preparation_sample("compilation_seconds_count"),
            (MAX_QUEUED_PREPARATIONS + 2) as f64
        );
        assert_eq!(
            fixture.preparation_sample("high_water{kind=\"tracked\"}"),
            (MAX_QUEUED_PREPARATIONS + 1) as f64
        );
    }

    #[tokio::test]
    async fn saturated_config_submission_has_a_deadline_and_can_retry_after_release() {
        let fixture = Fixture::new(
            CacheResponse::Hit(EMPTY_COMPONENT.to_vec()),
            EMPTY_COMPONENT.to_vec(),
            vec![],
            false,
        )
        .await;
        let held: Vec<_> = (0..=MAX_QUEUED_PREPARATIONS)
            .map(|_| fixture.registry.admit().unwrap())
            .collect();
        tokio::time::pause();
        let registry = fixture.registry.clone();
        let pinned = fixture.pinned.clone();
        let submission = tokio::spawn(async move {
            registry
                .submit_load("test".into(), "app".into(), "handler".into(), pinned)
                .await
        });
        tokio::task::yield_now().await;
        tokio::time::advance(PREPARATION_TIMEOUT).await;
        assert!(
            submission
                .await
                .unwrap()
                .unwrap_err()
                .to_string()
                .contains("capacity deadline")
        );
        assert!(
            fixture
                .registry
                .get("test", "app", "handler")
                .unwrap()
                .is_none()
        );
        assert_eq!(fixture.registry.records.available_permits(), 0);
        fixture.assert_preparation_idle();
        assert_eq!(
            fixture.preparation_sample("submissions_total{outcome=\"timeout\"}"),
            1.0
        );
        assert_eq!(
            fixture.preparation_sample("finished_total{outcome=\"timeout\"}"),
            0.0
        );
        drop(held);
        tokio::time::resume();
        fixture
            .registry
            .submit_load(
                "test".into(),
                "app".into(),
                "handler".into(),
                fixture.pinned.clone(),
            )
            .await
            .unwrap();
        tokio::time::timeout(Duration::from_secs(5), async {
            while fixture
                .registry
                .get("test", "app", "handler")
                .unwrap()
                .is_none()
            {
                tokio::time::sleep(Duration::from_millis(1)).await;
            }
        })
        .await
        .unwrap();
    }

    #[tokio::test]
    async fn timed_out_compilation_retains_permits_and_cannot_publish_late() {
        let mut fixture = Fixture::new(
            CacheResponse::Hit(EMPTY_COMPONENT.to_vec()),
            EMPTY_COMPONENT.to_vec(),
            vec![],
            false,
        )
        .await;
        let (gate, release_tx) = fixture.gate_compilation();
        let registry = fixture.registry.clone();
        let pinned = fixture.pinned.clone();
        let task =
            tokio::spawn(async move { registry.load("test", "app", "handler", &pinned).await });
        tokio::time::timeout(Duration::from_secs(5), gate.entered.notified())
            .await
            .unwrap();
        assert_eq!(fixture.preparation_sample("work{state=\"compiling\"}"), 1.0);
        assert_eq!(
            fixture.preparation_sample("occupancy{kind=\"compiling\"}"),
            1.0
        );
        tokio::time::pause();
        tokio::time::advance(PREPARATION_TIMEOUT).await;
        tokio::time::resume();
        assert!(
            tokio::time::timeout(Duration::from_secs(5), task)
                .await
                .expect("preparation timeout task did not complete")
                .unwrap()
                .unwrap_err()
                .to_string()
                .contains("deadline")
        );
        assert_eq!(fixture.registry.active.available_permits(), 0);
        assert_eq!(
            fixture.preparation_sample("work{state=\"terminating\"}"),
            1.0
        );
        assert_eq!(fixture.preparation_sample("work{state=\"compiling\"}"), 0.0);
        assert_eq!(
            fixture.preparation_sample("occupancy{kind=\"compiling\"}"),
            1.0
        );
        assert_eq!(
            fixture.preparation_sample("occupancy{kind=\"tracked\"}"),
            1.0
        );
        assert_eq!(
            fixture.preparation_sample("finished_total{outcome=\"timeout\"}"),
            0.0
        );
        assert_eq!(
            fixture.preparation_sample("compilation_finished_total{outcome=\"timeout\"}"),
            0.0
        );
        assert_eq!(
            fixture.registry.records.available_permits(),
            MAX_QUEUED_PREPARATIONS
        );
        let queued: Vec<_> = (0..MAX_QUEUED_PREPARATIONS)
            .map(|_| fixture.registry.admit().unwrap())
            .collect();
        tokio::time::pause();
        let overflow = fixture.registry.submit_load(
            "test".into(),
            "app".into(),
            "overflow".into(),
            fixture.pinned.clone(),
        );
        tokio::pin!(overflow);
        assert!(futures_util::poll!(&mut overflow).is_pending());
        tokio::time::advance(PREPARATION_TIMEOUT).await;
        tokio::time::resume();
        assert!(
            tokio::time::timeout(Duration::from_secs(5), overflow)
                .await
                .expect("submission capacity timeout did not complete")
                .is_err()
        );
        assert!(
            fixture
                .registry
                .get("test", "app", "handler")
                .unwrap()
                .is_none()
        );
        release_tx.send(()).unwrap();
        tokio::time::timeout(Duration::from_secs(5), async {
            while fixture.registry.active.available_permits() == 0
                || fixture.registry.records.available_permits() != 1
            {
                tokio::time::sleep(Duration::from_millis(1)).await;
            }
        })
        .await
        .unwrap();
        assert_eq!(fixture.registry.records.available_permits(), 1);
        fixture.assert_preparation_idle();
        assert_eq!(
            fixture.preparation_sample("finished_total{outcome=\"timeout\"}"),
            1.0
        );
        assert_eq!(
            fixture.preparation_sample("compilation_finished_total{outcome=\"timeout\"}"),
            1.0
        );
        assert_eq!(
            fixture.preparation_sample("compilation_finished_total{outcome=\"success\"}"),
            0.0
        );
        assert_eq!(fixture.preparation_sample("compilation_seconds_count"), 1.0);
        assert_eq!(
            fixture.preparation_sample("compilation_overrun_seconds_count"),
            1.0
        );
        assert!(
            fixture
                .registry
                .get("test", "app", "handler")
                .unwrap()
                .is_none()
        );
        drop(queued);
        assert_eq!(
            fixture.registry.records.available_permits(),
            MAX_QUEUED_PREPARATIONS + 1
        );
    }

    #[tokio::test]
    async fn aborted_preparation_reports_compiler_until_actual_exit() {
        let mut fixture = Fixture::new(
            CacheResponse::Hit(EMPTY_COMPONENT.to_vec()),
            EMPTY_COMPONENT.to_vec(),
            vec![],
            false,
        )
        .await;
        let (gate, release_tx) = fixture.gate_compilation();
        let registry = fixture.registry.clone();
        let pinned = fixture.pinned.clone();
        let task =
            tokio::spawn(async move { registry.load("test", "app", "handler", &pinned).await });
        tokio::time::timeout(Duration::from_secs(5), gate.entered.notified())
            .await
            .unwrap();
        task.abort();
        assert!(task.await.unwrap_err().is_cancelled());
        assert_eq!(
            fixture.preparation_sample("work{state=\"terminating\"}"),
            1.0
        );
        assert_eq!(
            fixture.preparation_sample("occupancy{kind=\"compiling\"}"),
            1.0
        );
        assert_eq!(
            fixture.preparation_sample("finished_total{outcome=\"cancelled\"}"),
            0.0
        );
        release_tx.send(()).unwrap();
        tokio::time::timeout(Duration::from_secs(5), async {
            while fixture.registry.active.available_permits() == 0
                || fixture.registry.records.available_permits() != MAX_QUEUED_PREPARATIONS + 1
            {
                tokio::time::sleep(Duration::from_millis(1)).await;
            }
        })
        .await
        .unwrap();
        fixture.assert_preparation_idle();
        assert_eq!(
            fixture.preparation_sample("finished_total{outcome=\"cancelled\"}"),
            1.0
        );
        assert_eq!(
            fixture.preparation_sample("compilation_finished_total{outcome=\"cancelled\"}"),
            1.0
        );
        assert_eq!(
            fixture.preparation_sample("compilation_overrun_seconds_count"),
            1.0
        );
        assert!(
            fixture
                .registry
                .get("test", "app", "handler")
                .unwrap()
                .is_none()
        );
    }
}

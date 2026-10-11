use std::io::{self, Write};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use platform_common::nats_client::{NatsConnectionInfo, run_nats_manager};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tracing_subscriber::prelude::*;

use crate::config::configsync;
use crate::sql_pool::SqlPoolMap;

#[derive(Clone, Default)]
pub(crate) struct LogCapture(Arc<Mutex<Vec<u8>>>);

impl Write for LogCapture {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        self.0.lock().unwrap().extend_from_slice(bytes);
        Ok(bytes.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

impl<'a> tracing_subscriber::fmt::MakeWriter<'a> for LogCapture {
    type Writer = Self;

    fn make_writer(&'a self) -> Self::Writer {
        self.clone()
    }
}

impl LogCapture {
    pub(crate) fn subscribe(&self) -> tracing::subscriber::DefaultGuard {
        self.subscribe_with_filter("trace")
    }

    pub(crate) fn subscribe_with_filter(&self, filter: &str) -> tracing::subscriber::DefaultGuard {
        tracing::subscriber::set_default(
            tracing_subscriber::registry()
                .with(tracing_subscriber::EnvFilter::new(filter))
                .with(
                    tracing_subscriber::fmt::layer()
                        .with_ansi(false)
                        .without_time()
                        .with_writer(self.clone())
                        .with_filter(tracing_subscriber::filter::filter_fn(
                            platform_common::logging::is_platform_log,
                        )),
                ),
        )
    }

    pub(crate) fn text(&self) -> String {
        String::from_utf8(self.0.lock().unwrap().clone()).unwrap()
    }

    pub(crate) fn assert_secrets_absent(&self, secrets: &[&str]) {
        let text = self.text();
        for secret in secrets {
            assert!(
                !text.contains(secret),
                "captured diagnostics contain a test secret"
            );
        }
    }
}

#[test]
fn redis_configuration_logs_success_and_failure_without_credentials() {
    let capture = LogCapture::default();
    let _subscriber = capture.subscribe();
    let password = "redis-password-sentinel";
    let token = "redis-token-sentinel";
    let url = format!("redis://user:{password}@localhost:6379/0");
    assert!(crate::redis_client::configured_client(&url).is_some());
    for malformed in [
        format!("redis://user:{password}@localhost:bad-port/?token={token}"),
        format!("unsupported://user:{password}@localhost/?token={token}"),
        format!("{password}?token={token}"),
    ] {
        assert!(crate::redis_client::configured_client(&malformed).is_none());
    }
    let text = capture.text();
    assert!(text.contains("Redis client configured from configsync"));
    assert!(text.contains("invalid Redis URL from configsync"));
    assert!(text.contains("error_kind="));
    capture.assert_secrets_absent(&[password, token, &url]);
}

#[tokio::test]
async fn sql_pool_logs_configuration_failure_without_connection_url() {
    let capture = LogCapture::default();
    let _subscriber = capture.subscribe();
    let password = "postgres-password-sentinel";
    let token = "postgres-token-sentinel";
    let url = format!("postgres://user:{password}@localhost:bad-port/db?token={token}");
    let pools = SqlPoolMap::new(1);
    tokio::time::timeout(
        Duration::from_secs(3),
        pools.ensure("test".into(), "app".into(), "user".into(), url.clone()),
    )
    .await
    .unwrap();
    assert!(pools.get("test", "app", "user").is_none());
    let text = capture.text();
    assert!(text.contains("failed to create SQL pool"));
    assert!(text.contains("error_kind=\"configuration\""));
    capture.assert_secrets_absent(&[password, token, &url]);
}

#[tokio::test]
async fn sql_pool_does_not_log_credentials_reflected_by_database_errors() {
    let capture = LogCapture::default();
    let _subscriber = capture.subscribe();
    let password = "database-reflected-password-sentinel";
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!(
        "postgres://user:{password}@{}/db?sslmode=disable",
        listener.local_addr().unwrap()
    );
    let server = tokio::spawn(async move {
        let (mut socket, _) = listener.accept().await.unwrap();
        let len = socket.read_u32().await.unwrap();
        let mut startup = vec![0; len as usize - 4];
        socket.read_exact(&mut startup).await.unwrap();
        let fields = format!("SERROR\0C28P01\0Mrejected credential {password}\0\0");
        socket.write_all(b"E").await.unwrap();
        socket.write_u32(fields.len() as u32 + 4).await.unwrap();
        socket.write_all(fields.as_bytes()).await.unwrap();
        socket.shutdown().await.unwrap();
    });
    let pools = SqlPoolMap::new(1);
    tokio::time::timeout(
        Duration::from_secs(3),
        pools.ensure("test".into(), "app".into(), "user".into(), url.clone()),
    )
    .await
    .unwrap();
    tokio::time::timeout(Duration::from_secs(3), server)
        .await
        .unwrap()
        .unwrap();
    assert!(capture.text().contains("error_kind=\"database\""));
    capture.assert_secrets_absent(&[password, &url]);
}

#[tokio::test]
async fn nats_connection_failure_does_not_log_url_or_password() {
    let capture = LogCapture::default();
    let _subscriber = capture.subscribe();
    let password = "nats-password-sentinel";
    let token = "nats-token-sentinel";
    let url = format!("nats://user:{password}@localhost:bad-port/?token={token}");
    let (_conn_tx, conn_rx) = tokio::sync::watch::channel(Some(NatsConnectionInfo {
        url: url.clone(),
        username: "user".into(),
        password: password.into(),
    }));
    let (client_tx, _client_rx) = tokio::sync::watch::channel(None);
    let (ready_tx, _ready_rx) = tokio::sync::watch::channel(false);
    let manager = tokio::spawn(run_nats_manager(conn_rx, client_tx, ready_tx));
    let result = tokio::time::timeout(Duration::from_secs(3), async {
        while !capture.text().contains("failed to connect to NATS") {
            tokio::task::yield_now().await;
        }
    })
    .await;
    manager.abort();
    let _ = manager.await;
    result.unwrap();
    assert!(capture.text().contains("error_kind="));
    capture.assert_secrets_absent(&[password, token, &url]);
}

#[test]
fn nested_configuration_debug_output_excludes_secrets_at_every_log_level() {
    let capture = LogCapture::default();
    let _subscriber = capture.subscribe();
    let password = "nested-password-sentinel";
    let env_secret = "environment-secret-sentinel";
    let module_token = "module-token-sentinel";
    let app = configsync::ApplicationConfig {
        name: "app".into(),
        namespace: "test".into(),
        env: [("API_TOKEN".into(), env_secret.into())].into(),
        functions: vec![configsync::FunctionConfig {
            name: "handler".into(),
            module_ref: format!("invalid://registry/?token={module_token}"),
            ..Default::default()
        }],
        sql_users: vec![configsync::SqlUserConfig {
            username: "user".into(),
            connection_url: format!("postgres://user:{password}@localhost/db"),
        }],
        ..Default::default()
    };
    let full = configsync::FullConfigResponse {
        config: Some(configsync::FullConfig {
            applications: vec![app.clone()],
            redis: Some(configsync::RedisConnectionConfig {
                url: format!("redis://user:{password}@localhost/0"),
            }),
            ..Default::default()
        }),
        ..Default::default()
    };
    let update = configsync::IncrementalUpdateRequest {
        incremental_config: Some(configsync::IncrementalConfig {
            updates: vec![configsync::AppUpdate {
                app_config: Some(app),
                delete: false,
            }],
            redis: full.config.as_ref().unwrap().redis.clone(),
            ..Default::default()
        }),
        ..Default::default()
    };
    let info = NatsConnectionInfo {
        url: format!("nats://user:{password}@localhost/?token={module_token}"),
        username: "nats-user-secret-sentinel".into(),
        password: password.into(),
    };
    tracing::trace!(?full, ?update, ?info, "configuration diagnostics");
    tracing::debug!(?full, ?update, ?info, "configuration diagnostics");
    tracing::info!(?full, ?update, ?info, "configuration diagnostics");
    tracing::warn!(?full, ?update, ?info, "configuration diagnostics");
    tracing::error!(?full, ?update, ?info, "configuration diagnostics");
    let text = capture.text();
    for level in ["TRACE", "DEBUG", "INFO", "WARN", "ERROR"] {
        assert!(text.contains(level));
    }
    assert!(text.contains("[REDACTED]"));
    assert!(text.contains("app"));
    capture.assert_secrets_absent(&[password, env_secret, module_token, &info.username]);
}

#[test]
fn http_payload_debug_output_excludes_authorization_cookies_and_bodies() {
    use platform_common::http_types::{HttpRequestPayload, HttpResponsePayload};
    let capture = LogCapture::default();
    let _subscriber = capture.subscribe();
    let request = HttpRequestPayload {
        method: "POST".into(),
        path: "/reset/path-secret-sentinel".into(),
        query: "token=query-secret-sentinel".into(),
        headers: vec![(
            "authorization".into(),
            "Bearer bearer-secret-sentinel".into(),
        )],
        body: Some(b"request-body-secret-sentinel".to_vec()),
    };
    let response = HttpResponsePayload {
        status: 200,
        headers: vec![("set-cookie".into(), "session=cookie-secret-sentinel".into())],
        body: Some(b"response-body-secret-sentinel".to_vec()),
    };
    tracing::debug!(?request, ?response, "HTTP payload diagnostics");
    assert!(capture.text().contains("HttpRequestPayload"));
    assert!(capture.text().contains("HttpResponsePayload"));
    capture.assert_secrets_absent(&[
        "path-secret-sentinel",
        "query-secret-sentinel",
        "bearer-secret-sentinel",
        "cookie-secret-sentinel",
        "request-body-secret-sentinel",
        "response-body-secret-sentinel",
    ]);
}

use std::sync::{
    Arc,
    atomic::{AtomicBool, AtomicUsize, Ordering},
};
use std::time::Duration;

use axum::{
    Json, Router,
    http::{HeaderMap, StatusCode, header},
    routing::get,
};
use platform_common::nats_client::{NatsConnectionInfo, run_nats_manager};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};

use crate::logging_tests::LogCapture;

const FILTERS: [&str; 2] = [
    "trace",
    "info,async_nats::connection=trace,oci_distribution::client=trace,oci_distribution::token_cache=trace",
];

#[tokio::test]
async fn successful_nats_authentication_does_not_log_dependency_credentials() {
    for filter in FILTERS {
        let capture = LogCapture::default();
        let _subscriber = capture.subscribe_with_filter(filter);
        let password = "successful-nats-password-sentinel";
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let (authenticated_tx, authenticated_rx) = tokio::sync::oneshot::channel();
        let server = tokio::spawn(async move {
            let (mut socket, _) = listener.accept().await.unwrap();
            let info = serde_json::json!({
                "server_id": "log-regression-server", "server_name": "log-regression",
                "version": "2.10.0", "proto": 1, "host": "127.0.0.1", "port": addr.port(),
                "auth_required": true, "tls_required": false, "max_payload": 1048576,
                "client_id": 1, "headers": true,
            });
            socket
                .write_all(format!("INFO {info}\r\n").as_bytes())
                .await
                .unwrap();
            let mut socket = BufReader::new(socket);
            let mut authenticated_tx = Some(authenticated_tx);
            loop {
                let mut line = String::new();
                if socket.read_line(&mut line).await.unwrap() == 0 {
                    break;
                }
                if let Some(connect) = line.strip_prefix("CONNECT ") {
                    let connect: serde_json::Value = serde_json::from_str(connect).unwrap();
                    let authenticated =
                        connect["user"] == "regression-user" && connect["pass"] == password;
                    if let Some(tx) = authenticated_tx.take() {
                        let _ = tx.send(authenticated);
                    }
                } else if line.trim_end() == "PING" {
                    socket.get_mut().write_all(b"PONG\r\n").await.unwrap();
                }
            }
        });

        let (_conn_tx, conn_rx) = tokio::sync::watch::channel(Some(NatsConnectionInfo {
            url: format!("nats://{addr}"),
            username: "regression-user".into(),
            password: password.into(),
        }));
        let (client_tx, client_rx) = tokio::sync::watch::channel(None);
        let (ready_tx, mut ready_rx) = tokio::sync::watch::channel(false);
        let manager = tokio::spawn(run_nats_manager(conn_rx, client_tx, ready_tx));

        let result = tokio::time::timeout(Duration::from_secs(3), async {
            assert!(
                authenticated_rx.await.unwrap(),
                "NATS credentials were not submitted"
            );
            ready_rx.wait_for(|ready| *ready).await.unwrap();
        })
        .await;

        manager.abort();
        tokio::time::timeout(Duration::from_secs(3), manager)
            .await
            .unwrap()
            .unwrap_err();
        drop(client_rx);
        server.abort();
        let _ = tokio::time::timeout(Duration::from_secs(3), server)
            .await
            .unwrap();
        result.expect("NATS authentication exceeded test deadline");

        assert!(capture.text().contains("connected to NATS"));
        capture.assert_secrets_absent(&[password]);
    }
}

#[tokio::test]
async fn successful_oci_token_authentication_does_not_log_dependency_tokens() {
    // The OCI client parses the JWT claims when caching the token. The distant
    // expiry keeps this fixture valid without relying on the test host's clock.
    const TOKEN: &str = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJleHAiOjQxMDI0NDQ4MDB9.cHJpdmF0ZS10b2tlbi1zZW50aW5lbA";
    for filter in FILTERS {
        let capture = LogCapture::default();
        let _subscriber = capture.subscribe_with_filter(filter);
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let auth_requests = Arc::new(AtomicUsize::new(0));
        let auth_requests_for_handler = Arc::clone(&auth_requests);
        let manifest_authorized = Arc::new(AtomicBool::new(false));
        let manifest_authorized_for_handler = Arc::clone(&manifest_authorized);
        let challenge = format!("Bearer realm=\"http://{addr}/token\",service=\"log-regression\"");
        let app = Router::new()
            .route(
                "/v2/",
                get(move || {
                    let challenge = challenge.clone();
                    async move {
                        (
                            StatusCode::UNAUTHORIZED,
                            [(header::WWW_AUTHENTICATE, challenge)],
                        )
                    }
                }),
            )
            .route(
                "/token",
                get(move || {
                    let requests = Arc::clone(&auth_requests_for_handler);
                    async move {
                        requests.fetch_add(1, Ordering::SeqCst);
                        Json(serde_json::json!({"token": TOKEN}))
                    }
                }),
            )
            .route(
                "/v2/test/manifests/latest",
                get(move |headers: HeaderMap| {
                    let authorized = Arc::clone(&manifest_authorized_for_handler);
                    async move {
                        let expected = format!("Bearer {TOKEN}");
                        let token_used = headers
                            .get(header::AUTHORIZATION)
                            .and_then(|value| value.to_str().ok())
                            == Some(expected.as_str());
                        authorized.store(token_used, Ordering::SeqCst);
                        let status = if token_used {
                            StatusCode::OK
                        } else {
                            StatusCode::UNAUTHORIZED
                        };
                        (
                            status,
                            Json(serde_json::json!({
                                "schemaVersion": 2,
                                "mediaType": "application/vnd.oci.image.manifest.v1+json",
                                "config": {
                                    "mediaType": "application/vnd.oci.image.config.v1+json",
                                    "digest": format!("sha256:{}", "0".repeat(64)), "size": 0,
                                },
                                "layers": [],
                            })),
                        )
                    }
                }),
            );
        let server = tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
        let result = tokio::time::timeout(
            Duration::from_secs(3),
            crate::oci::resolve_digest(&format!("oci://{addr}/test:latest")),
        )
        .await;
        server.abort();
        let _ = tokio::time::timeout(Duration::from_secs(3), server)
            .await
            .unwrap();
        let digest = result
            .expect("OCI authentication exceeded test deadline")
            .unwrap();

        assert!(digest.starts_with("sha256:"));
        assert!(
            auth_requests.load(Ordering::SeqCst) > 0,
            "OCI token endpoint was not exercised"
        );
        assert!(
            manifest_authorized.load(Ordering::SeqCst),
            "OCI manifest request did not use the token"
        );
        tracing::info!("authenticated OCI dependency regression completed");
        assert!(
            capture
                .text()
                .contains("authenticated OCI dependency regression completed")
        );
        capture.assert_secrets_absent(&[TOKEN, "cHJpdmF0ZS10b2tlbi1zZW50aW5lbA"]);
    }
}

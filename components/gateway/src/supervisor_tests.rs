use std::{
    io,
    panic::AssertUnwindSafe,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    task::Poll,
    time::Duration,
};

use axum::{
    body::Body,
    extract::Request,
    http::{Method, StatusCode, header::ALLOW},
    serve::Listener,
};
use futures_util::{FutureExt, future::BoxFuture, stream};
use platform_common::http_types::HttpResponsePayload;
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::{TcpListener, TcpStream},
    time::Instant,
};

use crate::{
    config::RequestLimits,
    listener::BoundedListener,
    nats::{RequestBroker, RequestError},
    route_table::{RouteEntry, RouteTable},
    supervisor::RequestSupervisor,
};

#[derive(Clone, Copy)]
enum BrokerAction {
    Reply,
    Fail,
    Wait,
    Panic,
}

struct FakeBroker {
    calls: AtomicUsize,
    action: Mutex<BrokerAction>,
}

impl FakeBroker {
    fn new(action: BrokerAction) -> Self {
        Self {
            calls: AtomicUsize::new(0),
            action: Mutex::new(action),
        }
    }

    fn reply(&self) {
        *self.action.lock().unwrap() = BrokerAction::Reply;
    }

    fn calls(&self) -> usize {
        self.calls.load(Ordering::SeqCst)
    }
}

impl RequestBroker for FakeBroker {
    fn is_available(&self) -> bool {
        true
    }

    fn request(
        &self,
        _subject: String,
        _payload: Vec<u8>,
        _deadline: Instant,
    ) -> BoxFuture<'_, Result<Vec<u8>, RequestError>> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        let action = *self.action.lock().unwrap();
        Box::pin(async move {
            match action {
                BrokerAction::Reply => Ok(serde_json::to_vec(&HttpResponsePayload {
                    status: 200,
                    headers: Vec::new(),
                    body: None,
                })
                .unwrap()),
                BrokerAction::Fail => Err(RequestError::Failed),
                BrokerAction::Wait => std::future::pending().await,
                BrokerAction::Panic => panic!("broker test panic"),
            }
        })
    }
}

fn limits() -> RequestLimits {
    RequestLimits {
        in_flight: 1,
        connections: 1,
        body_bytes: 128,
        payload_bytes: 1024,
        metadata_bytes: 512,
        body_read_timeout: Duration::from_secs(5),
        timeout: Duration::from_secs(5),
    }
}

fn supervisor(broker: Arc<FakeBroker>, limits: RequestLimits) -> RequestSupervisor {
    let table = RouteTable::new();
    table
        .upsert(
            "/bounded".to_string(),
            RouteEntry {
                methods: vec!["POST".to_string()],
                nats_subject: "http.test.bounded".to_string(),
            },
        )
        .unwrap();
    RequestSupervisor::new(table, broker, limits)
}

fn request() -> Request {
    Request::builder()
        .method(Method::POST)
        .uri("/bounded")
        .body(Body::empty())
        .unwrap()
}

#[tokio::test]
async fn serialized_payload_limit_rejects_a_body_within_its_raw_limit() {
    let broker = Arc::new(FakeBroker::new(BrokerAction::Reply));
    let mut limits = limits();
    limits.payload_bytes = 128;
    let supervisor = supervisor(broker.clone(), limits);
    let request = Request::builder()
        .method(Method::POST)
        .uri("/bounded")
        .body(Body::from(vec![255; 64]))
        .unwrap();

    let response = supervisor.handle(request).await;

    assert_eq!(response.status(), StatusCode::PAYLOAD_TOO_LARGE);
    assert_eq!(broker.calls(), 0);
    assert_eq!(supervisor.snapshot().in_flight, 1);
    drop(response);
    assert_eq!(supervisor.snapshot().in_flight, 0);
}

#[tokio::test]
async fn buffered_response_retains_admission_until_body_consumption() {
    let broker = Arc::new(FakeBroker::new(BrokerAction::Reply));
    let supervisor = supervisor(broker.clone(), limits());
    let response = supervisor.handle(request()).await;

    assert_eq!(response.status(), StatusCode::OK);
    assert_eq!(supervisor.snapshot().in_flight, 1);
    assert_eq!(supervisor.snapshot().completed, 0);
    assert_eq!(
        supervisor.handle(request()).await.status(),
        StatusCode::SERVICE_UNAVAILABLE
    );
    assert_eq!(broker.calls(), 1);

    axum::body::to_bytes(response.into_body(), 1024)
        .await
        .unwrap();

    assert_eq!(supervisor.snapshot().in_flight, 0);
    assert_eq!(supervisor.snapshot().completed, 1);
    assert_eq!(supervisor.handle(request()).await.status(), StatusCode::OK);
    assert_eq!(broker.calls(), 2);
}

#[tokio::test]
async fn dropping_an_accepted_request_releases_its_admission() {
    let broker = Arc::new(FakeBroker::new(BrokerAction::Wait));
    let supervisor = supervisor(broker.clone(), limits());
    let mut accepted = Box::pin(supervisor.handle(request()));
    assert!(accepted.as_mut().now_or_never().is_none());
    assert_eq!(broker.calls(), 1);
    assert_eq!(supervisor.snapshot().in_flight, 1);

    drop(accepted);

    assert_eq!(supervisor.snapshot().in_flight, 0);
    assert_eq!(supervisor.snapshot().cancelled, 1);
    broker.reply();
    assert_eq!(supervisor.handle(request()).await.status(), StatusCode::OK);
    assert_eq!(broker.calls(), 2);
    assert_eq!(supervisor.snapshot().in_flight, 0);
}

#[tokio::test]
async fn broker_failure_releases_admission_for_the_next_request() {
    let broker = Arc::new(FakeBroker::new(BrokerAction::Fail));
    let supervisor = supervisor(broker.clone(), limits());

    assert_eq!(
        supervisor.handle(request()).await.status(),
        StatusCode::BAD_GATEWAY
    );
    assert_eq!(supervisor.snapshot().in_flight, 0);
    broker.reply();
    assert_eq!(supervisor.handle(request()).await.status(), StatusCode::OK);
    assert_eq!(broker.calls(), 2);
    assert_eq!(supervisor.snapshot().in_flight, 0);
}

#[tokio::test]
async fn broker_panic_releases_admission_for_the_next_request() {
    let broker = Arc::new(FakeBroker::new(BrokerAction::Panic));
    let supervisor = supervisor(broker.clone(), limits());

    assert!(
        AssertUnwindSafe(supervisor.handle(request()))
            .catch_unwind()
            .await
            .is_err()
    );
    assert_eq!(supervisor.snapshot().in_flight, 0);
    broker.reply();
    assert_eq!(supervisor.handle(request()).await.status(), StatusCode::OK);
    assert_eq!(broker.calls(), 2);
    assert_eq!(supervisor.snapshot().in_flight, 0);
}

#[test]
fn unknown_routes_and_disallowed_methods_do_not_poll_the_body() {
    let broker = Arc::new(FakeBroker::new(BrokerAction::Reply));
    let supervisor = supervisor(broker.clone(), limits());

    for (path, expected) in [
        ("/unknown", StatusCode::NOT_FOUND),
        ("/bounded", StatusCode::METHOD_NOT_ALLOWED),
    ] {
        let polled = Arc::new(AtomicBool::new(false));
        let observed = polled.clone();
        let body = Body::from_stream(stream::poll_fn(move |_| {
            observed.store(true, Ordering::SeqCst);
            Poll::Pending::<Option<Result<Vec<u8>, io::Error>>>
        }));
        let request = Request::builder()
            .method(Method::GET)
            .uri(path)
            .body(body)
            .unwrap();

        let response = supervisor
            .handle(request)
            .now_or_never()
            .expect("route and method rejection must finish without reading the body");

        assert_eq!(response.status(), expected);
        if expected == StatusCode::METHOD_NOT_ALLOWED {
            assert_eq!(response.headers().get(ALLOW).unwrap(), "POST");
        }
        assert!(!polled.load(Ordering::SeqCst));
        assert_eq!(supervisor.snapshot().in_flight, 0);
    }
    assert_eq!(broker.calls(), 0);
}

#[tokio::test]
async fn bounded_listener_rejects_excess_connections_and_releases_closed_slots() {
    tokio::time::timeout(Duration::from_secs(5), async {
        let broker = Arc::new(FakeBroker::new(BrokerAction::Reply));
        let supervisor = supervisor(broker, limits());
        let tcp = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let mut listener = BoundedListener::new(tcp, supervisor.connection_capacity());
        let address = Listener::local_addr(&listener).unwrap();
        let _first_client = TcpStream::connect(address).await.unwrap();
        let (first_connection, _) = Listener::accept(&mut listener).await;
        assert_eq!(supervisor.snapshot().connection_count, 1);

        let mut excess_client = TcpStream::connect(address).await.unwrap();
        let mut byte = [0];
        tokio::select! {
            _ = Listener::accept(&mut listener) => {
                panic!("a connection was admitted while the sole slot was occupied");
            }
            result = excess_client.read(&mut byte) => {
                assert_eq!(result.unwrap(), 0, "excess connection must be closed");
            }
        }
        assert_eq!(supervisor.snapshot().connection_count, 1);

        drop(first_connection);
        assert_eq!(supervisor.snapshot().connection_count, 0);
        let mut next_client = TcpStream::connect(address).await.unwrap();
        let (mut next_connection, _) = Listener::accept(&mut listener).await;
        assert_eq!(supervisor.snapshot().connection_count, 1);

        next_client.write_all(b"a").await.unwrap();
        next_connection.read_exact(&mut byte).await.unwrap();
        assert_eq!(byte, *b"a");
        next_connection.write_all(b"b").await.unwrap();
        next_client.read_exact(&mut byte).await.unwrap();
        assert_eq!(byte, *b"b");

        drop(next_connection);
        assert_eq!(supervisor.snapshot().connection_count, 0);
    })
    .await
    .expect("socket admission regression exceeded its finite test budget");
}

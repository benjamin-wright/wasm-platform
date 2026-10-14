//! Projected workload credentials and the Kubernetes authentication adapter.

use std::{path::PathBuf, sync::Arc, time::Duration};

use serde::{Deserialize, Serialize};
use tokio::io::AsyncReadExt;
use tonic::{Request, transport::Channel};

pub const PLATFORM_AUDIENCE: &str = "wasm-platform";
pub const PLATFORM_TOKEN_PATH: &str = "/var/run/secrets/wasm-platform/token";
pub const RPC_TIMEOUT: Duration = Duration::from_secs(10);
pub const SYNC_SESSION_TIMEOUT: Duration = Duration::from_secs(300);
const API_TOKEN_PATH: &str = "/var/run/secrets/kubernetes.io/serviceaccount/token";
const API_CA_PATH: &str = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt";
const TOKEN_TIMEOUT: Duration = Duration::from_secs(2);
pub const MAX_TOKEN_BYTES: usize = 16 * 1024;
const REVIEW_TIMEOUT: Duration = Duration::from_secs(3);
const MAX_REVIEW_BYTES: usize = 64 * 1024;

#[derive(Debug, thiserror::Error)]
pub enum IdentityError {
    #[error("workload token unavailable")]
    TokenUnavailable,
    #[error("workload token invalid")]
    InvalidToken,
    #[error("workload identity configuration invalid")]
    Configuration,
    #[error("workload authentication unavailable")]
    ReviewUnavailable,
    #[error("workload authentication denied")]
    Unauthenticated,
    #[error("workload authorization denied")]
    Forbidden,
    #[error("operator connection failed")]
    Connection,
}

/// Opens the projected file for each use, including after Kubernetes rotates its symlink.
#[derive(Clone)]
pub struct ProjectedTokenReader {
    path: Arc<PathBuf>,
}

impl ProjectedTokenReader {
    pub fn platform() -> Self {
        Self::new(PLATFORM_TOKEN_PATH)
    }

    pub fn new(path: impl Into<PathBuf>) -> Self {
        Self {
            path: Arc::new(path.into()),
        }
    }

    async fn read(&self) -> Result<String, IdentityError> {
        tokio::time::timeout(TOKEN_TIMEOUT, async {
            let file = tokio::fs::File::open(self.path.as_ref())
                .await
                .map_err(|_| IdentityError::TokenUnavailable)?;
            let mut bytes = Vec::new();
            file.take((MAX_TOKEN_BYTES + 1) as u64)
                .read_to_end(&mut bytes)
                .await
                .map_err(|_| IdentityError::TokenUnavailable)?;
            if bytes.len() > MAX_TOKEN_BYTES {
                return Err(IdentityError::InvalidToken);
            }
            let token = std::str::from_utf8(&bytes)
                .map_err(|_| IdentityError::InvalidToken)?
                .trim();
            validate_token(token)?;
            Ok(token.to_owned())
        })
        .await
        .map_err(|_| IdentityError::TokenUnavailable)?
    }

    /// Fails startup when the mounted credential is missing or malformed.
    pub async fn validate(&self) -> Result<(), IdentityError> {
        self.read().await.map(|_| ())
    }

    pub async fn http_request(
        &self,
        request: reqwest::RequestBuilder,
    ) -> Result<reqwest::RequestBuilder, IdentityError> {
        let mut value =
            reqwest::header::HeaderValue::from_str(&format!("Bearer {}", self.read().await?))
                .map_err(|_| IdentityError::InvalidToken)?;
        value.set_sensitive(true);
        Ok(request.header(reqwest::header::AUTHORIZATION, value))
    }

    pub async fn grpc_request<T>(
        &self,
        message: T,
        timeout: Duration,
    ) -> Result<Request<T>, IdentityError> {
        let mut authorization = format!("Bearer {}", self.read().await?)
            .parse::<tonic::metadata::MetadataValue<_>>()
            .map_err(|_| IdentityError::InvalidToken)?;
        authorization.set_sensitive(true);
        let mut request = Request::new(message);
        request
            .metadata_mut()
            .insert("authorization", authorization);
        request.set_timeout(timeout);
        Ok(request)
    }
}

fn validate_token(token: &str) -> Result<(), IdentityError> {
    if token.is_empty()
        || token.len() > MAX_TOKEN_BYTES
        || !token
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
    {
        return Err(IdentityError::InvalidToken);
    }
    Ok(())
}

/// Uses finite transport deadlines without retaining credentials in the channel.
pub async fn operator_channel(address: &str) -> Result<Channel, IdentityError> {
    let url = reqwest::Url::parse(address).map_err(|_| IdentityError::Connection)?;
    if url.scheme() != "http"
        || url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
    {
        return Err(IdentityError::Connection);
    }
    let endpoint = tonic::transport::Endpoint::from_shared(address.to_owned())
        .map_err(|_| IdentityError::Connection)?
        .connect_timeout(RPC_TIMEOUT)
        .timeout(RPC_TIMEOUT);
    tokio::time::timeout(RPC_TIMEOUT, endpoint.connect())
        .await
        .map_err(|_| IdentityError::Connection)?
        .map_err(|_| IdentityError::Connection)
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct TokenReviewRequest<'a> {
    api_version: &'static str,
    kind: &'static str,
    spec: TokenReviewSpec<'a>,
}

#[derive(Serialize)]
struct TokenReviewSpec<'a> {
    token: &'a str,
    audiences: [&'static str; 1],
}

#[derive(Deserialize)]
struct TokenReviewResponse {
    status: TokenReviewStatus,
}

#[derive(Deserialize)]
struct TokenReviewStatus {
    #[serde(default)]
    authenticated: bool,
    #[serde(default)]
    audiences: Vec<String>,
    user: Option<TokenReviewUser>,
    error: Option<String>,
}

#[derive(Deserialize)]
struct TokenReviewUser {
    #[serde(default)]
    username: String,
}

/// Reviews each request against the API server; denied or unavailable reviews fail closed.
#[derive(Clone)]
pub struct ServiceAccountReviewer {
    client: reqwest::Client,
    endpoint: reqwest::Url,
    api_token: ProjectedTokenReader,
    expected_username: Arc<str>,
}

impl ServiceAccountReviewer {
    pub async fn in_cluster(service_account: &str) -> Result<Self, IdentityError> {
        let namespace = std::env::var("POD_NAMESPACE").map_err(|_| IdentityError::Configuration)?;
        if namespace.is_empty()
            || !namespace
                .bytes()
                .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
        {
            return Err(IdentityError::Configuration);
        }
        let host =
            std::env::var("KUBERNETES_SERVICE_HOST").map_err(|_| IdentityError::Configuration)?;
        let port: u16 = std::env::var("KUBERNETES_SERVICE_PORT_HTTPS")
            .or_else(|_| std::env::var("KUBERNETES_SERVICE_PORT"))
            .map_err(|_| IdentityError::Configuration)?
            .parse()
            .map_err(|_| IdentityError::Configuration)?;
        if port == 0 {
            return Err(IdentityError::Configuration);
        }
        let mut endpoint = reqwest::Url::parse(
            "https://kubernetes.default.svc/apis/authentication.k8s.io/v1/tokenreviews",
        )
        .map_err(|_| IdentityError::Configuration)?;
        endpoint
            .set_host(Some(&host))
            .map_err(|_| IdentityError::Configuration)?;
        endpoint
            .set_port(Some(port))
            .map_err(|_| IdentityError::Configuration)?;
        let pem = tokio::fs::read(API_CA_PATH)
            .await
            .map_err(|_| IdentityError::Configuration)?;
        let ca = reqwest::Certificate::from_pem(&pem).map_err(|_| IdentityError::Configuration)?;
        let client = reqwest::Client::builder()
            .add_root_certificate(ca)
            .timeout(REVIEW_TIMEOUT)
            .connect_timeout(REVIEW_TIMEOUT)
            .redirect(reqwest::redirect::Policy::none())
            .no_proxy()
            .build()
            .map_err(|_| IdentityError::Configuration)?;
        let api_token = ProjectedTokenReader::new(API_TOKEN_PATH);
        api_token.validate().await?;
        Ok(Self {
            client,
            endpoint,
            api_token,
            expected_username: format!("system:serviceaccount:{namespace}:{service_account}")
                .into(),
        })
    }

    pub async fn authorize(&self, token: &str) -> Result<(), IdentityError> {
        validate_token(token).map_err(|_| IdentityError::Unauthenticated)?;
        tokio::time::timeout(REVIEW_TIMEOUT, self.review(token))
            .await
            .map_err(|_| IdentityError::ReviewUnavailable)?
    }

    async fn review(&self, token: &str) -> Result<(), IdentityError> {
        let request = self
            .client
            .post(self.endpoint.clone())
            .json(&TokenReviewRequest {
                api_version: "authentication.k8s.io/v1",
                kind: "TokenReview",
                spec: TokenReviewSpec {
                    token,
                    audiences: [PLATFORM_AUDIENCE],
                },
            });
        let mut response = self
            .api_token
            .http_request(request)
            .await
            .map_err(|_| IdentityError::ReviewUnavailable)?
            .send()
            .await
            .map_err(|_| IdentityError::ReviewUnavailable)?;
        if !response.status().is_success()
            || response
                .content_length()
                .is_some_and(|n| n > MAX_REVIEW_BYTES as u64)
        {
            return Err(IdentityError::ReviewUnavailable);
        }
        let mut bytes = Vec::new();
        while let Some(chunk) = response
            .chunk()
            .await
            .map_err(|_| IdentityError::ReviewUnavailable)?
        {
            if bytes.len() + chunk.len() > MAX_REVIEW_BYTES {
                return Err(IdentityError::ReviewUnavailable);
            }
            bytes.extend_from_slice(&chunk);
        }
        let review: TokenReviewResponse =
            serde_json::from_slice(&bytes).map_err(|_| IdentityError::ReviewUnavailable)?;
        authorize_status(review.status, &self.expected_username)
    }
}

fn authorize_status(
    status: TokenReviewStatus,
    expected_username: &str,
) -> Result<(), IdentityError> {
    if !status.authenticated
        || status.error.is_some_and(|value| !value.is_empty())
        || !status
            .audiences
            .iter()
            .any(|audience| audience == PLATFORM_AUDIENCE)
    {
        return Err(IdentityError::Unauthenticated);
    }
    if status
        .user
        .is_none_or(|user| user.username != expected_username)
    {
        return Err(IdentityError::Forbidden);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::{
        Json, Router,
        extract::State,
        http::{HeaderMap, StatusCode},
        response::IntoResponse,
        routing::post,
    };
    use std::sync::atomic::{AtomicUsize, Ordering};

    struct TokenFile(PathBuf);

    impl TokenFile {
        async fn new() -> Self {
            static NEXT: AtomicUsize = AtomicUsize::new(0);
            let path = std::env::temp_dir().join(format!(
                "platform-identity-{}-{}.token",
                std::process::id(),
                NEXT.fetch_add(1, Ordering::Relaxed)
            ));
            tokio::fs::write(&path, "API_TOKEN").await.unwrap();
            Self(path)
        }
    }

    impl Drop for TokenFile {
        fn drop(&mut self) {
            let _ = std::fs::remove_file(&self.0);
        }
    }

    async fn reviewer(
        router: Router,
    ) -> (
        ServiceAccountReviewer,
        TokenFile,
        tokio::task::JoinHandle<()>,
    ) {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let server = tokio::spawn(async move { axum::serve(listener, router).await.unwrap() });
        let token_file = TokenFile::new().await;
        let reviewer = ServiceAccountReviewer {
            client: reqwest::Client::builder()
                .timeout(REVIEW_TIMEOUT)
                .no_proxy()
                .build()
                .unwrap(),
            endpoint: format!("http://{address}/review").parse().unwrap(),
            api_token: ProjectedTokenReader::new(&token_file.0),
            expected_username: "system:serviceaccount:platform:execution-host".into(),
        };
        (reviewer, token_file, server)
    }

    #[tokio::test]
    async fn reviews_use_requested_audience_and_rotated_api_credentials() {
        let expected_token = Arc::new(tokio::sync::Mutex::new("API_TOKEN"));
        let router = Router::new().route("/review", post(async |State(expected): State<Arc<tokio::sync::Mutex<&'static str>>>, headers: HeaderMap, Json(body): Json<serde_json::Value>| {
            assert_eq!(headers[reqwest::header::AUTHORIZATION], format!("Bearer {}", *expected.lock().await));
            assert_eq!(body["apiVersion"], "authentication.k8s.io/v1");
            assert_eq!(body["kind"], "TokenReview");
            assert_eq!(body["spec"]["audiences"], serde_json::json!([PLATFORM_AUDIENCE]));
            assert_eq!(body["spec"]["token"], "HOST_TOKEN");
            Json(serde_json::json!({"status": {"authenticated":true, "audiences":[PLATFORM_AUDIENCE], "user":{"username":"system:serviceaccount:platform:execution-host"}}}))
        })).with_state(expected_token.clone());
        let (reviewer, token_file, server) = reviewer(router).await;
        reviewer.authorize("HOST_TOKEN").await.unwrap();
        tokio::fs::write(&token_file.0, "ROTATED_API_TOKEN")
            .await
            .unwrap();
        *expected_token.lock().await = "ROTATED_API_TOKEN";
        reviewer.authorize("HOST_TOKEN").await.unwrap();
        server.abort();
        let _ = server.await;
    }

    #[tokio::test]
    async fn failed_malformed_and_oversized_reviews_never_expose_response_credentials() {
        for router in [
            Router::new().route("/review", post(|| async { (StatusCode::FORBIDDEN, "RESPONSE_SECRET API_TOKEN HOST_TOKEN").into_response() })),
            Router::new().route("/review", post(|| async { (StatusCode::OK, "malformed RESPONSE_SECRET API_TOKEN HOST_TOKEN").into_response() })),
            Router::new().route("/review", post(|| async { (StatusCode::OK, "a".repeat(MAX_REVIEW_BYTES + 1)).into_response() })),
            Router::new().route("/review", post(|| async { Json(serde_json::json!({"status":{"authenticated":true,"error":"RESPONSE_SECRET API_TOKEN HOST_TOKEN"}})).into_response() })),
        ] {
            let (reviewer, _token_file, server) = reviewer(router).await;
            let error = reviewer.authorize("HOST_TOKEN").await.unwrap_err();
            let diagnostic = format!("{error}: {error:?}");
            for secret in ["RESPONSE_SECRET", "API_TOKEN", "HOST_TOKEN"] { assert!(!diagnostic.contains(secret)); }
            server.abort();
            let _ = server.await;
        }
    }

    #[tokio::test]
    async fn stalled_token_review_has_a_finite_deadline() {
        let router = Router::new().route(
            "/review",
            post(|| async { std::future::pending::<StatusCode>().await }),
        );
        let (reviewer, _token_file, server) = reviewer(router).await;
        let result = tokio::time::timeout(
            REVIEW_TIMEOUT + Duration::from_secs(1),
            reviewer.authorize("HOST_TOKEN"),
        )
        .await
        .unwrap();
        assert!(matches!(result, Err(IdentityError::ReviewUnavailable)));
        server.abort();
        let _ = server.await;
    }

    #[tokio::test]
    async fn kubernetes_denial_with_empty_user_is_unauthenticated() {
        for status in [
            serde_json::json!({"authenticated": false, "user": {}}),
            serde_json::json!({"user": {}, "error": "invalid bearer token"}),
            serde_json::json!({"authenticated": false, "user": {}, "error": "audiences do not intersect"}),
        ] {
            let response = serde_json::json!({
                "kind": "TokenReview",
                "apiVersion": "authentication.k8s.io/v1",
                "metadata": {"creationTimestamp": null},
                "spec": {"token": "INVALID_HOST_TOKEN", "audiences": [PLATFORM_AUDIENCE]},
                "status": status,
            });
            let router = Router::new().route(
                "/review",
                post(move || {
                    let response = response.clone();
                    async move { Json(response) }
                }),
            );
            let (reviewer, _token_file, server) = reviewer(router).await;
            assert!(matches!(
                reviewer.authorize("INVALID_HOST_TOKEN").await,
                Err(IdentityError::Unauthenticated)
            ));
            server.abort();
            let _ = server.await;
        }
    }

    #[test]
    fn authenticated_identity_and_audience_must_both_match() {
        let expected = "system:serviceaccount:platform:execution-host";
        for (authenticated, audience, username, allowed) in [
            (true, PLATFORM_AUDIENCE, expected, true),
            (false, PLATFORM_AUDIENCE, expected, false),
            (true, "https://kubernetes.default.svc", expected, false),
            (
                true,
                PLATFORM_AUDIENCE,
                "system:serviceaccount:other:execution-host",
                false,
            ),
            (
                true,
                PLATFORM_AUDIENCE,
                "system:serviceaccount:platform:gateway",
                false,
            ),
        ] {
            let status = TokenReviewStatus {
                authenticated,
                audiences: vec![audience.into()],
                user: Some(TokenReviewUser {
                    username: username.into(),
                }),
                error: None,
            };
            assert_eq!(authorize_status(status, expected).is_ok(), allowed);
        }
    }

    #[tokio::test]
    async fn projected_credentials_rotate_and_metadata_is_sensitive() {
        let token_file = TokenFile::new().await;
        tokio::fs::write(&token_file.0, "FIRST_TOKEN")
            .await
            .unwrap();
        let reader = ProjectedTokenReader::new(&token_file.0);
        let request = reader.grpc_request((), RPC_TIMEOUT).await.unwrap();
        assert_eq!(
            request.metadata().get("authorization").unwrap(),
            "Bearer FIRST_TOKEN"
        );
        assert!(
            request
                .metadata()
                .get("authorization")
                .unwrap()
                .is_sensitive()
        );
        assert!(!format!("{request:?}").contains("FIRST_TOKEN"));
        tokio::fs::write(&token_file.0, "ROTATED_TOKEN\n")
            .await
            .unwrap();
        assert_eq!(reader.read().await.unwrap(), "ROTATED_TOKEN");
        tokio::fs::remove_file(&token_file.0).await.unwrap();
        let error = reader.read().await.unwrap_err();
        assert_eq!(error.to_string(), "workload token unavailable");
    }

    #[test]
    fn invalid_token_errors_do_not_include_the_input() {
        for token in [
            "",
            "SECRET\r\nheader:value",
            "Bearer SECRET",
            "secret=invalid",
        ] {
            let error = validate_token(token).unwrap_err();
            assert_eq!(
                format!("{error}: {error:?}"),
                "workload token invalid: InvalidToken"
            );
        }
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn projected_symlink_rotation_opens_the_new_target() {
        let first = TokenFile::new().await;
        let second = TokenFile::new().await;
        tokio::fs::write(&second.0, "ROTATED_TOKEN").await.unwrap();
        let projected = TokenFile(first.0.with_extension("projected"));
        let replacement = TokenFile(first.0.with_extension("replacement"));
        std::os::unix::fs::symlink(&first.0, &projected.0).unwrap();
        let reader = ProjectedTokenReader::new(&projected.0);
        assert_eq!(reader.read().await.unwrap(), "API_TOKEN");
        std::os::unix::fs::symlink(&second.0, &replacement.0).unwrap();
        tokio::fs::rename(&replacement.0, &projected.0)
            .await
            .unwrap();
        assert_eq!(reader.read().await.unwrap(), "ROTATED_TOKEN");
    }

    #[tokio::test]
    async fn oversized_token_files_fail_with_safe_errors() {
        let token_file = TokenFile::new().await;
        tokio::fs::write(&token_file.0, "S".repeat(MAX_TOKEN_BYTES + 1))
            .await
            .unwrap();
        let reader = ProjectedTokenReader::new(&token_file.0);
        assert!(matches!(
            reader.validate().await,
            Err(IdentityError::InvalidToken)
        ));
    }
}

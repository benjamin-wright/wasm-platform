use serde::{Deserialize, Serialize};

#[derive(Serialize, Deserialize)]
pub struct HttpRequestPayload {
    pub method: String,
    pub path: String,
    pub query: String,
    pub headers: Vec<(String, String)>,
    pub body: Option<Vec<u8>>,
}

#[derive(Serialize, Deserialize)]
pub struct HttpResponsePayload {
    pub status: u16,
    pub headers: Vec<(String, String)>,
    pub body: Option<Vec<u8>>,
}

impl std::fmt::Debug for HttpRequestPayload {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("HttpRequestPayload")
            .field("method", &self.method)
            .field("path", &"[REDACTED]")
            .field("query", &"[REDACTED]")
            .field("headers", &"[REDACTED]")
            .field("body", &"[REDACTED]")
            .finish()
    }
}

impl std::fmt::Debug for HttpResponsePayload {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("HttpResponsePayload")
            .field("status", &self.status)
            .field("headers", &"[REDACTED]")
            .field("body", &"[REDACTED]")
            .finish()
    }
}

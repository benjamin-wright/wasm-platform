wit_bindgen::generate!({
    world: "http-application",
    path: "../../../framework/runtime.wit",
});

use framework::runtime::{kv, log, messaging};

struct HttpHandler;

impl Guest for HttpHandler {
    fn on_request(request: HttpRequest) -> Result<HttpResponse, String> {
        log::emit(log::Level::Info, "handling request");

        // Publish an event so the message-handler function can track activity.
        messaging::send("demo-app.events", &b"tick".to_vec())?;

        let requests = kv::incr("counters", "requests")?;
        let messages = kv::get_int("counters", "messages")?.unwrap_or(0);

        let body = format!(
            "hello from wasm: method={} path={} requests={} messages={}",
            request.method, request.path, requests, messages
        );
        Ok(HttpResponse {
            status: 200,
            headers: vec![("content-type".to_string(), "text/plain".to_string())],
            body: Some(body.into_bytes()),
        })
    }
}

export!(HttpHandler);

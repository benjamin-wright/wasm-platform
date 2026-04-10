wit_bindgen::generate!({
    world: "http-application",
    path: "../../framework/runtime.wit",
});

use framework::runtime::{kv, messaging};

struct HelloWorld;

impl Guest for HelloWorld {
    fn on_request(request: HttpRequest) -> Result<HttpResponse, String> {
        // Publish an event so the message-counter module can track activity.
        messaging::send("hello-world.events", &b"tick".to_vec())?;

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

export!(HelloWorld);
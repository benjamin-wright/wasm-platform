wit_bindgen::generate!({
    world: "http-application",
    path: "../../../framework/runtime.wit",
});

use framework::runtime::{kv, log};

struct HttpHandler;

impl Guest for HttpHandler {
    fn on_request(_request: HttpRequest) -> Result<HttpResponse, String> {
        log::emit(log::Level::Info, "handling request");

        let requests = kv::incr("requests")?;

        let body = format!("counter-app: requests={}", requests);
        Ok(HttpResponse {
            status: 200,
            headers: vec![("content-type".to_string(), "text/plain".to_string())],
            body: Some(body.into_bytes()),
        })
    }
}

export!(HttpHandler);

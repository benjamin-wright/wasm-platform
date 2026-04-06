wit_bindgen::generate!({
    world: "http-application",
    path: "../../framework/runtime.wit",
});

struct HelloWorld;

impl Guest for HelloWorld {
    fn on_request(request: HttpRequest) -> Result<HttpResponse, String> {
        let body = format!(
            "hello from wasm 3: method={} path={}",
            request.method, request.path
        );
        Ok(HttpResponse {
            status: 200,
            headers: vec![("content-type".to_string(), "text/plain".to_string())],
            body: Some(body.into_bytes()),
        })
    }
}

export!(HelloWorld);
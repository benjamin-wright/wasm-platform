wit_bindgen::generate!({
    world: "http-application",
    path: "../../framework/runtime.wit",
});

struct HttpHandler;

impl Guest for HttpHandler {
    fn on_request(request: HttpRequest) -> Result<HttpResponse, String> {
        let authorization = request
            .headers
            .iter()
            .find(|(name, _)| name.eq_ignore_ascii_case("authorization"))
            .map(|(_, value)| value.as_str())
            .unwrap_or("");
        let body = String::from_utf8_lossy(request.body.as_deref().unwrap_or_default());
        Err(format!("{authorization} {body}"))
    }
}

export!(HttpHandler);

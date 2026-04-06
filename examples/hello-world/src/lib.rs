wit_bindgen::generate!({
    world: "http-application",
    path: "../../framework/runtime.wit",
});

use framework::runtime::kv;

struct HelloWorld;

impl Guest for HelloWorld {
    fn on_request(request: HttpRequest) -> Result<HttpResponse, String> {
        // Read the current counter, increment it, and write it back.
        let count: u64 = match kv::get("counters", "requests")? {
            Some(bytes) if bytes.len() == 8 => {
                u64::from_be_bytes(bytes.try_into().unwrap())
            }
            _ => 0,
        };
        let next = count + 1;
        kv::set("counters", "requests", &next.to_be_bytes().to_vec())?;

        let body = format!(
            "hello from wasm: method={} path={} requests={}",
            request.method, request.path, next
        );
        Ok(HttpResponse {
            status: 200,
            headers: vec![("content-type".to_string(), "text/plain".to_string())],
            body: Some(body.into_bytes()),
        })
    }
}

export!(HelloWorld);
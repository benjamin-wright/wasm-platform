wit_bindgen::generate!({
    world: "application",
    path: "../../framework/runtime.wit",
});

struct HelloWorld;

impl Guest for HelloWorld {
    fn on_request(
        method: String,
        path: String,
        body: Vec<u8>,
    ) -> Result<Vec<u8>, String> {
        let msg = format!(
            "on-request called: method={} path={} body='{}'",
            method,
            path,
            String::from_utf8_lossy(&body)
        );
        Ok(msg.into_bytes())
    }

    fn on_schedule(name: String) -> Result<(), String> {
        // logs would go here once WASI logging is wired up
        let _ = format!("on-schedule called: name={}", name);
        Ok(())
    }

    fn on_message(queue: String, payload: Vec<u8>) -> Result<(), String> {
        let _ = format!(
            "on-message called: queue={} payload={} bytes",
            queue,
            payload.len()
        );
        Ok(())
    }
}

export!(HelloWorld);
wit_bindgen::generate!({
    world: "message-application",
    path: "../../framework/runtime.wit",
});

struct HelloWorld;

impl Guest for HelloWorld {
    fn on_message(payload: Vec<u8>) -> Result<Option<Vec<u8>>, String> {
        let msg = format!(
            "on-message called: payload='{}'",
            String::from_utf8_lossy(&payload)
        );
        Ok(Some(msg.into_bytes()))
    }
}

export!(HelloWorld);
wit_bindgen::generate!({
    world: "message-application",
    path: "../../../framework/runtime.wit",
});

#[path = "../actions.rs"]
mod actions;

struct CancellationMessages;

impl Guest for CancellationMessages {
    fn on_message(payload: Vec<u8>) -> Result<Option<Vec<u8>>, String> {
        let command = String::from_utf8(payload).map_err(|_| "invalid command")?;
        actions::run(&command, "message")?;
        Ok(None)
    }
}

export!(CancellationMessages);

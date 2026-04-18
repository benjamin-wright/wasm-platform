wit_bindgen::generate!({
    world: "message-application",
    path: "../../../framework/runtime.wit",
});

use framework::runtime::{kv, log};

struct MessageHandler;

impl Guest for MessageHandler {
    fn on_message(_payload: Vec<u8>) -> Result<Option<Vec<u8>>, String> {
        log::emit(log::Level::Info, "handling message");

        kv::incr("messages")?;;
        Ok(None)
    }
}

export!(MessageHandler);

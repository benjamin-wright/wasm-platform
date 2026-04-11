wit_bindgen::generate!({
    world: "message-application",
    path: "../../framework/runtime.wit",
});

use framework::runtime::{kv, log};

struct MessageCounter;

impl Guest for MessageCounter {
    fn on_message(_payload: Vec<u8>) -> Result<Option<Vec<u8>>, String> {
        log::emit(log::Level::Info, "handling message");

        kv::incr("counters", "messages")?;
        Ok(None)
    }
}

export!(MessageCounter);

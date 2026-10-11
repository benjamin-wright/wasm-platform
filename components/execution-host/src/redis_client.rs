/// Prepares the Redis client received through configuration sync without exposing
/// connection credentials in successful or failed configuration diagnostics.
pub(crate) fn configured_client(url: &str) -> Option<redis::Client> {
    match redis::Client::open(url) {
        Ok(client) => {
            tracing::info!("Redis client configured from configsync");
            Some(client)
        }
        Err(error) => {
            tracing::warn!(error_kind = ?error.kind(), "invalid Redis URL from configsync");
            None
        }
    }
}

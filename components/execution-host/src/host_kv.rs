use redis::Commands;

use crate::runtime::{HostState, message_bindings};

impl message_bindings::framework::runtime::kv::Host for HostState {
    fn get(
        &mut self,
        store: String,
        key: String,
    ) -> Result<Option<Vec<u8>>, String> {
        let Some(ref client) = self.redis_client else {
            return Err("KV host function unavailable: REDIS_URL not configured".to_string());
        };
        let full_key = format!("{}/{}/{}", self.kv_prefix, store, key);
        let mut conn = match client.get_connection() {
            Ok(c) => c,
            Err(e) => return Err(format!("Redis connection failed: {e}")),
        };
        let result: redis::RedisResult<Option<Vec<u8>>> = conn.get(&full_key);
        result.map_err(|e| e.to_string())
    }

    fn set(
        &mut self,
        store: String,
        key: String,
        value: Vec<u8>,
    ) -> Result<(), String> {
        let Some(ref client) = self.redis_client else {
            return Err("KV host function unavailable: REDIS_URL not configured".to_string());
        };
        let full_key = format!("{}/{}/{}", self.kv_prefix, store, key);
        let mut conn = match client.get_connection() {
            Ok(c) => c,
            Err(e) => return Err(format!("Redis connection failed: {e}")),
        };
        let result: redis::RedisResult<()> = conn.set(&full_key, value);
        result.map_err(|e| e.to_string())
    }

    fn delete(
        &mut self,
        store: String,
        key: String,
    ) -> Result<bool, String> {
        let Some(ref client) = self.redis_client else {
            return Err("KV host function unavailable: REDIS_URL not configured".to_string());
        };
        let full_key = format!("{}/{}/{}", self.kv_prefix, store, key);
        let mut conn = match client.get_connection() {
            Ok(c) => c,
            Err(e) => return Err(format!("Redis connection failed: {e}")),
        };
        let count: redis::RedisResult<i64> = conn.del(&full_key);
        count.map(|n| n > 0).map_err(|e| e.to_string())
    }
}

use redis::Commands;

use crate::runtime::{HostState, message_bindings};

impl message_bindings::framework::runtime::kv::Host for HostState {
    fn get(
        &mut self,
        key: String,
    ) -> Result<Option<Vec<u8>>, String> {
        let Some(ref client) = self.redis_client else {
            return Err("KV host function unavailable: REDIS_URL not configured".to_string());
        };
        let full_key = format!("{}/{}/{}", self.app_namespace, self.app_name, key);
        let mut conn = match client.get_connection() {
            Ok(c) => c,
            Err(e) => return Err(format!("Redis connection failed: {e}")),
        };
        let result: redis::RedisResult<Option<Vec<u8>>> = conn.get(&full_key);
        self.metrics_registry.record_kv_read(&self.app_name, &self.app_namespace);
        result.map_err(|e| e.to_string())
    }

    fn set(
        &mut self,
        key: String,
        value: Vec<u8>,
    ) -> Result<(), String> {
        let Some(ref client) = self.redis_client else {
            return Err("KV host function unavailable: REDIS_URL not configured".to_string());
        };
        let full_key = format!("{}/{}/{}", self.app_namespace, self.app_name, key);
        let mut conn = match client.get_connection() {
            Ok(c) => c,
            Err(e) => return Err(format!("Redis connection failed: {e}")),
        };
        let result: redis::RedisResult<()> = conn.set(&full_key, value);
        self.metrics_registry.record_kv_write(&self.app_name, &self.app_namespace);
        result.map_err(|e| e.to_string())
    }

    fn delete(
        &mut self,
        key: String,
    ) -> Result<bool, String> {
        let Some(ref client) = self.redis_client else {
            return Err("KV host function unavailable: REDIS_URL not configured".to_string());
        };
        let full_key = format!("{}/{}/{}", self.app_namespace, self.app_name, key);
        let mut conn = match client.get_connection() {
            Ok(c) => c,
            Err(e) => return Err(format!("Redis connection failed: {e}")),
        };
        let count: redis::RedisResult<i64> = conn.del(&full_key);
        self.metrics_registry.record_kv_write(&self.app_name, &self.app_namespace);
        count.map(|n| n > 0).map_err(|e| e.to_string())
    }

    fn get_int(
        &mut self,
        key: String,
    ) -> Result<Option<i64>, String> {
        let Some(ref client) = self.redis_client else {
            return Err("KV host function unavailable: REDIS_URL not configured".to_string());
        };
        let full_key = format!("{}/{}/{}", self.app_namespace, self.app_name, key);
        let mut conn = match client.get_connection() {
            Ok(c) => c,
            Err(e) => return Err(format!("Redis connection failed: {e}")),
        };
        let result: redis::RedisResult<Option<i64>> = conn.get(&full_key);
        self.metrics_registry.record_kv_read(&self.app_name, &self.app_namespace);
        result.map_err(|e| e.to_string())
    }

    fn set_int(
        &mut self,
        key: String,
        value: i64,
    ) -> Result<(), String> {
        let Some(ref client) = self.redis_client else {
            return Err("KV host function unavailable: REDIS_URL not configured".to_string());
        };
        let full_key = format!("{}/{}/{}", self.app_namespace, self.app_name, key);
        let mut conn = match client.get_connection() {
            Ok(c) => c,
            Err(e) => return Err(format!("Redis connection failed: {e}")),
        };
        let result: redis::RedisResult<()> = conn.set(&full_key, value);
        self.metrics_registry.record_kv_write(&self.app_name, &self.app_namespace);
        result.map_err(|e| e.to_string())
    }

    fn incr(
        &mut self,
        key: String,
    ) -> Result<i64, String> {
        let Some(ref client) = self.redis_client else {
            return Err("KV host function unavailable: REDIS_URL not configured".to_string());
        };
        let full_key = format!("{}/{}/{}", self.app_namespace, self.app_name, key);
        let mut conn = match client.get_connection() {
            Ok(c) => c,
            Err(e) => return Err(format!("Redis connection failed: {e}")),
        };
        let result: redis::RedisResult<i64> = conn.incr(&full_key, 1);
        self.metrics_registry.record_kv_read(&self.app_name, &self.app_namespace);
        self.metrics_registry.record_kv_write(&self.app_name, &self.app_namespace);
        result.map_err(|e| e.to_string())
    }

    fn decr(
        &mut self,
        key: String,
    ) -> Result<i64, String> {
        let Some(ref client) = self.redis_client else {
            return Err("KV host function unavailable: REDIS_URL not configured".to_string());
        };
        let full_key = format!("{}/{}/{}", self.app_namespace, self.app_name, key);
        let mut conn = match client.get_connection() {
            Ok(c) => c,
            Err(e) => return Err(format!("Redis connection failed: {e}")),
        };
        let result: redis::RedisResult<i64> = conn.decr(&full_key, 1);
        self.metrics_registry.record_kv_read(&self.app_name, &self.app_namespace);
        self.metrics_registry.record_kv_write(&self.app_name, &self.app_namespace);
        result.map_err(|e| e.to_string())
    }
}


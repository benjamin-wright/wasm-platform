use redis::FromRedisValue;

use crate::redis_client::{self, Mutation};
use crate::runtime::{HostState, message_bindings};

impl HostState {
    async fn read_kv<T: FromRedisValue>(&mut self, key: &str) -> Result<Option<T>, String> {
        self.invocation.check()?;
        let client = self
            .redis_client
            .as_ref()
            .ok_or_else(|| "KV host function unavailable: Redis not configured".to_string())?;
        let full_key = format!("{}/{}/{}", self.app_namespace, self.app_name, key);
        let result = redis_client::read(client, &self.invocation, &full_key).await;
        self.metrics_registry
            .record_kv_read(&self.app_name, &self.app_namespace);
        result
    }

    async fn mutate_kv(&mut self, key: &str, mutation: Mutation) -> Result<i64, String> {
        self.invocation.check()?;
        let client = self
            .redis_client
            .as_ref()
            .ok_or_else(|| "KV host function unavailable: Redis not configured".to_string())?;
        let full_key = format!("{}/{}/{}", self.app_namespace, self.app_name, key);
        let is_increment = matches!(mutation, Mutation::Increment(_));
        let result = redis_client::mutate(client, &self.invocation, &full_key, mutation).await;
        if is_increment {
            self.metrics_registry
                .record_kv_read(&self.app_name, &self.app_namespace);
        }
        self.metrics_registry
            .record_kv_write(&self.app_name, &self.app_namespace);
        result
    }
}

impl message_bindings::framework::runtime::kv::Host for HostState {
    async fn get(&mut self, key: String) -> Result<Option<Vec<u8>>, String> {
        self.read_kv(&key).await
    }

    async fn set(&mut self, key: String, value: Vec<u8>) -> Result<(), String> {
        self.mutate_kv(&key, Mutation::Set(value)).await.map(|_| ())
    }

    async fn delete(&mut self, key: String) -> Result<bool, String> {
        self.mutate_kv(&key, Mutation::Delete)
            .await
            .map(|count| count > 0)
    }

    async fn get_int(&mut self, key: String) -> Result<Option<i64>, String> {
        self.read_kv(&key).await
    }

    async fn set_int(&mut self, key: String, value: i64) -> Result<(), String> {
        self.mutate_kv(&key, Mutation::Set(value.to_string().into_bytes()))
            .await
            .map(|_| ())
    }

    async fn incr(&mut self, key: String) -> Result<i64, String> {
        self.mutate_kv(&key, Mutation::Increment(1)).await
    }

    async fn decr(&mut self, key: String) -> Result<i64, String> {
        self.mutate_kv(&key, Mutation::Increment(-1)).await
    }
}

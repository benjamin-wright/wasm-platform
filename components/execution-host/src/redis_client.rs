use std::time::Duration;

use redis::{FromRedisValue, aio::MultiplexedConnection};
use tokio::time::Instant;

use crate::invocation::Invocation;

const MUTATE_BEFORE_DEADLINE: &str = r#"
local time = redis.call('TIME')
local now = tonumber(time[1]) * 1000000 + tonumber(time[2])
if now >= tonumber(ARGV[1]) then
    return {0, 0}
end
if ARGV[2] == 'set' then
    redis.call('SET', KEYS[1], ARGV[3])
    return {1, 0}
elseif ARGV[2] == 'delete' then
    return {1, redis.call('DEL', KEYS[1])}
else
    redis.call('INCRBY', KEYS[1], ARGV[3])
    return {1, redis.call('GET', KEYS[1])}
end
"#;

pub(crate) enum Mutation {
    Set(Vec<u8>),
    Delete,
    Increment(i64),
}

async fn connection(
    client: &redis::Client,
    invocation: &Invocation,
) -> Result<MultiplexedConnection, String> {
    // Keep this connection private to one host call: dropping its final handle
    // aborts the driver, without disrupting another invocation's Redis commands.
    invocation
        .run(client.get_multiplexed_async_connection())
        .await?
        .map_err(|error| redis_error("connect", error))
}

pub(crate) async fn read<T: FromRedisValue>(
    client: &redis::Client,
    invocation: &Invocation,
    key: &str,
) -> Result<Option<T>, String> {
    let mut connection = connection(client, invocation).await?;
    invocation
        .run(redis::cmd("GET").arg(key).query_async(&mut connection))
        .await?
        .map_err(|error| redis_error("get", error))
}

pub(crate) async fn mutate(
    client: &redis::Client,
    invocation: &Invocation,
    key: &str,
    mutation: Mutation,
) -> Result<i64, String> {
    let mut connection = connection(client, invocation).await?;
    let server_time: (u64, u32) = invocation
        .run(redis::cmd("TIME").query_async(&mut connection))
        .await?
        .map_err(|error| redis_error("time", error))?;
    invocation.check()?;

    // Using only the budget remaining after TIME returns subtracts its entire
    // round trip. With steadily advancing server time, this deadline is early
    // by the reply's transit/scheduling delay; host wall-clock skew is irrelevant.
    let remaining = invocation
        .deadline()
        .saturating_duration_since(Instant::now());
    let deadline = backend_deadline(server_time, remaining)?;
    let (operation, value) = match mutation {
        Mutation::Set(value) => ("set", value),
        Mutation::Delete => ("delete", Vec::new()),
        Mutation::Increment(delta) => ("increment", delta.to_string().into_bytes()),
    };
    let (accepted, result): (bool, i64) = invocation
        .run(
            redis::cmd("EVAL")
                .arg(MUTATE_BEFORE_DEADLINE)
                .arg(1)
                .arg(key)
                .arg(deadline)
                .arg(operation)
                .arg(value)
                .query_async(&mut connection),
        )
        .await?
        .map_err(|error| redis_error("mutate", error))?;
    if !accepted {
        return Err("invocation deadline exceeded".to_string());
    }
    Ok(result)
}

fn backend_deadline(server_time: (u64, u32), remaining: Duration) -> Result<u64, String> {
    const MAX_LUA_INTEGER: u64 = (1 << 53) - 1;
    let (seconds, microseconds) = server_time;
    let deadline = u64::try_from(remaining.as_micros())
        .ok()
        .and_then(|remaining| {
            seconds
                .checked_mul(1_000_000)?
                .checked_add(u64::from(microseconds))?
                .checked_add(remaining)
        });
    match deadline {
        Some(deadline) if microseconds < 1_000_000 && deadline <= MAX_LUA_INTEGER => Ok(deadline),
        _ => Err("Redis server time outside supported range".to_string()),
    }
}

fn redis_error(operation: &str, error: redis::RedisError) -> String {
    format!("Redis {operation} failed ({:?})", error.kind())
}

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

#[cfg(test)]
mod tests {
    use super::*;

    // Keep the relay's server connection alive after the invocation's private
    // connection closes, so success cannot come from discarding an unsent EVAL.
    async fn relay_delayed_mutation(
        client: &redis::Client,
        invocation: &Invocation,
        key: &str,
        mutation: Mutation,
    ) {
        use tokio::io::{AsyncWriteExt, BufReader};

        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let relay_client =
            redis::Client::open(format!("redis://{}", listener.local_addr().unwrap())).unwrap();
        let mut backend = client.get_multiplexed_async_connection().await.unwrap();
        let deliver_at = invocation.deadline() + Duration::from_millis(50);
        let (submitted, submission) = tokio::sync::oneshot::channel();
        let relay = tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            let (reader, mut writer) = stream.into_split();
            let mut reader = BufReader::new(reader);
            let mut decoder = Default::default();
            loop {
                let request = redis::parse_redis_value_async(&mut decoder, &mut reader)
                    .await
                    .unwrap();
                let redis::Value::Array(arguments) = request else {
                    panic!("relay expected a Redis command array");
                };
                let arguments: Vec<Vec<u8>> = arguments
                    .into_iter()
                    .map(|argument| match argument {
                        redis::Value::BulkString(argument) => argument,
                        _ => panic!("relay expected bulk string command arguments"),
                    })
                    .collect();
                let delayed = arguments[0] == b"EVAL";
                let mut command = redis::Cmd::new();
                for argument in arguments {
                    command.arg(argument);
                }
                if delayed {
                    submitted.send(()).unwrap();
                    tokio::time::sleep_until(deliver_at).await;
                    let response: (bool, i64) = command.query_async(&mut backend).await.unwrap();
                    return response;
                }
                let response: redis::Value = command.query_async(&mut backend).await.unwrap();
                writer.write_all(&resp2_response(response)).await.unwrap();
            }
        });
        let result = mutate(&relay_client, invocation, key, mutation).await;
        assert!(result.unwrap_err().starts_with("invocation cancelled"));
        submission
            .await
            .expect("production mutation never submitted EVAL");
        let (accepted, value) = relay.await.unwrap();
        assert!(!accepted, "Redis accepted an expired production script");
        assert_eq!(value, 0);
    }

    fn resp2_response(value: redis::Value) -> Vec<u8> {
        match value {
            redis::Value::Nil => b"$-1\r\n".to_vec(),
            redis::Value::Int(value) => format!(":{value}\r\n").into_bytes(),
            redis::Value::SimpleString(value) => format!("+{value}\r\n").into_bytes(),
            redis::Value::Okay => b"+OK\r\n".to_vec(),
            redis::Value::BulkString(value) => {
                let mut response = format!("${}\r\n", value.len()).into_bytes();
                response.extend(value);
                response.extend(b"\r\n");
                response
            }
            redis::Value::Array(values) => {
                let mut response = format!("*{}\r\n", values.len()).into_bytes();
                for value in values {
                    response.extend(resp2_response(value));
                }
                response
            }
            _ => panic!("unexpected RESP3 response on a RESP2 connection"),
        }
    }

    #[tokio::test]
    #[ignore = "requires the isolated Redis Tilt fixture and REDIS_TEST_URL"]
    async fn queued_mutations_reject_expired_scripts_and_preserve_integer_precision() {
        tokio::time::timeout(Duration::from_secs(15), async {
            let url = std::env::var("REDIS_TEST_URL").expect("REDIS_TEST_URL is required");
            let client = configured_client(&url).expect("valid Redis integration test URL");
            let key = format!("invocation-cancellation-test/{}", std::process::id());
            let budget = || Invocation::new(Duration::from_secs(3));
            mutate(&client, &budget(), &key, Mutation::Delete)
                .await
                .unwrap();

            relay_delayed_mutation(
                &client,
                &Invocation::new(Duration::from_millis(500)),
                &key,
                Mutation::Set(b"expired".to_vec()),
            )
            .await;
            assert_eq!(
                read::<Vec<u8>>(&client, &budget(), &key).await.unwrap(),
                None
            );

            let initial = 9_007_199_254_740_992_i64;
            mutate(
                &client,
                &budget(),
                &key,
                Mutation::Set(initial.to_string().into_bytes()),
            )
            .await
            .unwrap();
            for mutation in [
                Mutation::Delete,
                Mutation::Increment(1),
                Mutation::Increment(-1),
            ] {
                relay_delayed_mutation(
                    &client,
                    &Invocation::new(Duration::from_millis(500)),
                    &key,
                    mutation,
                )
                .await;
                assert_eq!(
                    read::<i64>(&client, &budget(), &key).await.unwrap(),
                    Some(initial)
                );
            }
            assert_eq!(
                mutate(&client, &budget(), &key, Mutation::Increment(1))
                    .await
                    .unwrap(),
                initial + 1
            );
            assert_eq!(
                mutate(&client, &budget(), &key, Mutation::Increment(-1))
                    .await
                    .unwrap(),
                initial
            );

            let committed = budget();
            mutate(&client, &committed, &key, Mutation::Set(vec![0, 255]))
                .await
                .unwrap();
            committed.cancel();
            assert_eq!(
                read::<Vec<u8>>(&client, &budget(), &key).await.unwrap(),
                Some(vec![0, 255])
            );
            assert_eq!(
                mutate(&client, &budget(), &key, Mutation::Delete)
                    .await
                    .unwrap(),
                1
            );
            assert_eq!(
                mutate(&client, &budget(), &key, Mutation::Delete)
                    .await
                    .unwrap(),
                0
            );
        })
        .await
        .expect("Redis integration test exceeded its outer deadline");
    }

    #[tokio::test]
    async fn cancelled_operations_do_not_acquire_a_connection() {
        let client = redis::Client::open("redis://127.0.0.1:1").unwrap();
        let invocation = Invocation::new(Duration::from_secs(1));
        invocation.cancel();
        let result = read::<Vec<u8>>(&client, &invocation, "key").await;
        assert!(result.unwrap_err().starts_with("invocation cancelled"));
        for mutation in [
            Mutation::Set(vec![1]),
            Mutation::Delete,
            Mutation::Increment(1),
        ] {
            let result = mutate(&client, &invocation, "key", mutation).await;
            assert!(result.unwrap_err().starts_with("invocation cancelled"));
        }
    }

    #[tokio::test]
    async fn stalled_redis_handshake_uses_the_invocation_deadline() {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let client =
            redis::Client::open(format!("redis://{}", listener.local_addr().unwrap())).unwrap();
        let (release, released) = tokio::sync::oneshot::channel();
        let server = tokio::spawn(async move {
            let (_socket, _) = listener.accept().await.unwrap();
            released.await.unwrap();
        });
        let invocation = Invocation::new(Duration::from_millis(100));
        let result = tokio::time::timeout(
            Duration::from_secs(1),
            read::<Vec<u8>>(&client, &invocation, "key"),
        )
        .await
        .expect("Redis acquisition exceeded the outer test bound");
        assert!(result.unwrap_err().starts_with("invocation cancelled"));
        assert!(Instant::now() >= invocation.deadline());
        release.send(()).unwrap();
        tokio::time::timeout(Duration::from_secs(1), server)
            .await
            .unwrap()
            .unwrap();
    }

    #[test]
    fn backend_deadline_uses_server_time_and_rounds_remaining_budget_down() {
        assert_eq!(
            backend_deadline((1_700_000_000, 999_998), Duration::from_nanos(2_999)).unwrap(),
            1_700_000_001_000_000,
        );
        assert_eq!(
            backend_deadline((1_700_000_000, 500_000), Duration::ZERO).unwrap(),
            1_700_000_000_500_000,
        );
    }

    #[test]
    fn backend_deadline_rejects_overflow_and_lua_precision_loss() {
        assert!(backend_deadline((u64::MAX, 0), Duration::ZERO).is_err());
        assert!(backend_deadline((9_007_199_254, 740_992), Duration::ZERO).is_err());
        assert!(backend_deadline((1, 1_000_000), Duration::ZERO).is_err());
        assert!(backend_deadline((1, 0), Duration::MAX).is_err());
    }
}

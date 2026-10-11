use crate::framework::runtime::{kv, metrics, sql};
use std::time::Duration;

pub fn run(command: &str, world: &str) -> Result<(), String> {
    let mut fields = command.split_whitespace();
    let mode = fields.next().ok_or("missing mode")?;
    let token = fields.next().ok_or("missing token")?;
    let delay: f64 = fields
        .next()
        .ok_or("missing delay")?
        .parse()
        .map_err(|_| "invalid delay")?;
    metrics::counter_increment(
        "cancellation_started_total",
        &[
            ("world".to_string(), world.to_string()),
            ("mode".to_string(), mode.to_string()),
        ],
    )?;
    match mode {
        "cpu" => spin(),
        "sleep" => {
            let delay = Duration::try_from_secs_f64(delay).map_err(|_| "invalid sleep delay")?;
            std::thread::sleep(delay);
            kv::set(token, b"after-sleep")
        }
        "sql" => {
            let _ = sql::execute(
                "INSERT INTO cancellation_effects (token) SELECT $1 FROM pg_sleep($2)",
                &[sql::Param::Text(token.to_string()), sql::Param::Real(delay)],
            );
            // Attempt a new effect even when the expired SQL call returns an error.
            kv::set(token, b"after-sql")
        }
        "write" => commit(token),
        "committed" => {
            commit(token)?;
            spin()
        }
        "transaction" => {
            for statement in ["BEGIN", "COMMIT", "ROLLBACK", "SET statement_timeout = 0"] {
                if sql::execute(statement, &[]).is_ok() {
                    return Err("guest transaction control was accepted".to_string());
                }
            }
            Ok(())
        }
        _ => Err("unknown mode".to_string()),
    }
}

fn commit(token: &str) -> Result<(), String> {
    sql::execute(
        "INSERT INTO cancellation_effects (token) VALUES ($1) ON CONFLICT (token) DO NOTHING",
        &[sql::Param::Text(token.to_string())],
    )?;
    kv::set(token, b"committed")
}

fn spin() -> ! {
    let mut value = 0_u64;
    loop {
        value = std::hint::black_box(value.wrapping_add(1));
    }
}

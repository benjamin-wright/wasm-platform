use anyhow::Context;
use std::{str::FromStr, time::Duration};

#[derive(Clone)]
pub struct RequestLimits {
    pub in_flight: usize,
    pub connections: usize,
    pub body_bytes: usize,
    pub payload_bytes: usize,
    pub metadata_bytes: usize,
    pub body_read_timeout: Duration,
    pub timeout: Duration,
}

pub struct Config {
    pub operator_addr: String,
    pub gateway_id: String,
    pub requests: RequestLimits,
    pub http_port: u16,
}

impl Config {
    pub fn from_env() -> anyhow::Result<Self> {
        let operator_addr = std::env::var("OPERATOR_ADDR")
            .context("OPERATOR_ADDR environment variable is required")?;
        let gateway_id = std::env::var("HOSTNAME").unwrap_or_else(|_| "unknown".to_string());
        let requests = RequestLimits {
            in_flight: read_limit("GATEWAY_MAX_IN_FLIGHT_REQUESTS", 32, 256)?,
            connections: read_limit("GATEWAY_MAX_CONNECTIONS", 128, 4096)?,
            body_bytes: read_limit("GATEWAY_MAX_BODY_BYTES", 128 * 1024, 1024 * 1024)?,
            payload_bytes: read_limit("GATEWAY_MAX_PAYLOAD_BYTES", 768 * 1024, 1024 * 1024)?,
            metadata_bytes: read_limit("GATEWAY_MAX_METADATA_BYTES", 16 * 1024, 64 * 1024)?,
            body_read_timeout: Duration::from_secs(read_limit(
                "GATEWAY_BODY_READ_TIMEOUT_SECS",
                5,
                300,
            )?),
            timeout: Duration::from_secs(read_limit("GATEWAY_TIMEOUT_SECS", 30, 300)?),
        };
        let http_port = read_limit("HTTP_PORT", 3000, u16::MAX)?;
        Ok(Config {
            operator_addr,
            gateway_id,
            requests,
            http_port,
        })
    }
}

fn read_limit<T>(name: &str, default: T, maximum: T) -> anyhow::Result<T>
where
    T: FromStr + PartialOrd + Default + std::fmt::Display,
{
    let value = match std::env::var(name) {
        Ok(value) => Some(value),
        Err(std::env::VarError::NotPresent) => None,
        Err(_) => anyhow::bail!("{name} must be a positive integer"),
    };
    parse_limit(name, value.as_deref(), default, maximum)
}

fn parse_limit<T>(name: &str, value: Option<&str>, default: T, maximum: T) -> anyhow::Result<T>
where
    T: FromStr + PartialOrd + Default + std::fmt::Display,
{
    let value = match value {
        Some(value) => value
            .parse::<T>()
            .map_err(|_| anyhow::anyhow!("{name} must be a positive integer"))?,
        None => default,
    };
    anyhow::ensure!(
        value > T::default() && value <= maximum,
        "{name} must be between 1 and {maximum}"
    );
    Ok(value)
}

#[cfg(test)]
mod tests {
    use super::parse_limit;

    #[test]
    fn numeric_configuration_accepts_defaults_and_rejects_invalid_limits() {
        assert_eq!(parse_limit("LIMIT", None, 32usize, 256).unwrap(), 32);
        assert_eq!(
            parse_limit("LIMIT", Some("256"), 32usize, 256).unwrap(),
            256
        );
        for value in ["", "garbage", "0", "-1", "257", "18446744073709551616"] {
            assert!(parse_limit("LIMIT", Some(value), 32usize, 256).is_err());
        }
    }
}

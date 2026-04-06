use anyhow::Context;

pub struct Config {
    pub operator_addr: String,
    pub gateway_id: String,
    pub timeout_secs: u64,
    pub http_port: u16,
}

impl Config {
    pub fn from_env() -> anyhow::Result<Self> {
        let operator_addr = std::env::var("OPERATOR_ADDR")
            .context("OPERATOR_ADDR environment variable is required")?;
        let gateway_id = std::env::var("HOSTNAME").unwrap_or_else(|_| "unknown".to_string());
        let timeout_secs = std::env::var("GATEWAY_TIMEOUT_SECS")
            .ok()
            .and_then(|v| v.parse().ok())
            .unwrap_or(30);
        let http_port = std::env::var("HTTP_PORT")
            .ok()
            .and_then(|v| v.parse().ok())
            .unwrap_or(3000);
        Ok(Config {
            operator_addr,
            gateway_id,
            timeout_secs,
            http_port,
        })
    }
}

use anyhow::{Result, bail};
use sha2::{Digest, Sha256};
use std::{fmt, str::FromStr};

pub const MAX_MANIFEST_BYTES: usize = 1024 * 1024;
pub const MAX_WASM_BYTES: usize = 16 * 1024 * 1024;
pub const CACHE_MAX_BYTES: usize = 64 * 1024 * 1024;
pub const CACHE_MAX_ENTRIES: usize = 256;

/// Canonical SHA-256 identity for immutable artifact bytes.
#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub struct Sha256Digest(String);

impl Sha256Digest {
    pub fn parse(value: &str) -> Result<Self> {
        let Some(hex) = value.strip_prefix("sha256:") else {
            bail!("artifact digest must use sha256");
        };
        if hex.len() != 64
            || !hex
                .bytes()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
        {
            bail!("artifact digest must contain 64 lowercase hexadecimal characters");
        }
        Ok(Self(value.to_owned()))
    }

    pub fn from_bytes(bytes: &[u8]) -> Self {
        Self(format!("sha256:{:x}", Sha256::digest(bytes)))
    }

    pub fn verify(&self, bytes: &[u8]) -> Result<()> {
        if *self != Self::from_bytes(bytes) {
            bail!("artifact digest mismatch");
        }
        Ok(())
    }

    pub fn as_str(&self) -> &str {
        &self.0
    }
}

impl FromStr for Sha256Digest {
    type Err = anyhow::Error;

    fn from_str(value: &str) -> Result<Self> {
        Self::parse(value)
    }
}

impl fmt::Display for Sha256Digest {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
}

/// Verifies the bounded binary Wasm envelope and its immutable byte identity.
/// Component validation remains the local compiler's responsibility.
pub fn verify_wasm(digest: &Sha256Digest, bytes: &[u8]) -> Result<()> {
    if bytes.len() > MAX_WASM_BYTES {
        bail!("Wasm artifact exceeds byte limit");
    }
    if bytes.len() < 8 || !bytes.starts_with(b"\0asm") {
        bail!("artifact is not binary Wasm");
    }
    digest.verify(bytes)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn canonical_digest_round_trips_and_rejects_aliases() {
        let digest = Sha256Digest::from_bytes(b"abc");
        assert_eq!(
            digest.as_str(),
            "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
        );
        assert_eq!(Sha256Digest::parse(digest.as_str()).unwrap(), digest);
        for invalid in [
            digest.as_str().to_uppercase(),
            digest.as_str().trim_start_matches("sha256:").to_string(),
            format!("{} ", digest),
            format!("sha256:{}", "g".repeat(64)),
            format!("sha256:{}", "0".repeat(63)),
            format!("sha512:{}", "0".repeat(64)),
        ] {
            assert!(Sha256Digest::parse(&invalid).is_err());
        }
        assert!(digest.verify(b"abd").is_err());
    }

    #[test]
    fn wasm_verification_checks_format_size_and_integrity() {
        let wasm = b"\0asm\x0d\0\x01\0";
        assert!(verify_wasm(&Sha256Digest::from_bytes(wasm), wasm).is_ok());
        let native = b"\x7fELF native artifact";
        assert!(verify_wasm(&Sha256Digest::from_bytes(native), native).is_err());
        assert!(verify_wasm(&Sha256Digest::from_bytes(b"\0asm"), b"\0asm").is_err());
        assert!(verify_wasm(&Sha256Digest::from_bytes(b"different"), wasm).is_err());
        let mut oversized = vec![0; MAX_WASM_BYTES + 1];
        oversized[..wasm.len()].copy_from_slice(wasm);
        assert!(verify_wasm(&Sha256Digest::from_bytes(&oversized), &oversized).is_err());
    }
}

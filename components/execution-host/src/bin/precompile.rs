use anyhow::{Context, Result};
use std::{env, fs};
use wasmtime::{Config, Engine};

/// AOT-compiles a WASM component to a native .cwasm artifact.
///
/// The Engine is configured identically to the runtime so that the serialised
/// artifact loads without recompilation at process startup.
fn main() -> Result<()> {
    let args: Vec<String> = env::args().collect();
    anyhow::ensure!(
        args.len() == 3,
        "usage: precompile <input.wasm> <output.cwasm>"
    );

    let input = &args[1];
    let output = &args[2];

    let mut config = Config::new();
    config.wasm_component_model(true);
    let engine = Engine::new(&config)?;

    let wasm = fs::read(input).with_context(|| format!("failed to read {input}"))?;
    let compiled = engine
        .precompile_component(&wasm)
        .map_err(|e| anyhow::anyhow!("failed to AOT-compile {input}: {e:#}"))?;
    fs::write(output, &compiled).with_context(|| format!("failed to write {output}"))?;

    Ok(())
}

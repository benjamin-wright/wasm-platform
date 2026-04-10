fn main() -> Result<(), Box<dyn std::error::Error>> {
    // Emit the resolved wasmtime version from Cargo.lock so src/modules.rs can
    // use it as the AOT compatibility key in module-cache lookups.  Reading the
    // lockfile instead of hardcoding means the value stays correct after any
    // `cargo update` without touching this file.
    let lock = std::fs::read_to_string("../../Cargo.lock")?;
    let wasmtime_version = lock
        .split("[[package]]")
        .skip(1)
        .find(|block| {
            block
                .lines()
                .any(|l| l.trim() == "name = \"wasmtime\"")
        })
        .and_then(|block| {
            block
                .lines()
                .find(|l| l.trim().starts_with("version ="))
                .and_then(|l| l.split_once('"').map(|x| x.1))
                .and_then(|s| s.strip_suffix('"'))
        })
        .ok_or("wasmtime not found in Cargo.lock")?;
    println!("cargo:rustc-env=WASMTIME_VERSION={wasmtime_version}");
    println!("cargo:rerun-if-changed=../../Cargo.lock");
    println!("cargo:rerun-if-changed=../../framework/runtime.wit");

    tonic_build::compile_protos("../../proto/configsync/v1/configsync.proto")?;
    Ok(())
}

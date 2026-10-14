fn main() -> Result<(), Box<dyn std::error::Error>> {
    println!("cargo:rerun-if-changed=../../framework/runtime.wit");

    tonic_build::configure()
        .skip_debug("configsync.v1.SqlUserConfig")
        .skip_debug("configsync.v1.RedisConnectionConfig")
        .skip_debug("configsync.v1.ApplicationConfig")
        .skip_debug("configsync.v1.FunctionConfig")
        .compile_protos(
            &["../../proto/configsync/v1/configsync.proto"],
            &["../../proto"],
        )?;
    Ok(())
}

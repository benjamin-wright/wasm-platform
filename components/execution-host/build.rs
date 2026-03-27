fn main() -> Result<(), Box<dyn std::error::Error>> {
    tonic_build::compile_protos("../../proto/configsync/v1/configsync.proto")?;
    Ok(())
}

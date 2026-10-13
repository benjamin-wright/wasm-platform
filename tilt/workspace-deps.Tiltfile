WORKSPACE_DEPS_IMAGE = 'workspace-deps'
RUST_WORKSPACE_INPUTS = [
    'Cargo.toml',
    'Cargo.lock',
    'lib',
    'components/execution-host/Cargo.toml',
    'components/gateway/Cargo.toml',
    'components/module-cache/Cargo.toml',
]

def workspace_deps():
    custom_build(
        WORKSPACE_DEPS_IMAGE,
        command = (
            'docker build' +
            ' --target workspace-deps' +
            ' -f Dockerfile.deps' +
            ' -t $EXPECTED_REF' +
            ' .'
        ),
        deps = [
            'Dockerfile.deps',
            '.dockerignore',
            'Cargo.toml',
            'Cargo.lock',
            'components/execution-host/Cargo.toml',
            'components/execution-host/build.rs',
            'components/gateway/Cargo.toml',
            'components/gateway/build.rs',
            'components/module-cache/Cargo.toml',
            'lib/platform-common/Cargo.toml',
            'proto/configsync/v1/configsync.proto',
            'proto/gateway/v1/gateway.proto',
            'framework/runtime.wit',
        ],
    )

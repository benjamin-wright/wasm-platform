WORKSPACE_DEPS_IMAGE = 'workspace-deps'

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
            'Cargo.toml',
            'Cargo.lock',
            'components/execution-host/Cargo.toml',
            'components/execution-host/build.rs',
            'components/gateway/Cargo.toml',
            'components/gateway/build.rs',
            'components/module-cache/Cargo.toml',
            'examples/demo-app/http-handler/Cargo.toml',
            'examples/demo-app/message-handler/Cargo.toml',
            'lib/platform-common/Cargo.toml',
            'proto/configsync/v1/configsync.proto',
            'proto/gateway/v1/gateway.proto',
            'framework/runtime.wit',
        ],
    )

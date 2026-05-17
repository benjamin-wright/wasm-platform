load('./tilt/namespace.Tiltfile', 'k8s_namespace')

allow_k8s_contexts('k3d-wasm-platform')

## Install platform components ##

namespace = 'wasm-platform'

load('./components/wp-databases/Tiltfile', 'db_operator')
load('./components/execution-host/Tiltfile', 'execution_host')
load('./components/gateway/Tiltfile', 'gateway')
load('./components/module-cache/Tiltfile', 'module_cache')
load('./components/wp-operator/Tiltfile', 'wp_operator')
load('./examples/demo-app/Tiltfile', 'demo_app')
load('./examples/counter-app/Tiltfile', 'counter_app')
load('./examples/sql-hello/Tiltfile', 'sql_hello')
load('./examples/sql-broken-migrations/Tiltfile', 'sql_broken_migrations')
load('./tests/e2e/Tiltfile', 'e2e_tests')
load('./tilt/workspace-deps.Tiltfile', 'workspace_deps')
load('./tilt/cert-manager.Tiltfile', 'cert_manager')

k8s_namespace(namespace)
db_operator(namespace = 'db-operator')
cert_manager()

workspace_deps()

# Render the unified platform chart. Each Deployment becomes its own Tilt resource;
# component Tiltfiles then attach port-forwards, labels, and deps via k8s_resource().
k8s_yaml(helm(
    'helm/wasm-platform',
    name = 'wasm-platform',
    namespace = namespace,
))

wp_operator()
execution_host(resource_deps=['wp-operator'])
gateway(resource_deps=['wp-operator'])
module_cache()


## Example applications ##

k8s_namespace('examples')
demo_app('examples', resource_deps=['wp-operator', 'wp-webhook', 'execution-host', 'gateway'])
counter_app('examples', resource_deps=['wp-operator', 'wp-webhook', 'execution-host', 'gateway'])
sql_hello('default', resource_deps=['wp-operator', 'wp-webhook', 'execution-host', 'gateway'])
sql_broken_migrations('default', resource_deps=['wp-operator', 'wp-webhook', 'execution-host', 'gateway'])

## Tests ##

e2e_tests()
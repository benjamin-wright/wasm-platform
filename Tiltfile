load('ext://helm_resource', 'helm_resource')
load('./tilt/namespace.Tiltfile', 'k8s_namespace')

allow_k8s_contexts('k3d-wasm-platform')

## Install platform components ##

namespace = 'wasm-platform'

load('./components/wp-databases/Tiltfile', 'db_operator', 'wp_databases')
load('./components/execution-host/Tiltfile', 'execution_host')
load('./components/gateway/Tiltfile', 'gateway')
load('./components/module-cache/Tiltfile', 'module_cache')
load('./components/wp-operator/Tiltfile', 'wp_operator')
load('./examples/demo-app/Tiltfile', 'demo_app')
load('./tests/e2e/Tiltfile', 'e2e_tests')
load('./tilt/workspace-deps.Tiltfile', 'workspace_deps')

k8s_namespace(namespace)
db_operator(namespace = 'db-operator')

workspace_deps()
wp_databases(namespace)
wp_operator(namespace)
execution_host(namespace, resource_deps=['wp-operator'])
gateway(namespace, resource_deps=['wp-operator'])
module_cache(namespace)


## Example applications ##

k8s_namespace('examples')
demo_app('examples', resource_deps=['wp-operator', 'execution-host', 'gateway'])

## Tests ##

e2e_tests()
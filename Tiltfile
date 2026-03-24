allow_k8s_contexts('k3d-wasm-platform')

load('./components/execution-host/Tiltfile', 'execution_host')
load('./components/module-cache/Tiltfile', 'module_cache')
load('./components/wp-operator/Tiltfile', 'wp_operator')

execution_host()
module_cache()
wp_operator()

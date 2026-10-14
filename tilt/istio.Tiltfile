load('ext://helm_resource', 'helm_resource')
load('./namespace.Tiltfile', 'k8s_namespace')

ISTIO_VERSION = '1.31.1'
ISTIO_REPOSITORY = 'https://blob.istio.io/istio-release/charts'

def istio():
    k8s_namespace('istio-system')
    flags = [
        '--create-namespace',
        '--repo=' + ISTIO_REPOSITORY,
        '--version=' + ISTIO_VERSION,
        '--wait',
        '--timeout=5m',
    ]
    local_resource(
        'istio-base',
        cmd=['helm', 'upgrade', '--install', 'istio-base', 'base', '--namespace=istio-system'] + flags,
        deps=['tilt/istio.Tiltfile'],
        labels=['istio'],
    )
    helm_resource(
        'istiod',
        chart='istiod',
        namespace='istio-system',
        flags=flags + ['--set=profile=ambient'],
        resource_deps=['istio-base'],
        labels=['istio'],
    )
    helm_resource(
        'istio-cni',
        chart='cni',
        namespace='istio-system',
        flags=flags + ['--set=profile=ambient', '--set=global.platform=k3d'],
        resource_deps=['istiod'],
        labels=['istio'],
    )
    helm_resource(
        'ztunnel',
        chart='ztunnel',
        namespace='istio-system',
        flags=flags,
        resource_deps=['istio-cni'],
        labels=['istio'],
    )
    local_resource(
        'istio-ready',
        cmd='kubectl rollout status deployment/istiod -n istio-system --timeout=120s && kubectl rollout status daemonset/istio-cni-node -n istio-system --timeout=120s && kubectl rollout status daemonset/ztunnel -n istio-system --timeout=120s',
        resource_deps=['istiod', 'istio-cni', 'ztunnel'],
        labels=['internal'],
    )

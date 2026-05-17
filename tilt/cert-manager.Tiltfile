load('ext://helm_resource', 'helm_resource')
load('./namespace.Tiltfile', 'k8s_namespace')

update_settings(
    k8s_upsert_timeout_secs=300,
)

def cert_manager():
    k8s_namespace('cert-manager')
    helm_resource(
        'cert-manager',
        namespace='cert-manager',
        chart='oci://quay.io/jetstack/charts/cert-manager:v1.17.2',
        flags=['--set=crds.enabled=true'],
        labels=['platform'],
    )
    local_resource(
        'cert-manager-ready',
        cmd='kubectl rollout status deployment/cert-manager -n cert-manager --timeout=120s',
        resource_deps=['cert-manager'],
        labels=['internal'],
    )

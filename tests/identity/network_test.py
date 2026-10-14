"""Verify deployed ambient transport policy through disposable workload pods."""

import json
import subprocess
import time
import uuid


def kubectl(*args, manifest=None):
    result = subprocess.run(
        ['kubectl', '--request-timeout=15s', *args],
        input=json.dumps(manifest) if manifest else None,
        text=True, capture_output=True, timeout=30, check=True,
    )
    return result.stdout


def check(namespace, account, endpoint, allowed, token=False, expected=None, registry=False):
    name = 'identity-' + uuid.uuid4().hex[:10]
    # Tokens stay inside the pod and never appear in kubectl arguments or logs.
    header = '-H "Authorization: Bearer $(cat /identity/token)"' if token else ''
    protocol = '--http2-prior-knowledge' if ':50051/' in endpoint else ''
    retries = '--retry 4 --retry-delay 1 --retry-all-errors --retry-max-time 20' if allowed else ''
    expected = expected or ('404' if token else ('415' if allowed else '000'))
    resolve = ''
    lookup = ''
    if registry:
        host = 'wasm-platform-registry.localhost'
        lookup = (
            f'registry_ip=$(nslookup {host} | awk \'/^Address: [0-9]+[.][0-9]+[.][0-9]+[.][0-9]+$/ {{print $2}}\' | tail -n 1); '
            'case "$registry_ip" in ""|127.*|0.0.0.0) echo "registry DNS resolved to an invalid address"; exit 1;; esac; '
        )
        resolve = f'--resolve {host}:5000:$registry_ip'
    script = (
        lookup +
        f'code=$(curl --silent --show-error --output /dev/null --write-out "%{{http_code}}" '
        f'--connect-timeout 3 --max-time 5 {retries} {protocol} {resolve} {header} {endpoint}) || true; '
        f'echo "HTTP $code (expected {expected})"; test "$code" = "{expected}"'
    )
    manifest = {
        'apiVersion': 'v1', 'kind': 'Pod',
        'metadata': {'name': name, 'namespace': namespace},
        'spec': {
            'serviceAccountName': account, 'automountServiceAccountToken': False,
            'restartPolicy': 'Never', 'activeDeadlineSeconds': 90,
            'securityContext': {'runAsNonRoot': True, 'runAsUser': 100,
                                'seccompProfile': {'type': 'RuntimeDefault'}},
            'containers': [{
                'name': 'probe', 'image': 'curlimages/curl:8.12.1',
                'command': ['sh', '-ec', script],
                'securityContext': {'allowPrivilegeEscalation': False,
                                    'readOnlyRootFilesystem': True,
                                    'capabilities': {'drop': ['ALL']}},
                'resources': {'requests': {'cpu': '10m', 'memory': '16Mi'},
                              'limits': {'cpu': '100m', 'memory': '32Mi'}},
            }],
        },
    }
    if namespace == 'wasm-platform':
        manifest['metadata']['labels'] = {'istio.io/dataplane-mode': 'ambient'}
    if registry:
        deployment = json.loads(kubectl('get', 'deployment', account, '-n', namespace, '-o', 'json'))
        annotations = deployment['spec']['template']['metadata'].get('annotations', {})
        if 'ambient.istio.io/dns-capture' in annotations:
            manifest['metadata']['annotations'] = {
                'ambient.istio.io/dns-capture': annotations['ambient.istio.io/dns-capture'],
            }
    if token:
        manifest['spec']['volumes'] = [{
            'name': 'identity', 'projected': {'sources': [{
                'serviceAccountToken': {'path': 'token', 'audience': 'wasm-platform',
                                        'expirationSeconds': 600},
            }]},
        }]
        manifest['spec']['containers'][0]['volumeMounts'] = [
            {'name': 'identity', 'mountPath': '/identity', 'readOnly': True},
        ]
    try:
        kubectl('create', '-f', '-', manifest=manifest)
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            pod = json.loads(kubectl('get', 'pod', name, '-n', namespace, '-o', 'json'))
            phase = pod.get('status', {}).get('phase')
            if phase in ('Succeeded', 'Failed'):
                log = kubectl('logs', name, '-n', namespace)
                print(f'{namespace}/{account} -> {endpoint}: {log.strip()}', flush=True)
                if phase != 'Succeeded':
                    raise RuntimeError('transport authorization regression failed')
                return
            time.sleep(1)
        raise TimeoutError('transport probe did not finish within 120 seconds')
    finally:
        kubectl('delete', 'pod', name, '-n', namespace, '--ignore-not-found', '--wait=false')


def main():
    namespace = 'identity-plaintext-' + uuid.uuid4().hex[:8]
    cache = 'http://module-cache.wasm-platform.svc:80/modules/sha256:' + '0' * 64
    operator = 'http://wp-operator-grpc.wasm-platform.svc:50051/'
    kubectl('create', '-f', '-', manifest={
        'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace},
    })
    try:
        registry = 'http://wasm-platform-registry.localhost:5000/v2/'
        for account in ['wp-operator', 'execution-host']:
            check('wasm-platform', account, registry, True, expected='200', registry=True)
        check('wasm-platform', 'execution-host', cache, True, token=True)
        check('wasm-platform', 'execution-host', operator, True)
        check('wasm-platform', 'gateway', operator, True)
        for account in ['default', 'gateway']:
            check('wasm-platform', account, cache, False)
        check('wasm-platform', 'default', operator, False)
        check(namespace, 'default', cache, False)
        check(namespace, 'default', operator, False)
    finally:
        kubectl('delete', 'namespace', namespace, '--ignore-not-found', '--wait=false')


if __name__ == '__main__':
    main()

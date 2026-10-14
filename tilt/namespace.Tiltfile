def k8s_namespace(name, labels={}):
    k8s_yaml(encode_yaml({
        'apiVersion': 'v1',
        'kind': 'Namespace',
        'metadata': {
            'name': name,
            'labels': labels,
        },
    }))

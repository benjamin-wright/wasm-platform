{{/*
Label set for wp-operator resources.
*/}}
{{- define "wasm-platform.wp-operator.labels" -}}
app.kubernetes.io/name: wp-operator
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: wasm-platform
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "wasm-platform.identity.mount" -}}
- name: workload-identity
  mountPath: /var/run/secrets/wasm-platform
  readOnly: true
{{- end }}

{{- define "wasm-platform.identity.volume" -}}
- name: workload-identity
  projected:
    sources:
      - serviceAccountToken:
          audience: wasm-platform
          expirationSeconds: 600
          path: token
{{- end }}

{{- define "wasm-platform.artifact-dns.annotations" -}}
{{- if .Values.artifacts.localPlaintextRegistries }}
annotations:
  ambient.istio.io/dns-capture: "false"
{{- end }}
{{- end }}

{{/*
Label set for execution-host resources.
*/}}
{{- define "wasm-platform.execution-host.labels" -}}
app.kubernetes.io/name: execution-host
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: wasm-platform
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Label set for gateway resources.
*/}}
{{- define "wasm-platform.gateway.labels" -}}
app.kubernetes.io/name: gateway
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: wasm-platform
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Label set for module-cache resources.
*/}}
{{- define "wasm-platform.module-cache.labels" -}}
app.kubernetes.io/name: module-cache
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: wasm-platform
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

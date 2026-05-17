{{/*
Label set for wp-operator resources.
*/}}
{{- define "wasm-platform.wp-operator.labels" -}}
app.kubernetes.io/name: wp-operator
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: wasm-platform
app.kubernetes.io/managed-by: {{ .Release.Service }}
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

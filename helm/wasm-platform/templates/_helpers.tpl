{{/*
Standard label set for all wasm-platform resources.
*/}}
{{- define "wasm-platform.labels" -}}
app.kubernetes.io/name: wasm-platform
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Standard label set for all module-cache resources.
*/}}
{{- define "module-cache.labels" -}}
app.kubernetes.io/name: module-cache
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

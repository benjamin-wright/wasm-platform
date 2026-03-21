{{/*
Standard label set for all execution-host resources.
*/}}
{{- define "execution-host.labels" -}}
app.kubernetes.io/name: execution-host
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

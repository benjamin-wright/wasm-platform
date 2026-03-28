{{/*
Standard label set for all wp-databases resources.
*/}}
{{- define "wp-databases.labels" -}}
app.kubernetes.io/name: wp-databases
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

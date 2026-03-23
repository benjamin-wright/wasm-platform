{{/*
Standard label set for all wp-operator resources.
*/}}
{{- define "wp-operator.labels" -}}
app.kubernetes.io/name: wp-operator
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

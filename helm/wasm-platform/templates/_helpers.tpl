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

{{/*
Label set for databases resources (postgres, redis, nats CRs).
*/}}
{{- define "wasm-platform.databases.labels" -}}
app.kubernetes.io/name: databases
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: wasm-platform
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
The name of the PostgresDatabase CR created by this release.
Falls back to <release-name>-postgres when the value is not explicitly overridden.
*/}}
{{- define "wasm-platform.postgresDatabaseName" -}}
{{- .Values.wpOperator.databases.postgresDatabaseName | default (printf "%s-postgres" .Release.Name) -}}
{{- end }}

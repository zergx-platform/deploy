{{- define "zergx.name" -}}
zergx
{{- end -}}

{{- /* fullname: service/deployment name == the service key (no prefix) */ -}}
{{- define "zergx.fullname" -}}
{{- .name -}}
{{- end -}}

{{- define "zergx.svcUrl" -}}
{{- printf "http://%s.%s.svc.cluster.local:80" .name .Release.Namespace -}}
{{- end -}}

{{- define "zergx.labels" -}}
app.kubernetes.io/name: {{ $.Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: {{ .name }}
{{- end -}}

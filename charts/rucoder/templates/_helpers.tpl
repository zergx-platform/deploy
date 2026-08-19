{{- define "rucoder.name" -}}
{{- $.Chart.Name -}}
{{- end -}}

{{- define "rucoder.fullname" -}}
{{- printf "%s-%s" $.Chart.Name .name -}}
{{- end -}}

{{- define "rucoder.svcUrl" -}}
{{- printf "http://%s-%s.%s.svc.cluster.local:80" $.Chart.Name .name .Release.Namespace -}}
{{- end -}}

{{- define "rucoder.labels" -}}
app.kubernetes.io/name: {{ include "rucoder.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: {{ .name }}
{{- end -}}

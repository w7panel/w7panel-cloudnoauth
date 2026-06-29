{{- define "w7panel-appid-proxy.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "w7panel-appid-proxy.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "w7panel-appid-proxy.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "w7panel-appid-proxy.labels" -}}
helm.sh/chart: {{ include "w7panel-appid-proxy.chart" . }}
{{ include "w7panel-appid-proxy.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "w7panel-appid-proxy.selectorLabels" -}}
app.kubernetes.io/name: {{ include "w7panel-appid-proxy.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "w7panel-appid-proxy.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "w7panel-appid-proxy.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "w7panel-appid-proxy.serviceFQDN" -}}
{{- printf "%s.%s.svc.cluster.local" (include "w7panel-appid-proxy.fullname" .) .Release.Namespace -}}
{{- end -}}

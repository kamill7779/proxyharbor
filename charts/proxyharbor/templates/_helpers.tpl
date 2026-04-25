{{/* 通用 helpers */}}
{{- define "proxyharbor.fullname" -}}
{{- printf "%s-proxyharbor" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "proxyharbor.labels" -}}
app.kubernetes.io/name: proxyharbor
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "proxyharbor.selectorLabels" -}}
app.kubernetes.io/name: proxyharbor
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "proxyharbor.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

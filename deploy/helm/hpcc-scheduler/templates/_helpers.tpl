{{/*
Expand the chart name.
*/}}
{{- define "hpcc-scheduler.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully-qualified app name: <release>-<chart>, truncated to 63 chars.
*/}}
{{- define "hpcc-scheduler.fullname" -}}
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

{{- define "hpcc-scheduler.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "hpcc-scheduler.labels" -}}
helm.sh/chart: {{ include "hpcc-scheduler.chart" . }}
{{ include "hpcc-scheduler.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "hpcc-scheduler.selectorLabels" -}}
app.kubernetes.io/name: {{ include "hpcc-scheduler.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "hpcc-scheduler.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "hpcc-scheduler.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Image reference. tag defaults to Chart.AppVersion.
*/}}
{{- define "hpcc-scheduler.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/*
Name of the Secret that backs TLS material — either the user-supplied
existingSecret, or the chart-managed one.
*/}}
{{- define "hpcc-scheduler.tlsSecretName" -}}
{{- if .Values.tls.existingSecret -}}
{{- .Values.tls.existingSecret -}}
{{- else -}}
{{- printf "%s-tls" (include "hpcc-scheduler.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
Name + key for the worker_token Secret.
*/}}
{{- define "hpcc-scheduler.authSecretName" -}}
{{- if .Values.auth.existingSecret -}}
{{- .Values.auth.existingSecret -}}
{{- else -}}
{{- printf "%s-auth" (include "hpcc-scheduler.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "hpcc-scheduler.authSecretKey" -}}
{{- if .Values.auth.existingSecret -}}
{{- .Values.auth.existingSecretKey -}}
{{- else -}}
WORKER_TOKEN
{{- end -}}
{{- end -}}

{{/*
ConfigMap name backing scheduler.toml.
*/}}
{{- define "hpcc-scheduler.configMapName" -}}
{{- if .Values.config.existingConfigMap -}}
{{- .Values.config.existingConfigMap -}}
{{- else -}}
{{- printf "%s-config" (include "hpcc-scheduler.fullname" .) -}}
{{- end -}}
{{- end -}}

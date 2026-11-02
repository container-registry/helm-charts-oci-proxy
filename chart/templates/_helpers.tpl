{{/*
Expand the name of the chart.
*/}}
{{- define "ocip.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "ocip.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "ocip.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "ocip.labels" -}}
helm.sh/chart: {{ include "ocip.chart" . }}
{{ include "ocip.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- if .Values.customLabels }}
{{ toYaml .Values.customLabels }}
{{- end }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "ocip.selectorLabels" -}}
app.kubernetes.io/name: {{ include "ocip.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "ocip.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "ocip.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Fail rendering on settings the proxy refuses at startup.
*/}}
{{- define "ocip.validate" -}}
{{- $env := .Values.app.env_vars }}
{{- if has (lower (toString $env.REWRITE_DEPENDENCIES)) (list "1" "t" "true") }}
{{- if not (trim (toString (default "" $env.PROXY_HOST))) }}
{{- fail "app.env_vars.PROXY_HOST is required when app.env_vars.REWRITE_DEPENDENCIES is true" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
terminationGracePeriodSeconds: preStop sleep + SHUTDOWN_TIMEOUT + a margin,
unless set explicitly. An empty SHUTDOWN_TIMEOUT selects the proxy default 25.
*/}}
{{- define "ocip.terminationGracePeriodSeconds" -}}
{{- $preStop := int .Values.shutdown.preStopSleepSeconds }}
{{- if lt $preStop 0 }}
{{- fail (printf "shutdown.preStopSleepSeconds must not be negative, got %d" $preStop) }}
{{- end }}
{{- $drain := int (default 25 .Values.app.env_vars.SHUTDOWN_TIMEOUT) }}
{{- $needed := add $preStop $drain }}
{{- with .Values.shutdown.terminationGracePeriodSeconds }}
{{- if lt (int .) $needed }}
{{- fail (printf "shutdown.terminationGracePeriodSeconds (%d) must be at least shutdown.preStopSleepSeconds + app.env_vars.SHUTDOWN_TIMEOUT (%d)" (int .) $needed) }}
{{- end }}
{{- int . }}
{{- else }}
{{- add $needed 5 }}
{{- end }}
{{- end }}

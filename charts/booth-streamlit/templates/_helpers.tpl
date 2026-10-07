{{/*
Standard name/label helpers, the same shape `helm create` scaffolds; mirrors booth-catalog's and
booth-storage's own chart helpers, nothing booth-streamlit-specific here.
*/}}

{{- define "booth-streamlit.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "booth-streamlit.fullname" -}}
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

{{- define "booth-streamlit.labels" -}}
app.kubernetes.io/name: {{ include "booth-streamlit.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
booth.projectbooth.io/module: streamlit
{{- end -}}

{{- define "booth-streamlit.selectorLabels" -}}
app.kubernetes.io/name: {{ include "booth-streamlit.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "booth-streamlit.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "booth-streamlit.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* The Secret holding the database connection string (see values.yaml `postgres`). */}}
{{- define "booth-streamlit.dsnSecretName" -}}
{{- if .Values.postgres.provisionedByCore -}}
{{- default "booth-database-credentials" .Values.postgres.dsnSecret.name -}}
{{- else -}}
{{- required "postgres.dsnSecret.name is required when postgres.provisionedByCore is false: name the Secret holding your own database's connection string" .Values.postgres.dsnSecret.name -}}
{{- end -}}
{{- end -}}

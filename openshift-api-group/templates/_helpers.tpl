{{ define "apiVersion" }}
{{- if .Capabilities.APIVersions.Has "openshift.org/v1" }}openshift.org/v1{{ else }}v1{{ end }}
{{- end }}

{{- define "openshift-api-group.labels" -}}
labels:
  chart: {{ .Chart.Name }}-{{ .Chart.Version }}
  release: {{ .Release.Name }}
{{- end -}}

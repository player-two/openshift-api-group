{{ define "apiVersion" }}
{{- if .Capabilities.APIVersions.Has "openshift.org/v1" }}openshift.org/v1{{ else }}v1{{ end }}
{{- end }}

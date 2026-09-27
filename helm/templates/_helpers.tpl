{{- define "oc-substrate.operatorLabels" -}}
app.kubernetes.io/name: oc-substrate-operator
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

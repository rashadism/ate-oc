{{- define "oc-substrate.operatorLabels" -}}
app.kubernetes.io/name: oc-substrate-operator
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "oc-substrate.frontdoorLabels" -}}
app.kubernetes.io/name: oc-substrate-frontdoor
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "prometheus-universal-exporter.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- define "prometheus-universal-exporter.fullname" -}}
{{- if .Values.fullnameOverride }}{{ .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}{{ else }}{{ include "prometheus-universal-exporter.name" . }}{{ end }}
{{- end }}
{{- define "prometheus-universal-exporter.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "prometheus-universal-exporter.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}
{{- define "prometheus-universal-exporter.metadataLabels" -}}
{{- $root := index . 0 -}}
{{- $labels := dict -}}
{{- range $key, $value := $root.Values.defaultLabels }}{{- $_ := set $labels $key $value -}}{{- end -}}
{{- if gt (len .) 1 }}{{- range $key, $value := index . 1 }}{{- $_ := set $labels $key $value -}}{{- end -}}{{- end -}}
{{- range $key, $value := (include "prometheus-universal-exporter.labels" $root | fromYaml) }}{{- $_ := set $labels $key $value -}}{{- end -}}
{{- toYaml $labels -}}
{{- end }}
{{- define "prometheus-universal-exporter.metadataAnnotations" -}}
{{- $root := index . 0 -}}
{{- $annotations := dict -}}
{{- range $key, $value := $root.Values.defaultAnnotations }}{{- $_ := set $annotations $key $value -}}{{- end -}}
{{- if gt (len .) 1 }}{{- range $key, $value := index . 1 }}{{- $_ := set $annotations $key $value -}}{{- end -}}{{- end -}}
{{- toYaml $annotations -}}
{{- end }}
{{- define "prometheus-universal-exporter.listenAddress" -}}
{{- default ":8080" .Values.server.listenAddress -}}
{{- end }}
{{- define "prometheus-universal-exporter.containerPort" -}}
{{- $address := include "prometheus-universal-exporter.listenAddress" . -}}
{{- $port := $address | splitList ":" | last | int -}}
{{- if or (lt $port 1) (gt $port 65535) -}}
{{- fail (printf "server.listenAddress %q must end in a valid TCP port, for example \":8080\"" $address) -}}
{{- end -}}
{{- $port -}}
{{- end }}
{{- define "prometheus-universal-exporter.pythonPath" -}}
{{- default "/usr/local/bin/python3" .Values.server.pythonPath -}}
{{- end }}
{{- define "prometheus-universal-exporter.targetsFile" -}}
{{- default "targets.yaml" .Values.otlpTargets.fileName -}}
{{- end }}
{{- define "prometheus-universal-exporter.validateTargets" -}}
{{- if .Values.otlpTargets.enabled -}}
{{- if .Values.config.enabled -}}
{{- if not (trim (.Values.otlpTargets.data | default "")) -}}
{{- fail "otlpTargets.enabled requires otlpTargets.data to hold the scheduled target document" -}}
{{- end -}}
{{- $config := fromYaml (index .Values.config.data "config.yaml" | default "") -}}
{{- if not (dig "otlp" "enabled" false $config) -}}
{{- fail "otlpTargets.enabled requires otlp.enabled: true in config.data.config.yaml; the exporter refuses to start with scheduled targets while OTLP export is disabled" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}{{ default (include "prometheus-universal-exporter.fullname" .) .Values.serviceAccount.name }}{{ else }}{{ default "default" .Values.serviceAccount.name }}{{ end }}
{{- end }}

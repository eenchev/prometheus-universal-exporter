{{- define "prometheus-universal-exporter.extraArgs" -}}
{{- /* The chart already renders the flags below from named values. Go's flag
       package keeps the last occurrence, so an extraArgs entry repeating one of
       them would win silently — and for --web.listen-address the container port
       and the probes would still follow server.listenAddress, leaving a pod
       that listens on one port while Kubernetes checks another. Rejecting the
       collision while rendering costs nothing; debugging it costs an
       afternoon. */ -}}
{{- $managed := dict
  "--config.file" "the chart renders the configuration itself, so change it through config.data, or set config.enabled to false and supply the ConfigMap yourself"
  "--web.listen-address" "set server.listenAddress instead"
  "--web.self-metrics-path" "set selfMetrics.path instead"
  "--python.path" "set server.pythonPath instead"
  "--config.watch" "set server.watchConfig instead"
  "--config.watch-interval" "set server.watchConfigInterval instead"
  "--otlp.targets-file" "set otlpTargets.enabled instead"
  "--config.export-env" "set server.expandEnv instead" -}}
{{- range $arg := .Values.extraArgs -}}
{{- $text := $arg | toString -}}
{{- if not (hasPrefix "--" $text) -}}
{{- fail (printf "extraArgs entry %q must start with `--`, for example \"--log.level=debug\"" $text) -}}
{{- end -}}
{{- $name := $text | splitList "=" | first -}}
{{- if eq $name "--dry-run" -}}
{{- fail (printf "extraArgs entry %q would make the exporter validate its configuration and exit, so the pod would never serve; run --dry-run as a separate command, a Job or an init container instead" $text) -}}
{{- end -}}
{{- if hasKey $managed $name -}}
{{- fail (printf "extraArgs entry %q sets %s, which the chart already manages; %s" $text $name (get $managed $name)) -}}
{{- end -}}
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.validateExtraMounts" -}}
{{- /* A mount at the configuration directory replaces it, so the exporter
       starts with no config.yaml and crash-loops with an error that points at
       the file rather than at the mount that hid it. */ -}}
{{- $reserved := list "/etc/prometheus-universal-exporter" -}}
{{- if and .Values.targetAuth .Values.targetAuth.enabled -}}
{{- $reserved = append $reserved (.Values.targetAuth.mountPath | toString) -}}
{{- end -}}
{{- range $mount := .Values.extraVolumeMounts -}}
{{- $path := $mount.mountPath | toString | trimSuffix "/" -}}
{{- if has $path $reserved -}}
{{- fail (printf "extraVolumeMounts uses mountPath %q, which the chart already mounts; choose another path" $mount.mountPath) -}}
{{- end -}}
{{- end -}}
{{- end }}
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
{{- $address := default ":8080" .Values.server.listenAddress -}}
{{- /* Go's net.Listen wants host:port. A bare port such as "9115" renders
       perfectly well here and then makes the container exit at once with
       "listen tcp: address 9115: missing port in address", so it is rejected
       while the chart is still being rendered. The host may be empty, a name or
       IPv4 address, or a bracketed IPv6 address. */ -}}
{{- if not (regexMatch "^([^:]*|\\[[0-9A-Fa-f:.]+\\]):[0-9]+$" $address) -}}
{{- fail (printf "server.listenAddress %q must be host:port with the port after a colon, for example \":8080\", \"0.0.0.0:8080\" or \"[::1]:8080\"" $address) -}}
{{- end -}}
{{- $port := $address | splitList ":" | last | int -}}
{{- if or (lt $port 1) (gt $port 65535) -}}
{{- fail (printf "server.listenAddress %q must end in a TCP port between 1 and 65535" $address) -}}
{{- end -}}
{{- $address -}}
{{- end }}
{{- define "prometheus-universal-exporter.containerPort" -}}
{{- /* listenAddress has already validated the shape and the range. */ -}}
{{- include "prometheus-universal-exporter.listenAddress" . | splitList ":" | last | int -}}
{{- end }}
{{- define "prometheus-universal-exporter.watchConfigInterval" -}}
{{- $interval := default "60s" .Values.server.watchConfigInterval -}}
{{- if not (regexMatch "^[0-9]+(\\.[0-9]+)?(ns|us|ms|s|m|h)$" $interval) -}}
{{- fail (printf "server.watchConfigInterval %q must be a positive Go duration, for example \"60s\"" $interval) -}}
{{- end -}}
{{- $interval -}}
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
{{- $raw := index .Values.config.data "config.yaml" | default "" -}}
{{- if not (trim $raw) -}}
{{- fail "config.data must contain a config.yaml entry, which is the file the exporter reads. When supplying it with --set-file, quote the whole argument so the escaped dot reaches helm instead of being consumed by the shell." -}}
{{- end -}}
{{- $config := fromYaml $raw -}}
{{- if not (dig "otlp" "enabled" false $config) -}}
{{- fail "otlpTargets.enabled requires otlp.enabled: true in config.data.config.yaml; the exporter refuses to start with scheduled targets while OTLP export is disabled" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}{{ default (include "prometheus-universal-exporter.fullname" .) .Values.serviceAccount.name }}{{ else }}{{ default "default" .Values.serviceAccount.name }}{{ end }}
{{- end }}

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
  "--static-targets-file" "set staticTargets.enabled and staticTargets.data instead"
  "--web.static-targets-path" "set staticTargets.path instead"
  "--config.expand-env" "set server.expandEnv instead"
  "--static-targets.expand-env" "set staticTargets.expandEnv instead"
  "--log.level" "set server.logLevel instead"
  "--probe.timeout-offset" "set server.probeTimeoutOffset instead"
  "--probe.default-timeout" "set server.probeDefaultTimeout instead"
  "--probe.max-concurrent" "set server.probeMaxConcurrent instead"
  "--python.max-workers" "set server.pythonMaxWorkers instead"
  "--web.enable-lifecycle" "set server.enableLifecycle instead"
  "--web.shutdown-timeout" "set server.shutdownTimeout instead"
  "--web.shutdown-delay" "set server.shutdownDelay instead" -}}
{{- /* These flags make the exporter print something and exit instead of
       serving, so a pod started with one would restart for ever. */ -}}
{{- $oneShot := dict
  "--dry-run" "would make the exporter validate its configuration and exit, so the pod would never serve; run --dry-run as a separate command, a Job or an init container instead"
  "--config.schema" "would make the exporter print the configuration schema and exit, so the pod would never serve; run it as a separate command instead"
  "--config.collector-file-schema" "would make the exporter print the collector file schema and exit, so the pod would never serve; run it as a separate command instead"
  "--static-targets-file-schema" "would make the exporter print the static target file schema and exit, so the pod would never serve; run it as a separate command instead"
  "--version" "would make the exporter print its version and exit, so the pod would never serve; the version is in the http_exporter_build_info self-metric"
  "--help" "would make the exporter print its usage and exit, so the pod would never serve"
  "--h" "would make the exporter print its usage and exit, so the pod would never serve" -}}
{{- range $arg := .Values.extraArgs -}}
{{- $text := $arg | toString -}}
{{- if not (hasPrefix "--" $text) -}}
{{- fail (printf "extraArgs entry %q must start with `--`, for example \"--some.new-flag=value\"" $text) -}}
{{- end -}}
{{- $name := $text | splitList "=" | first -}}
{{- if hasKey $oneShot $name -}}
{{- fail (printf "extraArgs entry %q %s" $text (get $oneShot $name)) -}}
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
{{- if and .Values.webAuth .Values.webAuth.enabled -}}
{{- $reserved = append $reserved (.Values.webAuth.mountPath | toString | trimSuffix "/") -}}
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
{{- define "prometheus-universal-exporter.logLevel" -}}
{{- $level := default "info" .Values.server.logLevel -}}
{{- if not (has $level (list "debug" "info" "warn" "error")) -}}
{{- fail (printf "server.logLevel %q must be debug, info, warn or error" $level) -}}
{{- end -}}
{{- $level -}}
{{- end }}
{{- define "prometheus-universal-exporter.probeTimeoutOffset" -}}
{{- /* Empty leaves the flag out: the exporter's default applies, and an image
       from before the flag existed still starts. */ -}}
{{- $offset := .Values.server.probeTimeoutOffset | default "" | toString -}}
{{- if $offset -}}
{{- if not (regexMatch "^(0|([0-9]+(\\.[0-9]+)?(ns|us|ms|s|m|h))+)$" $offset) -}}
{{- fail (printf "server.probeTimeoutOffset %q must be a Go duration of zero or more, for example \"500ms\" or \"1s\"" $offset) -}}
{{- end -}}
{{- $offset -}}
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.probeDefaultTimeout" -}}
{{- /* Empty leaves the flag out, as for probeTimeoutOffset. */ -}}
{{- $timeout := .Values.server.probeDefaultTimeout | default "" | toString -}}
{{- if $timeout -}}
{{- if not (regexMatch "^(0|([0-9]+(\\.[0-9]+)?(ns|us|ms|s|m|h))+)$" $timeout) -}}
{{- fail (printf "server.probeDefaultTimeout %q must be a Go duration of zero or more, for example \"30s\" or \"1m\"; 0 leaves a probe without a deadline unbounded" $timeout) -}}
{{- end -}}
{{- $timeout -}}
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.countFlag" -}}
{{- /* A count flag's value, from (list name value): empty or null leaves the
       flag out; otherwise a whole number of zero or more. */ -}}
{{- $name := index . 0 -}}
{{- $value := index . 1 -}}
{{- if not (or (kindIs "invalid" $value) (eq (toString $value) "")) -}}
{{- $text := toString $value -}}
{{- if not (regexMatch "^[0-9]+$" $text) -}}
{{- fail (printf "%s %q must be a whole number of zero or more, 0 for no limit, or empty to keep the exporter's default" $name $text) -}}
{{- end -}}
{{- $text -}}
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.shutdownTimeout" -}}
{{- /* Empty leaves the flag out, as for probeTimeoutOffset. Only whole hours,
       minutes and seconds, so the chart can work out the grace period. */ -}}
{{- $timeout := .Values.server.shutdownTimeout | default "" | toString -}}
{{- if $timeout -}}
{{- if not (regexMatch "^([0-9]+h)?([0-9]+m)?([0-9]+s)?$" $timeout) -}}
{{- fail (printf "server.shutdownTimeout %q must be whole hours, minutes and seconds, for example \"30s\" or \"1m30s\"" $timeout) -}}
{{- end -}}
{{- if eq (include "prometheus-universal-exporter.durationSeconds" $timeout) "0" -}}
{{- fail (printf "server.shutdownTimeout %q must be positive" $timeout) -}}
{{- end -}}
{{- $timeout -}}
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.shutdownDelay" -}}
{{- /* Empty leaves the flag out; 0s is rendered and turns the delay off. Only
       whole hours, minutes and seconds, so the chart can work out the grace
       period. */ -}}
{{- $delay := .Values.server.shutdownDelay | default "" | toString -}}
{{- if $delay -}}
{{- if not (regexMatch "^([0-9]+h)?([0-9]+m)?([0-9]+s)?$" $delay) -}}
{{- fail (printf "server.shutdownDelay %q must be whole hours, minutes and seconds, for example \"5s\" or \"0s\"" $delay) -}}
{{- end -}}
{{- $delay -}}
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.durationSeconds" -}}
{{- $seconds := 0 -}}
{{- range $part := regexFindAll "[0-9]+[hms]" . -1 -}}
{{- $n := $part | trimSuffix "h" | trimSuffix "m" | trimSuffix "s" | atoi -}}
{{- if hasSuffix "h" $part -}}{{- $seconds = add $seconds (mul $n 3600) -}}
{{- else if hasSuffix "m" $part -}}{{- $seconds = add $seconds (mul $n 60) -}}
{{- else -}}{{- $seconds = add $seconds $n -}}{{- end -}}
{{- end -}}
{{- $seconds -}}
{{- end }}
{{- define "prometheus-universal-exporter.terminationGracePeriodSeconds" -}}
{{- /* A stopping pod needs the shutdown delay, the shutdown timeout, then
       time for the last OTLP export and exit. Kubernetes kills it after 30
       seconds unless told otherwise, which would cut them short. */ -}}
{{- $delay := include "prometheus-universal-exporter.shutdownDelay" . | default "0s" -}}
{{- $shutdown := include "prometheus-universal-exporter.shutdownTimeout" . | default "15s" -}}
{{- $needed := add (include "prometheus-universal-exporter.durationSeconds" $delay | atoi) (include "prometheus-universal-exporter.durationSeconds" $shutdown | atoi) 10 -}}
{{- $explicit := .Values.terminationGracePeriodSeconds -}}
{{- if not (kindIs "invalid" $explicit) -}}
{{- if lt (int $explicit) (int $needed) -}}
{{- fail (printf "terminationGracePeriodSeconds %v is shorter than the %d seconds a stopping pod needs: server.shutdownDelay (%s), server.shutdownTimeout (%s) and 10 seconds for the last OTLP export and exit; raise it or lower server.shutdownDelay or server.shutdownTimeout" $explicit $needed $delay $shutdown) -}}
{{- end -}}
{{- int $explicit -}}
{{- else if gt (int $needed) 30 -}}
{{- $needed -}}
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.pythonPath" -}}
{{- default "/usr/local/bin/python3" .Values.server.pythonPath -}}
{{- end }}
{{- define "prometheus-universal-exporter.staticTargetsFile" -}}
{{- default "static-targets.yaml" .Values.staticTargets.fileName -}}
{{- end }}
{{- define "prometheus-universal-exporter.staticTargetsPath" -}}
{{- $path := default "/static-targets" .Values.staticTargets.path -}}
{{- if eq $path .Values.selfMetrics.path -}}
{{- fail (printf "staticTargets.path %q is also selfMetrics.path; the two endpoints need paths of their own" $path) -}}
{{- end -}}
{{- $path -}}
{{- end }}
{{- define "prometheus-universal-exporter.validateTargets" -}}
{{- if .Values.staticTargets.enabled -}}
{{- /* The file is a key of the configuration ConfigMap, so its name must be
       a ConfigMap key, and not one config.data already uses, which it would
       collide with or silently replace. */ -}}
{{- $fileName := include "prometheus-universal-exporter.staticTargetsFile" . -}}
{{- if or (not (regexMatch "^[-._a-zA-Z0-9]+$" $fileName)) (eq $fileName ".") (eq $fileName "..") -}}
{{- fail (printf "staticTargets.fileName %q must be a ConfigMap key: letters, digits, -, _ and ., with no /" $fileName) -}}
{{- end -}}
{{- if and .Values.config.enabled (hasKey (.Values.config.data | default dict) $fileName) -}}
{{- fail (printf "staticTargets.fileName %q is also a key of config.data; the static target file needs a name of its own in the ConfigMap" $fileName) -}}
{{- end -}}
{{- $data := trim (.Values.staticTargets.data | default "") -}}
{{- if .Values.config.enabled -}}
{{- if not $data -}}
{{- fail "staticTargets.enabled requires staticTargets.data to hold the static target document" -}}
{{- end -}}
{{- else if $data -}}
{{- /* Without config.enabled the chart renders no ConfigMap, so the data
       would be dropped without a word, and the exporter would read whatever
       the ConfigMap supplied instead has under the file name. */ -}}
{{- fail (printf "staticTargets.data is rendered into the chart's ConfigMap, which config.enabled: false leaves out; put the static target file into your ConfigMap %s under the key %s, and leave staticTargets.data empty" (include "prometheus-universal-exporter.fullname" .) $fileName) -}}
{{- end -}}
{{- /* The monitor may read only some targets. A name the document does not
       have would make every scrape of the endpoint fail with 400, so
       rendering fails first. A target without a name is called
       <collector>_<index>, as the exporter calls it. */ -}}
{{- $wanted := .Values.staticTargets.monitor.targets | default list -}}
{{- if and .Values.staticTargets.monitor.enabled $wanted $data -}}
{{- $names := dict -}}
{{- range $index, $target := (get (fromYaml .Values.staticTargets.data) "targets" | default list) -}}
{{- if kindIs "map" $target -}}
{{- $_ := set $names (get $target "name" | default (printf "%v_%d" (get $target "collector") $index) | toString) true -}}
{{- end -}}
{{- end -}}
{{- range $wanted -}}
{{- if not (hasKey $names (toString .)) -}}
{{- fail (printf "staticTargets.monitor.targets names %q, but staticTargets.data has no target of that name; the endpoint would answer every scrape with 400" (toString .)) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if .Values.config.enabled -}}
{{- $raw := index .Values.config.data "config.yaml" | default "" -}}
{{- if not (trim $raw) -}}
{{- fail "config.data must contain a config.yaml entry, which is the file the exporter reads. When supplying it with --set-file, quote the whole argument so the escaped dot reaches helm instead of being consumed by the shell." -}}
{{- end -}}
{{- $config := fromYaml $raw -}}
{{- $targets := fromYaml .Values.staticTargets.data -}}
{{- /* A target exported over OTLP needs OTLP export; the exporter refuses to
       start without it, so rendering fails first. */ -}}
{{- if not (dig "otlp" "enabled" false $config) -}}
{{- range (get $targets "targets" | default list) -}}
{{- if and (kindIs "map" .) (get . "export_via_otlp") -}}
{{- fail (printf "static target %v sets export_via_otlp, which needs otlp.enabled: true in config.data.config.yaml; the exporter refuses to start without it" (get . "name" | default "(unnamed)")) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}{{ default (include "prometheus-universal-exporter.fullname" .) .Values.serviceAccount.name }}{{ else }}{{ default "default" .Values.serviceAccount.name }}{{ end }}
{{- end }}

{{- define "prometheus-universal-exporter.envSets" -}}
{{- /* "true" when the env list, (list env name), sets the variable name. */ -}}
{{- $name := index . 1 -}}
{{- range (index . 0 | default list) -}}
{{- if eq (toString .name) $name }}true{{ end -}}
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.validateProbe" -}}
{{- /* A probe's settings must not replace the check itself: the chart owns
       the path and port. (list name probe) */ -}}
{{- $name := index . 0 -}}
{{- range $key := list "httpGet" "exec" "tcpSocket" "grpc" -}}
{{- if hasKey (index $ 1 | default dict) $key -}}
{{- fail (printf "%s.%s is set by the chart, which checks /health and /ready on the http port; set only timings such as timeoutSeconds and failureThreshold" $name $key) -}}
{{- end -}}
{{- end -}}
{{- end }}
